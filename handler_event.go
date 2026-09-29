package main

import (
	"database/sql"
	"net/http"
)

func (a *App) createQualityEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type           string `json:"type"`
		Severity       string `json:"severity"`
		RawBatchNo     string `json:"raw_batch_no"`
		ProductBatchNo string `json:"product_batch_no"`
		BoxNo          string `json:"box_no"`
		FeedbackID     int64  `json:"feedback_id"`
		Description    string `json:"description"`
	}
	if err := decode(r, &req); err != nil || req.Type == "" || req.Description == "" {
		writeErr(w, 400, "type/description 必填")
		return
	}
	valid := map[string]bool{
		"TEMP_EXCEEDED": true, "THAW_TIMEOUT": true, "SEASONING_MISMATCH": true,
		"METAL_ALARM": true, "PACKAGE_LEAK": true, "OFF_ODOR": true,
		"LIVESTREAM_BATCH_RETURN": true, "REGULATOR_RECALL": true, "OTHER": true,
	}
	if !valid[req.Type] {
		writeErr(w, 400, "type 取值非法")
		return
	}
	sev := req.Severity
	if sev == "" {
		sev = "MEDIUM"
	}
	if sev != "LOW" && sev != "MEDIUM" && sev != "HIGH" {
		writeErr(w, 400, "severity 必须为 LOW/MEDIUM/HIGH")
		return
	}
	var rawID, pbID sql.NullInt64
	if req.RawBatchNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM raw_material_batches WHERE batch_no=$1`, req.RawBatchNo).Scan(&rawID); err != nil {
			writeErr(w, 400, "原料批次不存在: "+req.RawBatchNo)
			return
		}
	}
	if req.ProductBatchNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM product_batches WHERE batch_no=$1`, req.ProductBatchNo).Scan(&pbID); err != nil {
			writeErr(w, 400, "成品批次不存在: "+req.ProductBatchNo)
			return
		}
	}
	en, err := a.nextDocNo(r.Context(), "QE")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	var id int64
	err = a.db.QueryRow(`INSERT INTO quality_events
		(event_no,type,severity,raw_batch_id,product_batch_id,box_no,feedback_id,description,raised_by)
		VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,0),$8,$9) RETURNING id`,
		en, req.Type, sev, rawID, pbID, nz(req.BoxNo, ""), req.FeedbackID, req.Description, u.Username).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "event_no": en, "status": "OPEN"})
}

func (a *App) listQualityEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := a.db.Query(`SELECT qe.id,qe.event_no,qe.type,qe.severity,
		qe.raw_batch_id,rb.batch_no AS raw_batch_no,
		qe.product_batch_id,pb.batch_no AS product_batch_no,
		qe.box_no,qe.description,qe.status,qe.raised_by,qe.raised_at,
		qe.confirmed_by,qe.confirmed_at,qe.conclusion,qe.recall_id,rc.recall_no
		FROM quality_events qe
		LEFT JOIN raw_material_batches rb ON rb.id=qe.raw_batch_id
		LEFT JOIN product_batches pb ON pb.id=qe.product_batch_id
		LEFT JOIN recalls rc ON rc.id=qe.recall_id
		WHERE ($1='' OR qe.status=$1) AND ($2='' OR qe.type=$2)
		  AND ($3=0 OR qe.raw_batch_id=$3) AND ($4=0 OR qe.product_batch_id=$4)
		ORDER BY qe.id DESC`,
		q.Get("status"), q.Get("type"), atoi64(q.Get("rawBatchId")), atoi64(q.Get("productBatchId")))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

// confirmQualityEvent 品控确认/驳回；CONFIRMED 后可作为召回触发事件
func (a *App) confirmQualityEvent(w http.ResponseWriter, r *http.Request) {
	eventNo := r.PathValue("eventNo")
	var req struct {
		Confirm    bool   `json:"confirm"`
		Conclusion string `json:"conclusion"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求体格式错误: "+err.Error())
		return
	}
	var id int64
	var status string
	if err := a.db.QueryRow(`SELECT id,status FROM quality_events WHERE event_no=$1`, eventNo).Scan(&id, &status); err != nil {
		writeErr(w, 404, "质量事件不存在")
		return
	}
	if status != "OPEN" {
		writeErr(w, 409, "事件已处理: "+status)
		return
	}
	u := currentUser(r)
	newStatus := "CONFIRMED"
	if !req.Confirm {
		newStatus = "DISMISSED"
	}
	if _, err := a.db.Exec(`UPDATE quality_events SET status=$2,confirmed_by=$3,confirmed_at=now(),conclusion=$4 WHERE id=$1`,
		id, newStatus, u.Username, req.Conclusion); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 驳回时解除原料批次冻结
	if !req.Confirm {
		a.db.Exec(`UPDATE raw_material_batches SET status='FROZEN_IN'
			WHERE id=(SELECT raw_batch_id FROM quality_events WHERE id=$1) AND status='ON_HOLD'`, id)
	}
	writeJSON(w, 200, map[string]any{"event_no": eventNo, "status": newStatus})
}
