package main

import (
	"database/sql"
	"net/http"
)

// ---------- 品控事件 ----------

type qualityEventReq struct {
	Type             string `json:"type"`
	Severity         string `json:"severity"`
	RawBatchNo       string `json:"raw_batch_no"`
	FinishedBatchNo  string `json:"finished_batch_no"`
	WorkOrderNo      string `json:"work_order_no"`
	ChannelCode      string `json:"channel_code"`
	Description      string `json:"description"`
}

var validEventTypes = map[string]bool{
	"RAW_TEMP_OVER": true, "THAW_TIMEOUT": true, "THAW_TEMP_OVER": true,
	"SEASONING_WRONG": true, "METAL_ALARM": true, "PACK_LEAK": true,
	"SAMPLE_UNQUALIFIED": true, "CHANNEL_ODOR": true, "LIVE_RETURN": true,
	"REGULATOR_RECALL": true, "OTHER": true,
}

func (s *Server) createQualityEvent(w http.ResponseWriter, r *http.Request) {
	var req qualityEventReq
	if !decode(w, r, &req) || !validEventTypes[req.Type] || req.Description == "" {
		errJSON(w, 400, "type(合法枚举) 与 description 必填")
		return
	}
	sev := req.Severity
	if sev == "" {
		sev = "major"
	}
	if sev != "minor" && sev != "major" && sev != "critical" {
		errJSON(w, 400, "severity 必须为 minor/major/critical")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var rawID, fgID, woID, chID sql.NullInt64
	if req.RawBatchNo != "" {
		id, e := lookupID(tx, `SELECT id FROM raw_batches WHERE batch_no=$1`, req.RawBatchNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		rawID = nullInt64(id)
	}
	if req.FinishedBatchNo != "" {
		id, e := lookupID(tx, `SELECT id FROM finished_batches WHERE batch_no=$1`, req.FinishedBatchNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		fgID = nullInt64(id)
	}
	if req.WorkOrderNo != "" {
		id, e := lookupID(tx, `SELECT id FROM work_orders WHERE order_no=$1`, req.WorkOrderNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		woID = nullInt64(id)
	}
	if req.ChannelCode != "" {
		id, e := lookupID(tx, `SELECT id FROM channels WHERE code=$1`, req.ChannelCode)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		chID = nullInt64(id)
	}
	no, _ := s.nextDocNo("QE")
	var id int64
	if err = tx.QueryRow(`INSERT INTO quality_events
		(event_no,type,severity,source,raw_batch_id,finished_batch_id,work_order_id,channel_id,
		 description,status,created_by)
		VALUES ($1,$2,$3,'manual',$4,$5,$6,$7,$8,'open',$9) RETURNING id`,
		no, req.Type, sev, rawID, fgID, woID, chID, req.Description,
		currentUser(r).Username).Scan(&id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "event_no": no, "type": req.Type, "severity": sev})
}

func (s *Server) listQualityEvents(w http.ResponseWriter, r *http.Request) {
	q := `SELECT q.id,q.event_no,q.type,q.severity,q.source,q.description,q.qc_conclusion,
		q.status,q.created_at,q.created_by,
		COALESCE(rb.batch_no,'') AS raw_batch_no,
		COALESCE(fb.batch_no,'') AS finished_batch_no,
		COALESCE(w.order_no,'') AS work_order_no,
		COALESCE(c.code,'') AS channel_code
		FROM quality_events q
		LEFT JOIN raw_batches rb ON rb.id=q.raw_batch_id
		LEFT JOIN finished_batches fb ON fb.id=q.finished_batch_id
		LEFT JOIN work_orders w ON w.id=q.work_order_id
		LEFT JOIN channels c ON c.id=q.channel_id
		ORDER BY q.id DESC LIMIT 500`
	rows, err := s.db.Query(q)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

type conclusionReq struct {
	Conclusion string `json:"conclusion"`
	Confirmed  bool   `json:"confirmed"` // 品控是否确认问题成立
}

// concludeQualityEvent 品控结论：确认成立可触发工单解冻放行或维持冻结
func (s *Server) concludeQualityEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var req conclusionReq
	if !decode(w, r, &req) || req.Conclusion == "" {
		errJSON(w, 400, "conclusion 必填")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var woID sql.NullInt64
	var status string
	if err = tx.QueryRow(`SELECT work_order_id,status FROM quality_events WHERE id=$1 FOR UPDATE`,
		id).Scan(&woID, &status); err != nil {
		errJSON(w, 404, "品控事件不存在")
		return
	}
	newStatus := "closed"
	if req.Confirmed {
		newStatus = "confirmed"
	}
	if _, err = tx.Exec(`UPDATE quality_events SET qc_conclusion=$1,status=$2 WHERE id=$3`,
		req.Conclusion, newStatus, id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	// 未确认成立 → 若关联工单被冻结则放行回生产中
	if !req.Confirmed && woID.Valid {
		tx.Exec(`UPDATE work_orders SET status='in_production' WHERE id=$1 AND status='on_hold'`,
			woID.Int64)
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": newStatus,
		"work_order_released": !req.Confirmed && woID.Valid})
}

// ---------- 渠道反馈（异味 / 直播间集中退货） ----------

type feedbackReq struct {
	ChannelCode      string `json:"channel_code"`
	FinishedBatchNo  string `json:"finished_batch_no"`
	OrderNo          string `json:"order_no"`
	FeedbackType     string `json:"feedback_type"`
	Content          string `json:"content"`
}

func (s *Server) createFeedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackReq
	if !decode(w, r, &req) || req.ChannelCode == "" || req.Content == "" ||
		(req.FeedbackType != "ODOR" && req.FeedbackType != "CONCENTRATED_RETURN" &&
			req.FeedbackType != "QUALITY_COMPLAINT" && req.FeedbackType != "OTHER") {
		errJSON(w, 400, "channel_code/content 与合法 feedback_type 必填")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	chID, e := lookupID(tx, `SELECT id FROM channels WHERE code=$1`, req.ChannelCode)
	if e != nil {
		errJSON(w, 400, e.Error())
		return
	}
	var fgID, orderID sql.NullInt64
	if req.FinishedBatchNo != "" {
		id, e := lookupID(tx, `SELECT id FROM finished_batches WHERE batch_no=$1`, req.FinishedBatchNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		fgID = nullInt64(id)
	}
	if req.OrderNo != "" {
		id, e := lookupID(tx, `SELECT id FROM customer_orders WHERE order_no=$1`, req.OrderNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		orderID = nullInt64(id)
	}
	// 反馈自动登记品控事件：异味→CHANNEL_ODOR；集中退货→LIVE_RETURN
	evType := map[string]string{
		"ODOR": "CHANNEL_ODOR", "CONCENTRATED_RETURN": "LIVE_RETURN",
		"QUALITY_COMPLAINT": "OTHER", "OTHER": "OTHER",
	}[req.FeedbackType]
	en, _ := s.nextDocNo("QE")
	var evID int64
	desc := "渠道[" + req.ChannelCode + "]反馈：" + req.Content
	if err = tx.QueryRow(`INSERT INTO quality_events
		(event_no,type,severity,source,finished_batch_id,channel_id,description,status,created_by)
		VALUES ($1,$2,'major','channel',$3,$4,$5,'open',$6) RETURNING id`,
		en, evType, fgID, nullInt64(chID), desc, currentUser(r).Username).Scan(&evID); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	var fbIDReturn int64
	if err = tx.QueryRow(`INSERT INTO channel_feedback
		(channel_id,finished_batch_id,order_id,feedback_type,content,quality_event_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		chID, fgID, orderID, req.FeedbackType, req.Content, evID).Scan(&fbIDReturn); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"id": fbIDReturn, "quality_event_no": en, "event_type": evType,
		"message": "渠道反馈已登记品控事件，可据此发起召回",
	})
}

// ---------- 监管抽样 ----------

type regSampleReq struct {
	Regulator       string `json:"regulator"`
	RawBatchNo      string `json:"raw_batch_no"`
	FinishedBatchNo string `json:"finished_batch_no"`
}

func (s *Server) createRegulatorySample(w http.ResponseWriter, r *http.Request) {
	var req regSampleReq
	if !decode(w, r, &req) || req.Regulator == "" ||
		(req.RawBatchNo == "" && req.FinishedBatchNo == "") {
		errJSON(w, 400, "regulator 与 raw_batch_no/finished_batch_no(至少其一) 必填")
		return
	}
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	var rawID, fgID sql.NullInt64
	if req.RawBatchNo != "" {
		id, e := lookupID(tx, `SELECT id FROM raw_batches WHERE batch_no=$1`, req.RawBatchNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		rawID = nullInt64(id)
		tx.Exec(`UPDATE raw_boxes SET status='sampled' WHERE raw_batch_id=$1 AND status='in_storage'
			AND id=(SELECT MIN(id) FROM raw_boxes WHERE raw_batch_id=$1)`, id)
	}
	if req.FinishedBatchNo != "" {
		id, e := lookupID(tx, `SELECT id FROM finished_batches WHERE batch_no=$1`, req.FinishedBatchNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		fgID = nullInt64(id)
	}
	no, _ := s.nextDocNo("RS")
	var id int64
	if err := tx.QueryRow(`INSERT INTO regulatory_samples
		(sample_no,regulator,raw_batch_id,finished_batch_id)
		VALUES ($1,$2,$3,$4) RETURNING id`, no, req.Regulator, rawID, fgID).Scan(&id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "sample_no": no, "result": "pending"})
}

type regResultReq struct {
	Result     string `json:"result"`
	ReportNo   string `json:"report_no"`
	Conclusion string `json:"conclusion"`
}

// resultRegulatorySample 监管抽检不合格 → 自动生成监管要求召回归控事件
func (s *Server) resultRegulatorySample(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var req regResultReq
	if !decode(w, r, &req) || (req.Result != "qualified" && req.Result != "unqualified") {
		errJSON(w, 400, "result 必须为 qualified/unqualified")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var rawID, fgID sql.NullInt64
	var cur, regulator string
	if err = tx.QueryRow(`SELECT result,regulator,raw_batch_id,finished_batch_id
		FROM regulatory_samples WHERE id=$1 FOR UPDATE`, id).
		Scan(&cur, &regulator, &rawID, &fgID); err != nil {
		errJSON(w, 404, "监管抽样不存在")
		return
	}
	if cur != "pending" {
		errJSON(w, 409, "抽样已出结论")
		return
	}
	tx.Exec(`UPDATE regulatory_samples SET result=$1,report_no=$2,conclusion=$3,resulted_at=now()
		WHERE id=$4`, req.Result, nullString(req.ReportNo), nullString(req.Conclusion), id)
	alerts := []map[string]any{}
	if req.Result == "unqualified" {
		en, _ := s.nextDocNo("QE")
		tx.Exec(`INSERT INTO quality_events
			(event_no,type,severity,source,raw_batch_id,finished_batch_id,description,status,created_by)
			VALUES ($1,'REGULATOR_RECALL','critical','regulator',$2,$3,$4,'open',$5)`,
			en, rawID, fgID,
			"监管机构["+regulator+"]抽检不合格"+cPrefix(req.Conclusion)+"，监管要求召回",
			currentUser(r).Username)
		alerts = append(alerts, map[string]any{
			"type": "REGULATOR_RECALL", "event_no": en, "message": "已生成监管要求召回事件",
		})
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "result": req.Result, "alerts": alerts})
}
