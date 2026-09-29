package main

import (
	"database/sql"
	"net/http"
)

// receiveReturn 退货回厂登记。来源可为渠道退货、直播间退货、库内召回冻结。
func (a *App) receiveReturn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RecallNo   string `json:"recall_no"`
		BoxNo      string `json:"box_no"`
		Source     string `json:"source"`
		ReceivedBy string `json:"received_by"`
	}
	if err := decode(r, &req); err != nil || req.BoxNo == "" || req.ReceivedBy == "" {
		writeErr(w, 400, "box_no/received_by 必填")
		return
	}
	source := nz(req.Source, "CHANNEL_RETURN")
	if source != "CHANNEL_RETURN" && source != "LIVESTREAM_RETURN" && source != "WAREHOUSE_RECALL" {
		writeErr(w, 400, "source 必须为 CHANNEL_RETURN/LIVESTREAM_RETURN/WAREHOUSE_RECALL")
		return
	}
	var boxID int64
	var bstatus string
	if err := a.db.QueryRow(`SELECT id,status FROM product_boxes WHERE box_no=$1`, req.BoxNo).Scan(&boxID, &bstatus); err != nil {
		writeErr(w, 404, "箱号不存在")
		return
	}
	if bstatus == "DESTROYED" || bstatus == "RETURN_HOLD" || bstatus == "RETESTING" || bstatus == "REWORKING" {
		writeErr(w, 409, "箱号当前状态 "+bstatus+"，不可重复回厂登记")
		return
	}
	var recallID sql.NullInt64
	if req.RecallNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM recalls WHERE recall_no=$1`, req.RecallNo).Scan(&recallID); err != nil {
			writeErr(w, 404, "召回单不存在: "+req.RecallNo)
			return
		}
	}

	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`INSERT INTO returned_units(recall_id,box_id,source,received_by,decision,final_status)
		VALUES($1,$2,$3,$4,'PENDING','OPEN') RETURNING id`, recallID, boxID, source, req.ReceivedBy).Scan(&id); err != nil {
		writeErr(w, 409, "该箱已存在回厂登记: "+err.Error())
		return
	}
	// 进入隔离待检库
	var qid int64
	if err := tx.QueryRow(`SELECT id FROM storage_locations WHERE code='HG-01'`).Scan(&qid); err == nil {
		tx.Exec(`UPDATE product_boxes SET status='RETURN_HOLD',location_id=$2,updated_at=now() WHERE id=$1`, boxID, qid)
	} else {
		tx.Exec(`UPDATE product_boxes SET status='RETURN_HOLD',updated_at=now() WHERE id=$1`, boxID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"id": id, "box_no": req.BoxNo, "decision": "PENDING",
		"disposition_options": []string{"RETEST", "REWORK", "DESTROY"},
	})
}

// decideReturn 品控分级：可复检 / 可返工 / 必须销毁。
// REWORK 可二次调用：REWORKING 完成后置 REWORKED。
func (a *App) decideReturn(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var req struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := decode(r, &req); err != nil || (req.Decision != "RETEST" && req.Decision != "REWORK" && req.Decision != "DESTROY") {
		writeErr(w, 400, "decision 必须为 RETEST/REWORK/DESTROY")
		return
	}
	var boxID int64
	var decision, finalStatus string
	if err := a.db.QueryRow(`SELECT box_id,decision,final_status FROM returned_units WHERE id=$1`, id).Scan(&boxID, &decision, &finalStatus); err != nil {
		writeErr(w, 404, "退货记录不存在")
		return
	}
	u := currentUser(r)
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()

	switch req.Decision {
	case "RETEST":
		if finalStatus != "OPEN" {
			writeErr(w, 409, "当前处置阶段为 "+finalStatus+"，不能改为复检")
			return
		}
		tx.Exec(`UPDATE returned_units SET decision='RETEST',retest_result='PENDING',final_status='RETESTING',decided_by=$2,decided_at=now(),note=COALESCE(NULLIF($3,''),note) WHERE id=$1`, id, u.Username, req.Note)
		tx.Exec(`UPDATE product_boxes SET status='RETESTING',updated_at=now() WHERE id=$1`, boxID)
	case "REWORK":
		if finalStatus == "REWORKING" {
			// 返工完成，回到成品冷库，状态 REWORKED（可再次销售，但召回未解除时仍受批次状态拦截）
			tx.Exec(`UPDATE returned_units SET final_status='REWORKED',handled_at=now(),note=COALESCE(NULLIF($2,''),note) WHERE id=$1`, id, req.Note)
			var rpbID int64
			var rloc sql.NullInt64
			tx.QueryRow(`SELECT batch_id FROM product_boxes WHERE id=$1`, boxID).Scan(&rpbID)
			tx.QueryRow(`SELECT cold_location_id FROM product_batches WHERE id=$1`, rpbID).Scan(&rloc)
			tx.Exec(`UPDATE product_boxes SET status='REWORKED',reworked=TRUE,location_id=$2,updated_at=now() WHERE id=$1`, boxID, rloc)
			break
		}
		if finalStatus != "OPEN" {
			writeErr(w, 409, "当前处置阶段为 "+finalStatus+"，不能改为返工")
			return
		}
		tx.Exec(`UPDATE returned_units SET decision='REWORK',final_status='REWORKING',decided_by=$2,decided_at=now(),note=COALESCE(NULLIF($3,''),note) WHERE id=$1`, id, u.Username, req.Note)
		tx.Exec(`UPDATE product_boxes SET status='REWORKING',updated_at=now() WHERE id=$1`, boxID)
	case "DESTROY":
		if finalStatus == "DESTROYED" {
			writeErr(w, 409, "已销毁")
			return
		}
		tx.Exec(`UPDATE returned_units SET decision='DESTROY',final_status='DESTROYED',decided_by=$2,decided_at=now(),handled_at=now(),note=COALESCE(NULLIF($3,''),note) WHERE id=$1`, id, u.Username, req.Note)
		var xid int64
		if tx.QueryRow(`SELECT id FROM storage_locations WHERE code='XH-01'`).Scan(&xid) == nil {
			tx.Exec(`UPDATE product_boxes SET status='DESTROYED',location_id=$2,updated_at=now() WHERE id=$1`, boxID, xid)
		} else {
			tx.Exec(`UPDATE product_boxes SET status='DESTROYED',updated_at=now() WHERE id=$1`, boxID)
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "decision": req.Decision})
}

// retestReturn 复检结论：PASS 可重新入库，FAIL 必须销毁（防止库存状态失真）
func (a *App) retestReturn(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var req struct {
		Result string `json:"result"`
		Note   string `json:"note"`
	}
	if err := decode(r, &req); err != nil || (req.Result != "PASS" && req.Result != "FAIL") {
		writeErr(w, 400, "result 必须为 PASS/FAIL")
		return
	}
	var boxID int64
	var finalStatus string
	if err := a.db.QueryRow(`SELECT box_id,final_status FROM returned_units WHERE id=$1`, id).Scan(&boxID, &finalStatus); err != nil {
		writeErr(w, 404, "退货记录不存在")
		return
	}
	if finalStatus != "RETESTING" {
		writeErr(w, 409, "仅复检中(RETESTING)的退货可录入复检结论，当前: "+finalStatus)
		return
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	if req.Result == "PASS" {
		tx.Exec(`UPDATE returned_units SET retest_result='PASS',final_status='RESTOCK_ELIGIBLE',handled_at=now(),note=COALESCE(NULLIF($2,''),note) WHERE id=$1`, id, req.Note)
		var pbID int64
		var loc sql.NullInt64
		tx.QueryRow(`SELECT batch_id FROM product_boxes WHERE id=$1`, boxID).Scan(&pbID)
		tx.QueryRow(`SELECT cold_location_id FROM product_batches WHERE id=$1`, pbID).Scan(&loc)
		tx.Exec(`UPDATE product_boxes SET status='IN_STOCK',location_id=$2,updated_at=now() WHERE id=$1`, boxID, loc)
	} else {
		tx.Exec(`UPDATE returned_units SET retest_result='FAIL',decision='DESTROY',final_status='DESTROYED',handled_at=now(),note=COALESCE(NULLIF($2,''),note) WHERE id=$1`, id, req.Note)
		tx.Exec(`UPDATE product_boxes SET status='DESTROYED',updated_at=now() WHERE id=$1`, boxID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "retest_result": req.Result})
}

func (a *App) listReturns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := a.db.Query(`SELECT ru.id,COALESCE(rc.recall_no,'') AS recall_no,b.box_no,pb.batch_no AS product_batch_no,
		ru.source,ru.returned_at,ru.received_by,ru.decision,ru.retest_result,ru.final_status,
		ru.decided_by,ru.decided_at,ru.handled_at,ru.note
		FROM returned_units ru
		JOIN product_boxes b ON b.id=ru.box_id
		JOIN product_batches pb ON pb.id=b.batch_id
		LEFT JOIN recalls rc ON rc.id=ru.recall_id
		WHERE ($1='' OR ru.final_status=$1) AND ($2='' OR ru.source=$2) AND ($3='' OR rc.recall_no=$3)
		ORDER BY ru.id DESC`, q.Get("status"), q.Get("source"), q.Get("recallNo"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

// returnsSummary 退货处置汇总，确保三态清晰、库存不失真
func (a *App) returnsSummary(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT decision,final_status,retest_result,COUNT(*) AS boxes,COALESCE(SUM(b.weight_kg),0) AS kg
		FROM returned_units ru JOIN product_boxes b ON b.id=ru.box_id
		GROUP BY decision,final_status,retest_result ORDER BY decision,final_status`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := scanRows(rows)

	// 库存状态一致性核对：处于处置中的箱必须与 returned_units 对应
	mismatchRows, _ := a.db.Query(`SELECT b.box_no,b.status FROM product_boxes b
		WHERE b.status IN ('RETURN_HOLD','RETESTING','REWORKING')
		AND NOT EXISTS(SELECT 1 FROM returned_units ru WHERE ru.box_id=b.id AND ru.final_status IN ('OPEN','RETESTING','REWORKING'))`)
	mismatch := scanRows(mismatchRows)
	mismatchRows.Close()

	writeJSON(w, 200, map[string]any{
		"disposition": items,
		"stock_integrity": map[string]any{
			"orphan_in_disposition": mismatch,
			"consistent":            len(mismatch) == 0,
		},
	})
}
