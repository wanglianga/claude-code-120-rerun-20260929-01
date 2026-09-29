package main

import (
	"database/sql"
	"net/http"
)

// ---------- 客服赔付 ----------

type compReq struct {
	RecallNo    string  `json:"recall_no"`
	OrderNo     string  `json:"order_no"`
	ChannelCode string  `json:"channel_code"`
	Amount      float64 `json:"amount"`
	Kind        string  `json:"kind"` // REFND / COMPENSATION
	Note        string  `json:"note"`
}

func (s *Server) createCompensation(w http.ResponseWriter, r *http.Request) {
	var req compReq
	if !decode(w, r, &req) || req.Amount < 0 {
		errJSON(w, 400, "amount 必填且不能为负")
		return
	}
	if req.Kind == "" {
		req.Kind = "COMPENSATION"
	}
	if req.Kind != "REFUND" && req.Kind != "COMPENSATION" {
		errJSON(w, 400, "kind 必须为 REFUND/COMPENSATION")
		return
	}
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	var recallID, orderID, channelID sql.NullInt64
	if req.RecallNo != "" {
		id, e := lookupID(tx, `SELECT id FROM recalls WHERE recall_no=$1`, req.RecallNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		recallID = nullInt64(id)
	}
	if req.OrderNo != "" {
		id, e := lookupID(tx, `SELECT id FROM customer_orders WHERE order_no=$1`, req.OrderNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		orderID = nullInt64(id)
		var ch int64
		tx.QueryRow(`SELECT channel_id FROM customer_orders WHERE id=$1`, id).Scan(&ch)
		channelID = nullInt64(ch)
	}
	if req.ChannelCode != "" {
		id, e := lookupID(tx, `SELECT id FROM channels WHERE code=$1`, req.ChannelCode)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		channelID = nullInt64(id)
	}
	no, _ := s.nextDocNo("CP")
	var id int64
	if err := tx.QueryRow(`INSERT INTO compensation_records
		(comp_no,recall_id,order_id,channel_id,amount,kind,note)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		no, recallID, orderID, channelID, req.Amount, req.Kind, nullString(req.Note)).Scan(&id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "comp_no": no, "status": "proposed", "amount": req.Amount})
}

func (s *Server) payCompensation(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	var status string
	var orderID sql.NullInt64
	if err := tx.QueryRow(`SELECT status,order_id FROM compensation_records WHERE id=$1 FOR UPDATE`,
		id).Scan(&status, &orderID); err != nil {
		errJSON(w, 404, "赔付记录不存在")
		return
	}
	if status == "paid" {
		errJSON(w, 409, "赔付已支付")
		return
	}
	tx.Exec(`UPDATE compensation_records SET status='paid',processed_at=now() WHERE id=$1`, id)
	if orderID.Valid {
		tx.Exec(`UPDATE customer_orders SET status='compensated' WHERE id=$1
			AND status IN ('recall_notified','returned','refunded')`, orderID.Int64)
	}
	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "paid"})
}

func (s *Server) listCompensations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT cp.id,cp.comp_no,COALESCE(rc.recall_no,'') AS recall_no,
		COALESCE(o.order_no,'') AS order_no,COALESCE(c.code,'') AS channel_code,
		cp.amount,cp.kind,cp.status,cp.note,cp.created_at,cp.processed_at
		FROM compensation_records cp
		LEFT JOIN recalls rc ON rc.id=cp.recall_id
		LEFT JOIN customer_orders o ON o.id=cp.order_id
		LEFT JOIN channels c ON c.id=cp.channel_id
		ORDER BY cp.id DESC LIMIT 500`)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := scanMaps(rows)
	var total float64
	for _, it := range items {
		total += toFloat(it["amount"])
	}
	writeJSON(w, 200, map[string]any{"items": items, "total_amount": total, "count": len(items)})
}

// ---------- 供应商扣款 / 追偿 ----------

type deductionReq struct {
	SupplierCode string  `json:"supplier_code"`
	RecallNo     string  `json:"recall_no"`
	RawBatchNo   string  `json:"raw_batch_no"`
	Amount       float64 `json:"amount"`
	Reason       string  `json:"reason"`
}

func (s *Server) createDeduction(w http.ResponseWriter, r *http.Request) {
	var req deductionReq
	if !decode(w, r, &req) || req.SupplierCode == "" || req.Amount < 0 || req.Reason == "" {
		errJSON(w, 400, "supplier_code/amount/reason 必填")
		return
	}
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	supID, e := lookupID(tx, `SELECT id FROM suppliers WHERE code=$1`, req.SupplierCode)
	if e != nil {
		errJSON(w, 400, e.Error())
		return
	}
	var recallID, rawID sql.NullInt64
	if req.RecallNo != "" {
		id, e := lookupID(tx, `SELECT id FROM recalls WHERE recall_no=$1`, req.RecallNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		recallID = nullInt64(id)
	}
	if req.RawBatchNo != "" {
		id, e := lookupID(tx, `SELECT id FROM raw_batches WHERE batch_no=$1`, req.RawBatchNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		rawID = nullInt64(id)
	}
	no, _ := s.nextDocNo("SD")
	var id int64
	if err := tx.QueryRow(`INSERT INTO supplier_deductions
		(deduct_no,supplier_id,recall_id,raw_batch_id,amount,reason)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		no, supID, recallID, rawID, req.Amount, req.Reason).Scan(&id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "deduct_no": no, "status": "proposed", "amount": req.Amount})
}

func (s *Server) settleDeduction(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM supplier_deductions WHERE id=$1 FOR UPDATE`,
		id).Scan(&status); err != nil {
		errJSON(w, 404, "扣款记录不存在")
		return
	}
	if status == "settled" {
		errJSON(w, 409, "扣款已结算")
		return
	}
	tx.Exec(`UPDATE supplier_deductions SET status='settled',processed_at=now() WHERE id=$1`, id)
	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "settled"})
}

func (s *Server) listDeductions(w http.ResponseWriter, r *http.Request) {
	q := `SELECT sd.id,sd.deduct_no,s.code AS supplier_code,s.name AS supplier_name,
		COALESCE(rc.recall_no,'') AS recall_no,COALESCE(rb.batch_no,'') AS raw_batch_no,
		sd.amount,sd.reason,sd.status,sd.created_at,sd.processed_at
		FROM supplier_deductions sd
		JOIN suppliers s ON s.id=sd.supplier_id
		LEFT JOIN recalls rc ON rc.id=sd.recall_id
		LEFT JOIN raw_batches rb ON rb.id=sd.raw_batch_id
		ORDER BY sd.id DESC LIMIT 500`
	rows, err := s.db.Query(q)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := scanMaps(rows)
	var proposed, settled float64
	for _, it := range items {
		if it["status"] == "settled" {
			settled += toFloat(it["amount"])
		} else {
			proposed += toFloat(it["amount"])
		}
	}
	writeJSON(w, 200, map[string]any{
		"items": items, "proposed_amount": proposed, "settled_amount": settled, "count": len(items),
	})
}
