package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ---------- 召回创建 ----------

type recallReq struct {
	EventNo       string `json:"event_no"`       // 触发品控事件（可空，配合 raw_batch_no）
	RawBatchNo    string `json:"raw_batch_no"`    // 直接按原料批次召回
	Level         string `json:"level"`          // I/II/III
	Reason        string `json:"reason"`
}

func (s *Server) createRecall(w http.ResponseWriter, r *http.Request) {
	var req recallReq
	if !decode(w, r, &req) || req.Reason == "" {
		errJSON(w, 400, "reason 必填（event_no 与 raw_batch_no 至少提供一个）")
		return
	}
	if req.Level != "I" && req.Level != "II" && req.Level != "III" {
		req.Level = s.setting("recall_auto_level")
		if req.Level == "" {
			req.Level = "II"
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var eventID, seedRaw sql.NullInt64
	var seedFGs []int64
	if req.EventNo != "" {
		var evType, evStatus string
		if err = tx.QueryRow(`SELECT id,type,status,raw_batch_id,finished_batch_id
			FROM quality_events WHERE event_no=$1`, req.EventNo).
			Scan(&eventID, &evType, &evStatus, &seedRaw, new(sql.NullInt64)); err != nil {
			errJSON(w, 400, "品控事件不存在: "+req.EventNo)
			return
		}
		_ = evType
		_ = evStatus
		var fg sql.NullInt64
		tx.QueryRow(`SELECT finished_batch_id FROM quality_events WHERE event_no=$1`,
			req.EventNo).Scan(&fg)
		if fg.Valid {
			seedFGs = append(seedFGs, fg.Int64)
		}
	}
	if req.RawBatchNo != "" {
		id, e := lookupID(tx, `SELECT id FROM raw_batches WHERE batch_no=$1`, req.RawBatchNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		seedRaw = nullInt64(id)
	}
	if !eventID.Valid && !seedRaw.Valid && len(seedFGs) == 0 {
		errJSON(w, 400, "event_no 与 raw_batch_no 至少提供一个")
		return
	}

	rawSet := map[int64]bool{}
	fgSet := map[int64]bool{}
	if seedRaw.Valid {
		rawSet[seedRaw.Int64] = true
	}
	for _, f := range seedFGs {
		fgSet[f] = true
	}
	// 原料批次 → 消费它的工单 → 成品批次
	if seedRaw.Valid {
		rows, e := tx.Query(`SELECT DISTINCT fb.id FROM finished_batches fb
			JOIN material_usages mu ON mu.work_order_id=fb.work_order_id
			WHERE mu.raw_batch_id=$1`, seedRaw.Int64)
		if e != nil {
			errJSON(w, 500, e.Error())
			return
		}
		for rows.Next() {
			var x int64
			rows.Scan(&x)
			fgSet[x] = true
		}
		rows.Close()
	}
	// 成品返工链扩散：返工产出的成品同样在召回范围
	expandReworks(tx, fgSet)
	// 成品 → 其工单消费过的全部原料批次（用于供应商追责与箱定位）
	for f := range fgSet {
		rows, _ := tx.Query(`SELECT DISTINCT raw_batch_id FROM material_usages
			WHERE work_order_id=(SELECT work_order_id FROM finished_batches WHERE id=$1)`, f)
		for rows.Next() {
			var x int64
			rows.Scan(&x)
			rawSet[x] = true
		}
		rows.Close()
	}
	if len(fgSet) == 0 {
		errJSON(w, 409, "该触发范围尚未产出任何成品批次；若需封存原料请直接登记品控事件隔离")
		return
	}

	no, _ := s.nextDocNo("RC")
	var recallID int64
	if err = tx.QueryRow(`INSERT INTO recalls(recall_no,trigger_event_id,raw_batch_id,level,reason,status,created_by)
		VALUES ($1,$2,$3,$4,$5,'draft',$6) RETURNING id`,
		no, eventID, seedRaw, req.Level, req.Reason, currentUser(r).Username).Scan(&recallID); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	for f := range fgSet {
		tx.Exec(`INSERT INTO recall_scopes(recall_id,finished_batch_id) VALUES ($1,$2)
			ON CONFLICT DO NOTHING`, recallID, f)
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	snap := s.recallSnapshot(recallID)
	writeJSON(w, 201, snap)
}

func expandReworks(tx *sql.Tx, fgSet map[int64]bool) {
	for {
		 grew := false
		var ids []int64
		for f := range fgSet {
			ids = append(ids, f)
		}
		rows, _ := tx.Query(`SELECT id,rework_of_id FROM finished_batches WHERE rework_of_id = ANY($1)`,
			pqInt64Array(ids))
		for rows.Next() {
			var id int64
			var parent sql.NullInt64
			rows.Scan(&id, &parent)
			if !fgSet[id] {
				fgSet[id] = true
				grew = true
			}
		}
		rows.Close()
		if !grew {
			return
		}
	}
}

// ---------- 召回快照（定位渠道/库存/订单/留样/箱） ----------

func (s *Server) recallSnapshot(recallID int64) map[string]any {
	db := s.db
	var no, level, reason, status string
	var triggerEvent, rawBatch sql.NullString
	var createdAt, releasedAt sql.NullTime
	db.QueryRow(`SELECT rc.recall_no,rc.level,rc.reason,rc.status,
			COALESCE(qe.event_no,''),COALESCE(rb.batch_no,''),rc.created_at,rc.released_at
		FROM recalls rc
		LEFT JOIN quality_events qe ON qe.id=rc.trigger_event_id
		LEFT JOIN raw_batches rb ON rb.id=rc.raw_batch_id
		WHERE rc.id=$1`, recallID).
		Scan(&no, &level, &reason, &status, &triggerEvent, &rawBatch, &createdAt, &releasedAt)

	// 范围内成品
	rows, _ := db.Query(`SELECT fb.id,fb.batch_no,p.code AS product_code,p.name AS product_name,
		fb.qty_kg,fb.status,COALESCE(c.code,'') AS channel_code,fb.pack_spec
		FROM recall_scopes rs JOIN finished_batches fb ON fb.id=rs.finished_batch_id
		JOIN products p ON p.id=fb.product_id
		LEFT JOIN channels c ON c.id=fb.pack_channel_id
		WHERE rs.recall_id=$1 ORDER BY fb.id`, recallID)
	fgs := scanMaps(rows)
	rows.Close()
	var fgIDs []int64
	for _, f := range fgs {
		fgIDs = append(fgIDs, int64(f["id"].(int64)))
	}

	// 库存分量
	stockRows, _ := db.Query(`SELECT fs.finished_batch_id,fs.qty_on_hand,fs.qty_hold,
		fs.qty_quarantined,fs.qty_shipped,fs.qty_destroyed,fs.qty_reworked
		FROM finished_stock fs WHERE fs.finished_batch_id = ANY($1)`, pqInt64Array(fgIDs))
	stockByFG := map[int64]map[string]any{}
	for _, m := range scanMaps(stockRows) {
		stockByFG[int64(m["finished_batch_id"].(int64))] = m
	}

	// 已发货渠道（精准到发货单与数量）
	chRows, _ := db.Query(`SELECT c.id,c.code,c.name,c.type,sh.shipment_no,sh.status,
		si.finished_batch_id,si.qty_kg,sh.shipped_at
		FROM shipment_items si
		JOIN shipments sh ON sh.id=si.shipment_id
		JOIN channels c ON c.id=sh.channel_id
		WHERE si.finished_batch_id = ANY($1) ORDER BY sh.id`, pqInt64Array(fgIDs))
	shipments := scanMaps(chRows)
	chRows.Close()

	// 已售订单（精准到订单）
	oRows, _ := db.Query(`SELECT o.id,o.order_no,c.code AS channel_code,o.finished_batch_id,
		fb.batch_no AS finished_batch_no,o.buyer,o.qty_kg,o.amount,o.status,o.created_at
		FROM customer_orders o JOIN channels c ON c.id=o.channel_id
		JOIN finished_batches fb ON fb.id=o.finished_batch_id
		WHERE o.finished_batch_id = ANY($1) ORDER BY o.id`, pqInt64Array(fgIDs))
	orders := scanMaps(oRows)
	oRows.Close()

	// 留样结论
	spRows, _ := db.Query(`SELECT sr.finished_batch_id,sr.sample_no,sr.result,sr.report_no,sr.conclusion
		FROM sample_retentions sr WHERE sr.finished_batch_id = ANY($1) ORDER BY sr.id`,
		pqInt64Array(fgIDs))
	samples := scanMaps(spRows)
	spRows.Close()

	// 关联原料批次与箱（精准到箱）
	rawRows, _ := db.Query(`SELECT DISTINCT rb.id,rb.batch_no,m.code AS material_code,
		s.code AS supplier_code,s.name AS supplier_name,rb.status,rb.total_weight_kg
		FROM recall_scopes rs
		JOIN material_usages mu ON mu.work_order_id=(SELECT work_order_id FROM finished_batches WHERE id=rs.finished_batch_id)
		JOIN raw_batches rb ON rb.id=mu.raw_batch_id
		JOIN materials m ON m.id=rb.material_id
		JOIN suppliers s ON s.id=rb.supplier_id
		WHERE rs.recall_id=$1 ORDER BY rb.id`, recallID)
	raws := scanMaps(rawRows)
	rawRows.Close()
	var rawIDs []int64
	for _, m := range raws {
		rawIDs = append(rawIDs, int64(m["id"].(int64)))
	}
	boxRows, _ := db.Query(`SELECT b.raw_batch_id,b.box_no,COALESCE(l.code,'') AS location_code,
		b.weight_kg,b.status
		FROM raw_boxes b LEFT JOIN locations l ON l.id=b.location_id
		WHERE b.raw_batch_id = ANY($1) ORDER BY b.raw_batch_id,b.box_no`,
		pqInt64Array(rawIDs))
	boxes := scanMaps(boxRows)
	boxRows.Close()

	// 退货与处置
	retRows, _ := db.Query(`SELECT rg.return_no,fb.batch_no AS finished_batch_no,
		COALESCE(c.code,'') channel_code,rg.qty_kg,rg.status,rg.disposition,rg.arrived_at
		FROM return_goods rg JOIN finished_batches fb ON fb.id=rg.finished_batch_id
		LEFT JOIN channels c ON c.id=rg.channel_id
		WHERE rg.recall_id=$1 ORDER BY rg.id`, recallID)
	returns := scanMaps(retRows)
	retRows.Close()

	// 材料/动作
	aRows, _ := db.Query(`SELECT action_type,target_type,COALESCE(target_ref,'') AS target_ref,
		title,status,created_at,executed_at
		FROM recall_actions WHERE recall_id=$1 ORDER BY id`, recallID)
	actions := scanMaps(aRows)
	aRows.Close()

	// 汇总
	var onHandTotal, holdTotal, shippedTotal, returnedTotal, destroyedTotal float64
	for _, f := range fgs {
		id := int64(f["id"].(int64))
		if st, ok := stockByFG[id]; ok {
			onHandTotal += toFloat(st["qty_on_hand"])
			holdTotal += toFloat(st["qty_hold"])
			shippedTotal += toFloat(st["qty_shipped"])
			returnedTotal += toFloat(st["qty_quarantined"])
			destroyedTotal += toFloat(st["qty_destroyed"])
		}
	}
	pendingOrders := 0
	for _, o := range orders {
		if o["status"] == "sold" {
			pendingOrders++
		}
	}
	unqualifiedSamples := 0
	for _, sp := range samples {
		if sp["result"] == "unqualified" {
			unqualifiedSamples++
		}
	}

	for _, f := range fgs {
		id := int64(f["id"].(int64))
		if st, ok := stockByFG[id]; ok {
			f["stock"] = st
		}
	}

	return map[string]any{
		"id": recallID, "recall_no": no, "level": level, "reason": reason, "status": status,
		"trigger_event_no": triggerEvent.String, "seed_raw_batch_no": rawBatch.String,
		"created_at": createdAt.Time, "released_at": releasedAt.Time,
		"finished_batches": fgs,
		"shipments":        shipments,
		"orders":           orders,
		"samples":          samples,
		"raw_batches":      raws,
		"boxes":            boxes,
		"returns":          returns,
		"documents":        actions,
		"summary": map[string]any{
			"finished_batch_count":  len(fgs),
			"raw_batch_count":       len(raws),
			"box_count":             len(boxes),
			"on_hand_kg":            onHandTotal,
			"hold_kg":               holdTotal,
			"shipped_kg":            shippedTotal,
			"returned_quarantine_kg": returnedTotal,
			"destroyed_kg":          destroyedTotal,
			"order_count":           len(orders),
			"pending_notify_orders": pendingOrders,
			"unqualified_samples":   unqualifiedSamples,
		},
	}
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	}
	return 0
}

func (s *Server) getRecall(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	if !s.recallExists(id) {
		errJSON(w, 404, "recall not found")
		return
	}
	writeJSON(w, 200, s.recallSnapshot(id))
}

func (s *Server) listRecalls(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT rc.id,rc.recall_no,rc.level,rc.reason,rc.status,
		COALESCE(qe.event_no,'') AS trigger_event_no,
		COALESCE(rb.batch_no,'') AS raw_batch_no,rc.created_at,rc.released_at,
		(SELECT COUNT(*) FROM recall_scopes WHERE recall_id=rc.id) AS scope_count
		FROM recalls rc
		LEFT JOIN quality_events qe ON qe.id=rc.trigger_event_id
		LEFT JOIN raw_batches rb ON rb.id=rc.raw_batch_id
		ORDER BY rc.id DESC`)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

func (s *Server) recallExists(id int64) bool {
	var x int
	s.db.QueryRow(`SELECT 1 FROM recalls WHERE id=$1`, id).Scan(&x)
	return x == 1
}

// ---------- 召回发布：停售/退货/赔偿/追责/监管报告材料 ----------

func (s *Server) releaseRecall(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	var no, level, reason, status string
	var eventID, seedRaw sql.NullInt64
	if err = tx.QueryRow(`SELECT recall_no,level,reason,status,trigger_event_id,raw_batch_id
		FROM recalls WHERE id=$1 FOR UPDATE`, id).Scan(&no, &level, &reason, &status, &eventID, &seedRaw); err != nil {
		errJSON(w, 404, "recall not found")
		return
	}
	if status != "draft" {
		errJSON(w, 409, "召回单已发布，状态: "+status)
		return
	}
	var fgIDs []int64
	rows, _ := tx.Query(`SELECT finished_batch_id FROM recall_scopes WHERE recall_id=$1`, id)
	for rows.Next() {
		var x int64
		rows.Scan(&x)
		fgIDs = append(fgIDs, x)
	}
	rows.Close()

	// 1) 在库可售库存全部冻结停售
	for _, fg := range fgIDs {
		var batchNo string
		tx.QueryRow(`SELECT batch_no FROM finished_batches WHERE id=$1`, fg).Scan(&batchNo)
		var onHand float64
		tx.QueryRow(`SELECT qty_on_hand FROM finished_stock WHERE finished_batch_id=$1 FOR UPDATE`,
			fg).Scan(&onHand)
		if onHand > 0 {
			tx.Exec(`UPDATE finished_stock SET qty_on_hand=0,qty_hold=qty_hold+$1,updated_at=now()
				WHERE finished_batch_id=$2`, onHand, fg)
			tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
				VALUES ($1,$2,0,'RECALL_HOLD',$3)`, fg, -onHand, no)
		}
		tx.Exec(`UPDATE finished_batches SET status='recalled' WHERE id=$1`, fg)
		tx.Exec(`INSERT INTO recall_actions(recall_id,action_type,target_type,target_ref,title,content,status,executed_at)
			VALUES ($1,'STOP_SALE','system',$2,$3,$4,'issued',now())`,
			id, batchNo, "停售通知-成品批次 "+batchNo,
			fmt.Sprintf("自即日起停止成品批次 %s 的一切销售，在库 %skg 即刻冻结封存，等待处置指令。",
				batchNo, strconvFormat(onHand)))
	}

	// 2) 已售订单：通知召回
	rows, _ = tx.Query(`SELECT id,order_no FROM customer_orders
		WHERE finished_batch_id = ANY($1) AND status='sold' FOR UPDATE`, pqInt64Array(fgIDs))
	type oRef struct{ id int64; no string }
	var soldOrders []oRef
	for rows.Next() {
		var x oRef
		rows.Scan(&x.id, &x.no)
		soldOrders = append(soldOrders, x)
	}
	rows.Close()
	for _, o := range soldOrders {
		tx.Exec(`UPDATE customer_orders SET status='recall_notified' WHERE id=$1`, o.id)
		tx.Exec(`INSERT INTO recall_actions(recall_id,action_type,target_type,target_ref,title,content,status,executed_at)
			VALUES ($1,'CHANNEL_NOTICE','order',$2,$3,$4,'issued',now())`,
			id, o.no, "消费者召回通知-订单 "+o.no,
			"订单 "+o.no+" 所涉产品纳入召回范围，请停止食用并联系客服办理退货与赔付。")
	}

	// 3) 渠道：退货通知 + 赔偿（退款）清单
	rows, _ = tx.Query(`SELECT DISTINCT c.id,c.code,c.name,COALESCE(SUM(o.amount),0),COUNT(o.id)
		FROM customer_orders o JOIN channels c ON c.id=o.channel_id
		WHERE o.finished_batch_id = ANY($1) GROUP BY c.id,c.code,c.name`, pqInt64Array(fgIDs))
	type chAgg struct {
		id                int64
		code, name        string
		amount            float64
		orderCnt          int
	}
	var channels []chAgg
	for rows.Next() {
		var a chAgg
		rows.Scan(&a.id, &a.code, &a.name, &a.amount, &a.orderCnt)
		channels = append(channels, a)
	}
	rows.Close()
	for _, a := range channels {
		tx.Exec(`INSERT INTO recall_actions(recall_id,action_type,target_type,target_ref,title,content,status,executed_at)
			VALUES ($1,'RETURN_REQUEST','channel',$2,$3,$4,'issued',now())`,
			id, a.code, "退货通知-渠道 "+a.code+" "+a.name,
			fmt.Sprintf("召回单号 %s：请渠道 %s 立即下架涉事批次产品并安排退回工厂，涉及订单 %d 笔。",
				no, a.name, a.orderCnt))
		compNo, _ := s.nextDocNo("CP")
		tx.Exec(`INSERT INTO compensation_records(comp_no,recall_id,channel_id,amount,kind,note)
			VALUES ($1,$2,$3,$4,'REFUND',$5)`,
			compNo, id, a.id, a.amount, "召回 "+no+" 渠道 "+a.code+" 订单退款汇总")
		tx.Exec(`INSERT INTO recall_actions(recall_id,action_type,target_type,target_ref,title,content,status,executed_at)
			VALUES ($1,'COMPENSATION','channel',$2,$3,$4,'issued',now())`,
			id, a.code, "赔偿清单-渠道 "+a.code,
			fmt.Sprintf("召回 %s 对渠道 %s 拟定退款/赔偿合计 %.2f 元（赔付单号 %s），由财务复核支付。",
				no, a.name, a.amount, compNo))
	}
	// 发货单置为召回
	tx.Exec(`UPDATE shipments SET status='recalled' WHERE id IN
		(SELECT DISTINCT shipment_id FROM shipment_items WHERE finished_batch_id = ANY($1))`,
		pqInt64Array(fgIDs))

	// 4) 供应商追责材料 + 在库原料箱精准封存
	rows, _ = tx.Query(`SELECT DISTINCT s.id,s.code,s.name,rb.batch_no
		FROM material_usages mu
		JOIN raw_batches rb ON rb.id=mu.raw_batch_id
		JOIN suppliers s ON s.id=rb.supplier_id
		WHERE mu.work_order_id IN (SELECT DISTINCT work_order_id FROM finished_batches WHERE id = ANY($1))`,
		pqInt64Array(fgIDs))
	type supAgg struct{ id int64; code, name, rawBatch string }
	supSeen := map[string]bool{}
	var sups []supAgg
	for rows.Next() {
		var a supAgg
		rows.Scan(&a.id, &a.code, &a.name, &a.rawBatch)
		sups = append(sups, a)
	}
	rows.Close()
	supMats := map[string][]string{}
	for _, a := range sups {
		supMats[a.code] = append(supMats[a.code], a.rawBatch)
	}
	for _, a := range sups {
		if supSeen[a.code] {
			continue
		}
		supSeen[a.code] = true
		tx.Exec(`INSERT INTO recall_actions(recall_id,action_type,target_type,target_ref,title,content,status,executed_at)
			VALUES ($1,'SUPPLIER_CLAIM','supplier',$2,$3,$4,'issued',now())`,
			id, a.code, "供应商追责函-"+a.name,
			fmt.Sprintf("召回 %s 涉及供应商 %s（%s）原料批次 %s，请封存同批次库存并配合调查；" +
				"损失金额由财务核算后通过供应商扣款追偿。",
				no, a.name, a.code, strings.Join(supMats[a.code], "、")))
	}
	// 在库原料批次/箱封存（精准到箱）
	tx.Exec(`UPDATE raw_batches SET status='recalled' WHERE id IN
		(SELECT DISTINCT mu.raw_batch_id FROM material_usages mu
		 WHERE mu.work_order_id IN (SELECT DISTINCT work_order_id FROM finished_batches WHERE id = ANY($1)))
		AND status IN ('in_storage','quarantined','in_production')`, pqInt64Array(fgIDs))
	tx.Exec(`UPDATE raw_boxes SET status='recalled' WHERE raw_batch_id IN
		(SELECT DISTINCT mu.raw_batch_id FROM material_usages mu
		 WHERE mu.work_order_id IN (SELECT DISTINCT work_order_id FROM finished_batches WHERE id = ANY($1)))
		AND status IN ('in_storage','quarantined')`, pqInt64Array(fgIDs))

	// 5) 监管报告
	tx.Exec(`INSERT INTO recall_actions(recall_id,action_type,target_type,target_ref,title,content,status,executed_at)
		VALUES ($1,'REGULATOR_REPORT','regulator','MARKET_REGULATOR',$2,$3,'issued',now())`,
		id, "食品召回报告-"+no,
		fmt.Sprintf("召回级别 %s 级，召回单号 %s。原因：%s。涉及成品批次 %d 个、订单 %d 笔，已采取停售、下架、退货、消费者通知等措施。",
			level, no, reason, len(fgIDs), len(soldOrders)))

	// 6) 触发事件置为已触发召回；召回发布
	if eventID.Valid {
		tx.Exec(`UPDATE quality_events SET status='recall_triggered' WHERE id=$1 AND status<>'closed'`, eventID.Int64)
	}
	tx.Exec(`UPDATE recalls SET status='released',released_at=now() WHERE id=$1`, id)

	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if s.cache != nil {
		s.cache.invalidateTrace(r.Context(), nil, fgIDs)
	}
	writeJSON(w, 200, s.recallSnapshot(id))
}

// recallDocuments 输出可打印的召回材料全文
func (s *Server) recallDocuments(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	if !s.recallExists(id) {
		errJSON(w, 404, "recall not found")
		return
	}
	rows, err := s.db.Query(`SELECT action_type,target_type,target_ref,title,content,status,created_at
		FROM recall_actions WHERE recall_id=$1 ORDER BY
		CASE action_type WHEN 'STOP_SALE' THEN 1 WHEN 'CHANNEL_NOTICE' THEN 2 WHEN 'RETURN_REQUEST' THEN 3
		WHEN 'COMPENSATION' THEN 4 WHEN 'SUPPLIER_CLAIM' THEN 5 WHEN 'REGULATOR_REPORT' THEN 6 END,id`, id)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	docs := scanMaps(rows)
	writeJSON(w, 200, map[string]any{
		"recall_id": id,
		"document_count": len(docs),
		"documents": docs,
	})
}
