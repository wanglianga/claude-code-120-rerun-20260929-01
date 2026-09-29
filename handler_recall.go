package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---- 创建召回（DRAFT），支持按成品批次/原料批次/触发事件圈定范围 ----

func (a *App) createRecall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReasonType      string   `json:"reason_type"`
		Level           string   `json:"level"`
		TriggerEventNo  string   `json:"trigger_event_no"`
		RegulatorRefNo  string   `json:"regulator_ref_no"`
		Description     string   `json:"description"`
		ProductBatchNos []string `json:"product_batch_nos"`
		RawBatchNo      string   `json:"raw_batch_no"`
		Action          string   `json:"action"` // STOP_SALE_ONLY / STOP_SALE_RETURN / DESTROY
	}
	if err := decode(r, &req); err != nil || req.ReasonType == "" || req.Description == "" {
		writeErr(w, 400, "reason_type/description 必填")
		return
	}
	validReason := map[string]bool{
		"TEMP_EXCEEDED": true, "THAW_TIMEOUT": true, "SEASONING_MISMATCH": true,
		"METAL_ALARM": true, "PACKAGE_LEAK": true, "OFF_ODOR": true,
		"LIVESTREAM_BATCH_RETURN": true, "REGULATOR_RECALL": true, "OTHER": true,
	}
	if !validReason[req.ReasonType] {
		writeErr(w, 400, "reason_type 取值非法")
		return
	}
	level := nz(req.Level, "CLASS_II")
	if level != "CLASS_I" && level != "CLASS_II" && level != "CLASS_III" {
		writeErr(w, 400, "level 必须为 CLASS_I/CLASS_II/CLASS_III")
		return
	}
	action := nz(req.Action, "STOP_SALE_RETURN")
	if action != "STOP_SALE_ONLY" && action != "STOP_SALE_RETURN" && action != "DESTROY" {
		writeErr(w, 400, "action 非法")
		return
	}

	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()

	var triggerEvent sql.NullInt64
	var eventRaw, eventProduct sql.NullInt64
	if req.TriggerEventNo != "" {
		var estatus string
		err := tx.QueryRow(`SELECT id,COALESCE(raw_batch_id,0),COALESCE(product_batch_id,0),status FROM quality_events WHERE event_no=$1`,
			req.TriggerEventNo).Scan(&triggerEvent, &eventRaw, &eventProduct, &estatus)
		if err != nil {
			writeErr(w, 404, "触发质量事件不存在: "+req.TriggerEventNo)
			return
		}
		if estatus == "DISMISSED" {
			writeErr(w, 409, "触发事件已被驳回，不能发起召回")
			return
		}
	}

	// 圈定成品批次
	batchIDs := map[int64]bool{}
	for _, bn := range req.ProductBatchNos {
		var id int64
		if err := tx.QueryRow(`SELECT id FROM product_batches WHERE batch_no=$1`, bn).Scan(&id); err != nil {
			writeErr(w, 400, "成品批次不存在: "+bn)
			return
		}
		batchIDs[id] = true
	}
	if req.RawBatchNo != "" {
		var rawID int64
		if err := tx.QueryRow(`SELECT id FROM raw_material_batches WHERE batch_no=$1`, req.RawBatchNo).Scan(&rawID); err != nil {
			writeErr(w, 400, "原料批次不存在: "+req.RawBatchNo)
			return
		}
		rows, err := tx.Query(`SELECT product_batch_id FROM material_genealogy WHERE raw_batch_id=$1`, rawID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			batchIDs[id] = true
		}
		rows.Close()
	}
	if eventProduct.Valid && eventProduct.Int64 != 0 {
		batchIDs[eventProduct.Int64] = true
	}
	if eventRaw.Valid && eventRaw.Int64 != 0 {
		rows, _ := tx.Query(`SELECT product_batch_id FROM material_genealogy WHERE raw_batch_id=$1`, eventRaw.Int64)
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			batchIDs[id] = true
		}
		rows.Close()
	}
	if len(batchIDs) == 0 {
		writeErr(w, 400, "召回范围为空：请提供 product_batch_nos / raw_batch_no / 有效触发事件")
		return
	}

	u := currentUser(r)
	recallNo, _ := a.nextDocNo(r.Context(), "RC")
	var recallID int64
	if err := tx.QueryRow(`INSERT INTO recalls(recall_no,reason_type,level,trigger_event_id,regulator_ref_no,description,initiated_by)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		recallNo, req.ReasonType, level, triggerEvent, req.RegulatorRefNo, req.Description, u.Username).Scan(&recallID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for id := range batchIDs {
		if _, err := tx.Exec(`INSERT INTO recall_batches(recall_id,product_batch_id,action) VALUES($1,$2,$3)
			ON CONFLICT DO NOTHING`, recallID, id, action); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if triggerEvent.Valid {
		tx.Exec(`UPDATE quality_events SET recall_id=$2 WHERE id=$1`, triggerEvent.Int64, recallID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.rdb.Set(r.Context(), "recall:latest", recallNo, 0)
	writeJSON(w, 201, map[string]any{
		"recall_no": recallNo, "status": "DRAFT", "level": level,
		"affected_product_batches": len(batchIDs),
		"next":                     "发布前可 GET /recalls/" + recallNo + " 查看影响面，确认后 POST /release",
	})
}

// ---- 召回详情：精准定位渠道/库存/订单/留样 ----

func (a *App) getRecall(w http.ResponseWriter, r *http.Request) {
	recallNo := r.PathValue("recallNo")
	recall, err := a.queryOneMap(`SELECT * FROM recalls WHERE recall_no=$1`, recallNo)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "召回单不存在")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	recallID := recall["id"]

	batchRows, err := a.db.Query(`SELECT rb.action,pb.id,pb.batch_no,pb.product_code,pb.product_name,pb.status,pb.qty_kg,
		l.code AS line_code, loc.code AS cold_location
		FROM recall_batches rb
		JOIN product_batches pb ON pb.id=rb.product_batch_id
		JOIN production_lines l ON l.id=pb.line_id
		LEFT JOIN storage_locations loc ON loc.id=pb.cold_location_id
		WHERE rb.recall_id=$1 ORDER BY pb.batch_no`, recallID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	batches := scanRows(batchRows)
	batchRows.Close()

	for _, b := range batches {
		pbID := b["id"]
		// 库存箱（含冻结/在库/返工）
		boxRows, _ := a.db.Query(`SELECT box_no,weight_kg,status,reworked,COALESCE((SELECT code FROM storage_locations WHERE id=product_boxes.location_id),'') AS location_code
			FROM product_boxes WHERE batch_id=$1 ORDER BY status,box_no`, pbID)
		b["boxes"] = scanRows(boxRows)
		boxRows.Close()

		// 已发货渠道
		chRows, _ := a.db.Query(`SELECT DISTINCT c.id,c.code,c.name,c.type,
			(SELECT COUNT(*) FROM shipment_boxes sb JOIN shipments s3 ON s3.id=sb.shipment_id
			 WHERE s3.product_batch_id=$1 AND s3.channel_id=c.id) AS shipped_boxes
			FROM shipments s JOIN channels c ON c.id=s.channel_id WHERE s.product_batch_id=$1 ORDER BY c.code`, pbID)
		b["shipped_channels"] = scanRows(chRows)
		chRows.Close()

		// 已售订单（精准到订单与箱）
		ordRows, _ := a.db.Query(`SELECT o.order_no,c.code AS channel_code,c.name AS channel_name,o.customer_ref,
			o.amount_cny,o.status,o.sold_at,
			COALESCE((SELECT json_agg(json_build_object('box_no',pb2.box_no,'qty',oi.qty))
				FROM order_items oi JOIN product_boxes pb2 ON pb2.id=oi.box_id
				WHERE oi.order_id=o.id AND oi.product_batch_id=$1),'[]') AS boxes
			FROM sales_orders o JOIN channels c ON c.id=o.channel_id
			WHERE o.id IN (SELECT DISTINCT order_id FROM order_items WHERE product_batch_id=$1)
			ORDER BY o.sold_at DESC`, pbID)
		b["sold_orders"] = scanRows(ordRows)
		ordRows.Close()
		// json_agg 经驱动返回为 JSON 字符串，这里解析为真正的数组
		for _, o := range b["sold_orders"].([]map[string]any) {
			if raw, ok := o["boxes"].(string); ok {
				var arr []any
				if json.Unmarshal([]byte(raw), &arr) == nil {
					o["boxes"] = arr
				}
			}
		}

		// 留样检测结果
		sRows, _ := a.db.Query(`SELECT sample_no,qty_g,lab_result,lab_report_no,tested_at,conclusion
			FROM retained_samples WHERE product_batch_id=$1 ORDER BY id`, pbID)
		b["samples"] = scanRows(sRows)
		sRows.Close()

		// 上游原料批次
		rawRows, _ := a.db.Query(`SELECT rb.batch_no,rb.material_type,s.name AS supplier_name,s.id AS supplier_id,g.qty_consumed_kg
			FROM material_genealogy g
			JOIN raw_material_batches rb ON rb.id=g.raw_batch_id
			JOIN suppliers s ON s.id=rb.supplier_id
			WHERE g.product_batch_id=$1`, pbID)
		b["raw_materials"] = scanRows(rawRows)
		rawRows.Close()

		// 退货回厂情况
		retRows, _ := a.db.Query(`SELECT ru.final_status,ru.decision,COUNT(*) AS boxes
			FROM returned_units ru JOIN product_boxes pbx ON pbx.id=ru.box_id
			WHERE pbx.batch_id=$1 AND ru.recall_id=$2 GROUP BY ru.final_status,ru.decision`, pbID, recallID)
		b["returns"] = scanRows(retRows)
		retRows.Close()
	}

	docRows, _ := a.db.Query(`SELECT doc_type,doc_no,title,target,generated_by,generated_at FROM recall_documents WHERE recall_id=$1 ORDER BY id`, recallID)
	docs := scanRows(docRows)
	docRows.Close()

	writeJSON(w, 200, map[string]any{"recall": recall, "batches": batches, "documents": docs})
}

func (a *App) listRecalls(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT rc.id,rc.recall_no,rc.reason_type,rc.level,rc.status,rc.regulator_ref_no,
		rc.description,rc.initiated_by,rc.initiated_at,rc.released_at,
		(SELECT COUNT(*) FROM recall_batches WHERE recall_id=rc.id) AS batch_count
		FROM recalls rc WHERE ($1='' OR rc.status=$1) ORDER BY rc.id DESC`, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

// ---- 发布召回：冻结在库、锁定发货单/订单、自动生成停售通知与追溯报告 ----

func (a *App) releaseRecall(w http.ResponseWriter, r *http.Request) {
	recallNo := r.PathValue("recallNo")
	var recallID int64
	var status, reason, level, desc string
	var regulatorRef sql.NullString
	if err := a.db.QueryRow(`SELECT id,status,reason_type,level,COALESCE(regulator_ref_no,''),description FROM recalls WHERE recall_no=$1`,
		recallNo).Scan(&recallID, &status, &reason, &level, &regulatorRef, &desc); err != nil {
		writeErr(w, 404, "召回单不存在")
		return
	}
	if status != "DRAFT" {
		writeErr(w, 409, "召回单已发布/关闭，当前状态: "+status)
		return
	}
	u := currentUser(r)
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()

	rows, _ := tx.Query(`SELECT product_batch_id FROM recall_batches WHERE recall_id=$1`, recallID)
	var pbIDs []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		pbIDs = append(pbIDs, id)
	}
	rows.Close()
	if len(pbIDs) == 0 {
		writeErr(w, 409, "召回范围为空")
		return
	}

	frozenBoxes, affectedShipments, affectedOrders := 0, 0, 0
	channelSet := map[int64]string{}
	for _, pbID := range pbIDs {
		// 在库箱（含返工合格再入库）全部冻结
		res, err := tx.Exec(`UPDATE product_boxes SET status='RECALL_HELD', updated_at=now()
			WHERE batch_id=$1 AND status IN ('IN_STOCK','REWORKED')`, pbID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		n, _ := res.RowsAffected()
		frozenBoxes += int(n)
		tx.Exec(`UPDATE product_batches SET status='RECALL_HELD' WHERE id=$1 AND status NOT IN ('RECALLED')`, pbID)

		// 已发货未退回的发货单标记召回
		srows, _ := tx.Query(`SELECT DISTINCT s.id,s.channel_id FROM shipments s
			JOIN shipment_boxes sb ON sb.shipment_id=s.id
			JOIN product_boxes b ON b.id=sb.box_id
			WHERE b.batch_id=$1 AND s.status IN ('SHIPPED','PARTIAL_RETURN')`, pbID)
		type ship struct{ id, ch int64 }
		var ships []ship
		for srows.Next() {
			var x ship
			srows.Scan(&x.id, &x.ch)
			ships = append(ships, x)
		}
		srows.Close()
		for _, s := range ships {
			tx.Exec(`UPDATE shipments SET status='RECALLED' WHERE id=$1`, s.id)
			channelSet[s.ch] = ""
			affectedShipments++
		}

		// 已售订单标记召回（精准到订单，供退货与赔付）
		orows, _ := tx.Query(`SELECT DISTINCT o.id,c.id FROM order_items oi
			JOIN sales_orders o ON o.id=oi.order_id
			JOIN channels c ON c.id=o.channel_id
			WHERE oi.product_batch_id=$1 AND o.status IN ('SOLD','RETURNED')`, pbID)
		type ord struct{ id, ch int64 }
		var ords []ord
		for orows.Next() {
			var x ord
			orows.Scan(&x.id, &x.ch)
			ords = append(ords, x)
		}
		orows.Close()
		for _, x := range ords {
			tx.Exec(`UPDATE sales_orders SET status='RECALLED' WHERE id=$1`, x.id)
			channelSet[x.ch] = ""
			affectedOrders++
		}
	}

	if _, err := tx.Exec(`UPDATE recalls SET status='RELEASED',released_at=now(),released_by=$2 WHERE id=$1`, recallID, u.Username); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tx.Exec(`UPDATE quality_events SET status='CLOSED' WHERE id=(SELECT trigger_event_id FROM recalls WHERE id=$1) AND status='CONFIRMED'`, recallID)

	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// 生成材料（独立事务，失败不影响发布结果）
	docResults := a.generateReleaseDocuments(r.Context(), recallID, reason, level, regulatorRef.String, desc, channelSet, u.Username)

	writeJSON(w, 200, map[string]any{
		"recall_no": recallNo, "status": "RELEASED",
		"frozen_in_stock_boxes": frozenBoxes,
		"affected_shipments":    affectedShipments,
		"affected_orders":       affectedOrders,
		"documents":             docResults,
	})
}

func (a *App) generateReleaseDocuments(ctx context.Context, recallID int64, reason, level, regulatorRef, desc string, channelSet map[int64]string, by string) []map[string]any {
	out := []map[string]any{}
	recallNo := a.scalarText(`SELECT recall_no FROM recalls WHERE id=$1`, recallID)
	batchNos := a.stringCol(`SELECT pb.batch_no FROM recall_batches rb JOIN product_batches pb ON pb.id=rb.product_batch_id WHERE rb.recall_id=$1 ORDER BY pb.batch_no`, recallID)

	// 每个受影响渠道一份停售通知 + 退货通知
	for chID := range channelSet {
		chName := a.scalarText(`SELECT name FROM channels WHERE id=$1`, chID)
		if docNo, ok := a.insertRecallDoc(ctx, recallID, "STOP_SALE_NOTICE", chName,
			fmt.Sprintf("【立即停售通知】召回单号 %s，召回级别 %s。请贵渠道（%s）立即停止销售以下批次：%s。原因：%s（%s）。",
				recallNo, level, chName, strings.Join(batchNos, "、"), reasonText(reason), desc)); ok {
			out = append(out, map[string]any{"doc_type": "STOP_SALE_NOTICE", "target": chName, "doc_no": docNo})
		}
		if reason != "TEMP_EXCEEDED" {
			a.insertRecallDoc(ctx, recallID, "RETURN_NOTICE", chName,
				fmt.Sprintf("【退货通知】召回单号 %s。请 %s 将以下批次全部库存与在途货物退回工厂，运费由工厂承担：%s。",
					recallNo, chName, strings.Join(batchNos, "、")))
		}
	}

	// 监管报告
	if reason == "REGULATOR_RECALL" || regulatorRef != "" {
		a.insertRecallDoc(ctx, recallID, "REGULATOR_REPORT", "监管部门",
			fmt.Sprintf("【召回备案报告】召回单号 %s，监管文号 %s，级别 %s，涉及批次 %s，原因：%s。%s",
				recallNo, nz(regulatorRef, "-"), level, strings.Join(batchNos, "、"), reasonText(reason), desc))
	}

	// 追溯报告（原料->成品->渠道->订单全景）
	a.buildTraceReport(ctx, recallID, by)
	out = append(out, map[string]any{"doc_type": "TRACE_REPORT", "target": "内部"})
	return out
}

// ---- 手工生成召回材料 ----

func (a *App) generateRecallDocument(w http.ResponseWriter, r *http.Request) {
	recallNo := r.PathValue("recallNo")
	docType := r.PathValue("docType")
	validDoc := map[string]bool{
		"STOP_SALE_NOTICE": true, "RETURN_NOTICE": true, "COMPENSATION_PLAN": true,
		"SUPPLIER_CLAIM": true, "REGULATOR_REPORT": true, "TRACE_REPORT": true,
	}
	if !validDoc[docType] {
		writeErr(w, 400, "doc_type 非法")
		return
	}
	var recallID int64
	var reason, level, desc string
	var regRef sql.NullString
	if err := a.db.QueryRow(`SELECT id,reason_type,level,COALESCE(regulator_ref_no,''),description FROM recalls WHERE recall_no=$1`,
		recallNo).Scan(&recallID, &reason, &level, &regRef, &desc); err != nil {
		writeErr(w, 404, "召回单不存在")
		return
	}
	var req struct {
		Target  string `json:"target"`
		Content string `json:"content"`
	}
	_ = decode(r, &req)
	u := currentUser(r)

	batchNos := a.stringCol(`SELECT pb.batch_no FROM recall_batches rb JOIN product_batches pb ON pb.id=rb.product_batch_id WHERE rb.recall_id=$1`, recallID)
	target := nz(req.Target, "内部")
	content := req.Content
	if content == "" {
		switch docType {
		case "STOP_SALE_NOTICE":
			content = fmt.Sprintf("【立即停售通知】召回单 %s 级别 %s，请 %s 立即停售批次：%s。原因：%s。", recallNo, level, target, strings.Join(batchNos, "、"), reasonText(reason))
		case "RETURN_NOTICE":
			content = fmt.Sprintf("【退货通知】召回单 %s，请 %s 退回批次：%s。", recallNo, target, strings.Join(batchNos, "、"))
		case "COMPENSATION_PLAN":
			content = fmt.Sprintf("【赔偿方案】召回单 %s：对受影响已售订单按订单金额先行赔付/退款，批次：%s。", recallNo, strings.Join(batchNos, "、"))
		case "REGULATOR_REPORT":
			content = fmt.Sprintf("【召回备案报告】召回单 %s，监管文号 %s，级别 %s，批次 %s，原因：%s。%s", recallNo, nz(regRef.String, "-"), level, strings.Join(batchNos, "、"), reasonText(reason), desc)
		case "SUPPLIER_CLAIM":
			content = fmt.Sprintf("【供应商追责函】召回单 %s，批次 %s，原因：%s。请供应商确认责任并承担召回损失（详见财务追偿）。", recallNo, strings.Join(batchNos, "、"), reasonText(reason))
		}
	}
	if docType == "TRACE_REPORT" {
		a.buildTraceReport(r.Context(), recallID, u.Username)
		doc, _ := a.queryOneMap(`SELECT doc_no,title,content FROM recall_documents WHERE recall_id=$1 AND doc_type='TRACE_REPORT'`, recallID)
		writeJSON(w, 201, doc)
		return
	}
	if _, ok := a.insertRecallDoc(r.Context(), recallID, docType, target, content); !ok {
		writeErr(w, 409, "该对象的同类材料已存在")
		return
	}
	doc, _ := a.queryOneMap(`SELECT doc_no,title,target,content,generated_at FROM recall_documents WHERE recall_id=$1 AND doc_type=$2 AND target=$3`,
		recallID, docType, target)
	writeJSON(w, 201, doc)
}

func (a *App) listRecallDocuments(w http.ResponseWriter, r *http.Request) {
	recallNo := r.PathValue("recallNo")
	var recallID int64
	if err := a.db.QueryRow(`SELECT id FROM recalls WHERE recall_no=$1`, recallNo).Scan(&recallID); err != nil {
		writeErr(w, 404, "召回单不存在")
		return
	}
	rows, err := a.db.Query(`SELECT id,doc_type,doc_no,title,target,content,generated_by,generated_at
		FROM recall_documents WHERE recall_id=$1 ORDER BY id`, recallID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"recall_no": recallNo, "items": scanRows(rows)})
}

// ---- 材料生成辅助 ----

// insertRecallDoc 插入召回材料；返回 docNo 与是否新插入（false 表示同对象同类材料已存在）
func (a *App) insertRecallDoc(ctx context.Context, recallID int64, docType, target, content string) (string, bool) {
	docNo, err := a.nextDocNo(ctx, "DN")
	if err != nil {
		return "", false
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO recall_documents(recall_id,doc_type,target,doc_no,title,content,generated_by)
		VALUES($1,$2,$3,$4,$5,$6,'system')
		ON CONFLICT (recall_id,doc_type,target) DO NOTHING`, recallID, docType, target, docNo, docTitle(docType), content)
	if err != nil {
		return "", false
	}
	var exists bool
	_ = a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recall_documents WHERE recall_id=$1 AND doc_no=$2)`, recallID, docNo).Scan(&exists)
	if !exists {
		// 冲突未插入：取回已有单号
		existing := a.scalarText(`SELECT doc_no FROM recall_documents WHERE recall_id=$1 AND doc_type=$2 AND target=$3`, recallID, docType, target)
		return existing, false
	}
	return docNo, true
}

func (a *App) buildTraceReport(ctx context.Context, recallID int64, by string) {
	var sections []string
	rows, _ := a.db.QueryContext(ctx, `SELECT pb.batch_no,
		COALESCE((SELECT string_agg(s.name||'/'||rb.batch_no,'；') FROM material_genealogy g
			JOIN raw_material_batches rb ON rb.id=g.raw_batch_id JOIN suppliers s ON s.id=rb.supplier_id
			WHERE g.product_batch_id=pb.id),'无') AS upstream,
		COALESCE((SELECT string_agg(DISTINCT c.name,'；') FROM shipments s JOIN channels c ON c.id=s.channel_id WHERE s.product_batch_id=pb.id),'未发货'),
		COALESCE((SELECT string_agg(DISTINCT lab_result,',') FROM retained_samples WHERE product_batch_id=pb.id),'无留样')
		FROM recall_batches rb JOIN product_batches pb ON pb.id=rb.product_batch_id WHERE rb.recall_id=$1`, recallID)
	for rows.Next() {
		var bn, up, ch, sample string
		rows.Scan(&bn, &up, &ch, &sample)
		sections = append(sections, fmt.Sprintf("成品批次 %s｜上游原料: %s｜已发渠道: %s｜留样结论: %s", bn, up, ch, sample))
	}
	rows.Close()
	content := "【批次追溯报告】\n" + strings.Join(sections, "\n") +
		"\n说明：本报告由系统依据采购入库、仓储温控、解冻加工、品控留样、发货订单记录自动汇总。"
	a.db.ExecContext(ctx, `INSERT INTO recall_documents(recall_id,doc_type,target,doc_no,title,content,generated_by)
		VALUES($1,'TRACE_REPORT','内部','DN-TRACE',$2,$3,$4)
		ON CONFLICT (recall_id,doc_type,target) DO UPDATE SET content=EXCLUDED.content, generated_at=now()`,
		recallID, docTitle("TRACE_REPORT"), content, nz(by, "system"))
}

func docTitle(t string) string {
	switch t {
	case "STOP_SALE_NOTICE":
		return "停售通知"
	case "RETURN_NOTICE":
		return "退货通知"
	case "COMPENSATION_PLAN":
		return "赔偿方案"
	case "SUPPLIER_CLAIM":
		return "供应商追责函"
	case "REGULATOR_REPORT":
		return "监管召回备案报告"
	case "TRACE_REPORT":
		return "批次追溯报告"
	}
	return t
}

func reasonText(t string) string {
	switch t {
	case "TEMP_EXCEEDED":
		return "原料/仓储温度超标"
	case "THAW_TIMEOUT":
		return "解冻超时"
	case "SEASONING_MISMATCH":
		return "调味料批次用错"
	case "METAL_ALARM":
		return "金检报警"
	case "PACKAGE_LEAK":
		return "包装漏气"
	case "OFF_ODOR":
		return "渠道反馈异味"
	case "LIVESTREAM_BATCH_RETURN":
		return "直播间集中退货"
	case "REGULATOR_RECALL":
		return "监管要求召回"
	}
	return t
}

func (a *App) scalarText(q string, args ...any) string {
	var s sql.NullString
	_ = a.db.QueryRow(q, args...).Scan(&s)
	return s.String
}

func (a *App) stringCol(q string, args ...any) []string {
	rows, err := a.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			out = append(out, s)
		}
	}
	return out
}
