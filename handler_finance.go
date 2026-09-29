package main

import (
	"database/sql"
	"net/http"
)

// ---- 供应商索赔与扣款追偿 ----

func (a *App) createSupplierClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SupplierCode string  `json:"supplier_code"`
		RecallNo     string  `json:"recall_no"`
		RawBatchNo   string  `json:"raw_batch_no"`
		Reason       string  `json:"reason"`
		ClaimAmount  float64 `json:"claim_amount"`
	}
	if err := decode(r, &req); err != nil || req.SupplierCode == "" || req.Reason == "" || req.ClaimAmount <= 0 {
		writeErr(w, 400, "supplier_code/reason/claim_amount 必填")
		return
	}
	var supplierID int64
	if err := a.db.QueryRow(`SELECT id FROM suppliers WHERE code=$1`, req.SupplierCode).Scan(&supplierID); err != nil {
		writeErr(w, 400, "供应商不存在: "+req.SupplierCode)
		return
	}
	var recallID sql.NullInt64
	if req.RecallNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM recalls WHERE recall_no=$1`, req.RecallNo).Scan(&recallID); err != nil {
			writeErr(w, 400, "召回单不存在: "+req.RecallNo)
			return
		}
	}
	var rawID sql.NullInt64
	if req.RawBatchNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM raw_material_batches WHERE batch_no=$1`, req.RawBatchNo).Scan(&rawID); err != nil {
			writeErr(w, 400, "原料批次不存在: "+req.RawBatchNo)
			return
		}
	}
	claimNo, err := a.nextDocNo(r.Context(), "CL")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	var id int64
	err = a.db.QueryRow(`INSERT INTO supplier_claims
		(claim_no,supplier_id,recall_id,raw_batch_id,reason,claim_amount,issued_by)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		claimNo, supplierID, recallID, rawID, req.Reason, req.ClaimAmount, u.Username).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "claim_no": claimNo, "claim_amount": req.ClaimAmount, "status": "ISSUED"})
}

func (a *App) listSupplierClaims(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := a.db.Query(`SELECT cl.id,cl.claim_no,cl.supplier_id,s.code AS supplier_code,s.name AS supplier_name,
		COALESCE(rc.recall_no,'') AS recall_no,COALESCE(rb.batch_no,'') AS raw_batch_no,
		cl.reason,cl.claim_amount,cl.deduct_amount,cl.status,cl.payment_ref,
		cl.issued_by,cl.issued_at,cl.deducted_at
		FROM supplier_claims cl
		JOIN suppliers s ON s.id=cl.supplier_id
		LEFT JOIN recalls rc ON rc.id=cl.recall_id
		LEFT JOIN raw_material_batches rb ON rb.id=cl.raw_batch_id
		WHERE ($1='' OR cl.status=$1) AND ($2='' OR s.code=$2) AND ($3='' OR rc.recall_no=$3)
		ORDER BY cl.id DESC`, q.Get("status"), q.Get("supplierCode"), q.Get("recallNo"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

// deductSupplierClaim 财务执行供应商扣款，并可标记供应商黑名单/不合格
func (a *App) deductSupplierClaim(w http.ResponseWriter, r *http.Request) {
	claimNo := r.PathValue("claimNo")
	var req struct {
		DeductAmount float64 `json:"deduct_amount"`
		PaymentRef   string  `json:"payment_ref"`
		Blacklist    bool    `json:"blacklist_supplier"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求体格式错误: "+err.Error())
		return
	}
	var id, supplierID int64
	var status string
	var claimAmount float64
	if err := a.db.QueryRow(`SELECT id,supplier_id,status,claim_amount FROM supplier_claims WHERE claim_no=$1`,
		claimNo).Scan(&id, &supplierID, &status, &claimAmount); err != nil {
		writeErr(w, 404, "索赔单不存在")
		return
	}
	if status == "DEDUCTED" || status == "CLOSED" {
		writeErr(w, 409, "索赔单已扣款/关闭")
		return
	}
	deduct := req.DeductAmount
	if deduct <= 0 {
		deduct = claimAmount
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE supplier_claims SET deduct_amount=$2,status='DEDUCTED',payment_ref=$3,deducted_at=now() WHERE id=$1`,
		id, deduct, nz(req.PaymentRef, "-")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if req.Blacklist {
		tx.Exec(`UPDATE suppliers SET blacklisted=TRUE,qualified=FALSE WHERE id=$1`, supplierID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"claim_no": claimNo, "status": "DEDUCTED", "deduct_amount": deduct})
}

// ---- 客服赔付 ----

func (a *App) createCompensation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RecallNo string  `json:"recall_no"`
		OrderNo  string  `json:"order_no"`
		Amount   float64 `json:"amount_cny"`
		Reason   string  `json:"reason"`
	}
	if err := decode(r, &req); err != nil || req.OrderNo == "" || req.Amount <= 0 {
		writeErr(w, 400, "order_no/amount_cny 必填")
		return
	}
	var orderID, channelID int64
	if err := a.db.QueryRow(`SELECT id,channel_id FROM sales_orders WHERE order_no=$1`, req.OrderNo).Scan(&orderID, &channelID); err != nil {
		writeErr(w, 400, "订单不存在: "+req.OrderNo)
		return
	}
	var recallID sql.NullInt64
	if req.RecallNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM recalls WHERE recall_no=$1`, req.RecallNo).Scan(&recallID); err != nil {
			writeErr(w, 400, "召回单不存在: "+req.RecallNo)
			return
		}
	}
	compNo, err := a.nextDocNo(r.Context(), "CP")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	var id int64
	err = a.db.QueryRow(`INSERT INTO customer_compensations(comp_no,recall_id,order_id,channel_id,amount_cny,reason,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		compNo, recallID, orderID, channelID, req.Amount, nz(req.Reason, "召回产品退款赔付"), u.Username).Scan(&id)
	if err != nil {
		writeErr(w, 409, "创建赔付失败（该订单可能已存在赔付）: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "comp_no": compNo, "amount_cny": req.Amount, "status": "PROPOSED"})
}

func (a *App) listCompensations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := a.db.Query(`SELECT cp.id,cp.comp_no,COALESCE(rc.recall_no,'') AS recall_no,o.order_no,
		cp.channel_id,ch.code AS channel_code,ch.name AS channel_name,
		cp.amount_cny,cp.reason,cp.status,cp.created_by,cp.created_at,cp.paid_at
		FROM customer_compensations cp
		JOIN sales_orders o ON o.id=cp.order_id
		JOIN channels ch ON ch.id=cp.channel_id
		LEFT JOIN recalls rc ON rc.id=cp.recall_id
		WHERE ($1='' OR cp.status=$1) AND ($2='' OR rc.recall_no=$2)
		ORDER BY cp.id DESC`, q.Get("status"), q.Get("recallNo"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) payCompensation(w http.ResponseWriter, r *http.Request) {
	compNo := r.PathValue("compNo")
	var id, orderID int64
	var status string
	if err := a.db.QueryRow(`SELECT id,order_id,status FROM customer_compensations WHERE comp_no=$1`, compNo).Scan(&id, &orderID, &status); err != nil {
		writeErr(w, 404, "赔付单不存在")
		return
	}
	if status != "PROPOSED" {
		writeErr(w, 409, "赔付单状态: "+status)
		return
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	tx.Exec(`UPDATE customer_compensations SET status='PAID',paid_at=now() WHERE id=$1`, id)
	tx.Exec(`UPDATE sales_orders SET status='COMPENSATED' WHERE id=$1 AND status='RECALLED'`, orderID)
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"comp_no": compNo, "status": "PAID"})
}

// recallCost 召回成本汇总：赔付 + 供应商扣款回收 + 销毁处置成本
func (a *App) recallCost(w http.ResponseWriter, r *http.Request) {
	recallNo := r.PathValue("recallNo")
	var recallID int64
	if err := a.db.QueryRow(`SELECT id FROM recalls WHERE recall_no=$1`, recallNo).Scan(&recallID); err != nil {
		writeErr(w, 404, "召回单不存在")
		return
	}
	var compTotal, compPaid, claimTotal, deducted sql.NullFloat64
	a.db.QueryRow(`SELECT COALESCE(SUM(amount_cny),0),COALESCE(SUM(amount_cny) FILTER(WHERE status='PAID'),0)
		FROM customer_compensations WHERE recall_id=$1`, recallID).Scan(&compTotal, &compPaid)
	a.db.QueryRow(`SELECT COALESCE(SUM(claim_amount),0),COALESCE(SUM(deduct_amount),0)
		FROM supplier_claims WHERE recall_id=$1`, recallID).Scan(&claimTotal, &deducted)

	var destroyBoxes int
	var unitCost float64
	a.db.QueryRow(`SELECT value::float8 FROM system_config WHERE key='destroy_unit_cost_cny'`).Scan(&unitCost)
	a.db.QueryRow(`SELECT COUNT(*) FROM returned_units WHERE recall_id=$1 AND final_status='DESTROYED'`, recallID).Scan(&destroyBoxes)
	destroyCost := float64(destroyBoxes) * unitCost

	netLoss := compTotal.Float64 + destroyCost - deducted.Float64
	writeJSON(w, 200, map[string]any{
		"recall_no":                      recallNo,
		"customer_compensation_proposed": compTotal.Float64,
		"customer_compensation_paid":     compPaid.Float64,
		"supplier_claim_issued":          claimTotal.Float64,
		"supplier_deducted_recovered":    deducted.Float64,
		"destroyed_boxes":                destroyBoxes,
		"destroy_cost":                   destroyCost,
		"estimated_net_loss":             netLoss,
	})
}
