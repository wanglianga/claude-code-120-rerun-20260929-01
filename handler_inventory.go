package main

import (
	"database/sql"
	"net/http"
)

func (s *Server) listStock(w http.ResponseWriter, r *http.Request) {
	q := `SELECT fb.id,fb.batch_no,p.code AS product_code,p.name AS product_name,
		COALESCE(l.code,'') AS location_code,
		fs.qty_on_hand,fs.qty_hold,fs.qty_quarantined,fs.qty_shipped,
		fs.qty_destroyed,fs.qty_reworked,fs.updated_at
		FROM finished_stock fs
		JOIN finished_batches fb ON fb.id=fs.finished_batch_id
		JOIN products p ON p.id=fb.product_id
		LEFT JOIN locations l ON l.id=fs.location_id
		ORDER BY fb.id DESC LIMIT 500`
	rows, err := s.db.Query(q)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := scanMaps(rows)
	var onHand, hold, quarantined, destroyed, reworked float64
	for _, it := range items {
		onHand += toFloat(it["qty_on_hand"])
		hold += toFloat(it["qty_hold"])
		quarantined += toFloat(it["qty_quarantined"])
		destroyed += toFloat(it["qty_destroyed"])
		reworked += toFloat(it["qty_reworked"])
	}
	writeJSON(w, 200, map[string]any{
		"items": items, "count": len(items),
		"totals_kg": map[string]any{
			"on_hand": onHand, "hold": hold, "quarantined": quarantined,
			"destroyed": destroyed, "reworked": reworked,
		},
	})
}

// ---------- 冷库盘点 ----------

type stocktakeReq struct {
	LocationCode string `json:"location_code"`
}

func (s *Server) createStocktake(w http.ResponseWriter, r *http.Request) {
	var req stocktakeReq
	if !decode(w, r, &req) {
		return
	}
	var locID sql.NullInt64
	if req.LocationCode != "" {
		id, err := s.getIDByCode("locations", "code", req.LocationCode)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		locID = nullInt64(id)
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	no, _ := s.nextDocNo("ST")
	var takeID int64
	if err = tx.QueryRow(`INSERT INTO stocktakes(take_no,location_id) VALUES ($1,$2) RETURNING id`,
		no, locID).Scan(&takeID); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	// 账面快照：实物理论可点 = 可售 + 冻结（召回停售品仍在库）
	q := `SELECT fs.finished_batch_id,fs.qty_on_hand+fs.qty_hold
		FROM finished_stock fs`
	args := []any{}
	if locID.Valid {
		q += ` WHERE fs.location_id=$1`
		args = append(args, locID.Int64)
	}
	rows, e := tx.Query(q, args...)
	if e != nil {
		errJSON(w, 500, e.Error())
		return
	}
	type line struct{ fg int64; sys float64 }
	var lines []line
	for rows.Next() {
		var x line
		rows.Scan(&x.fg, &x.sys)
		lines = append(lines, x)
	}
	rows.Close()
	for _, ln := range lines {
		tx.Exec(`INSERT INTO stocktake_items(stocktake_id,finished_batch_id,system_qty)
			VALUES ($1,$2,$3)`, takeID, ln.fg, ln.sys)
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"id": takeID, "take_no": no, "status": "counting", "line_count": len(lines),
		"note": "已冻结账面快照，请对每行 POST count 录入实盘数量",
	})
}

type countReq struct {
	FinishedBatchNo string  `json:"finished_batch_no"`
	CountedQty      float64 `json:"counted_qty"`
}

func (s *Server) recordStocktakeCount(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var req countReq
	if !decode(w, r, &req) || req.FinishedBatchNo == "" {
		errJSON(w, 400, "finished_batch_no 与 counted_qty 必填")
		return
	}
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM stocktakes WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		errJSON(w, 404, "盘点单不存在")
		return
	}
	if status != "counting" {
		errJSON(w, 409, "盘点单状态: " + status)
		return
	}
	fgID, e := lookupID(tx, `SELECT id FROM finished_batches WHERE batch_no=$1`, req.FinishedBatchNo)
	if e != nil {
		errJSON(w, 400, e.Error())
		return
	}
	var sys float64
	if err := tx.QueryRow(`SELECT system_qty FROM stocktake_items
		WHERE stocktake_id=$1 AND finished_batch_id=$2 FOR UPDATE`, id, fgID).Scan(&sys); err != nil {
		errJSON(w, 404, "该批次不在本次盘点范围内")
		return
	}
	tx.Exec(`UPDATE stocktake_items SET counted_qty=$1,diff_qty=$1-system_qty
		WHERE stocktake_id=$2 AND finished_batch_id=$3`, req.CountedQty, id, fgID)
	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"stocktake_id": id, "finished_batch_no": req.FinishedBatchNo,
		"system_qty": sys, "counted_qty": req.CountedQty,
		"diff_qty": req.CountedQty - sys,
	})
}

func (s *Server) confirmStocktake(w http.ResponseWriter, r *http.Request) {
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
	var status string
	if err = tx.QueryRow(`SELECT status FROM stocktakes WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		errJSON(w, 404, "盘点单不存在")
		return
	}
	if status != "counting" {
		errJSON(w, 409, "盘点单状态: " + status)
		return
	}
	itemRows, err := tx.Query(`SELECT si.finished_batch_id,si.system_qty,si.counted_qty,si.diff_qty,
		fb.batch_no FROM stocktake_items si
		JOIN finished_batches fb ON fb.id=si.finished_batch_id
		WHERE si.stocktake_id=$1 ORDER BY si.id`, id)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	type adj struct {
		fg                  int64
		batchNo             string
		sys, counted, diff  float64
	}
	var adjs []adj
	uncounted := 0
	for itemRows.Next() {
		var a adj
		var counted sql.NullFloat64
		var diff sql.NullFloat64
		itemRows.Scan(&a.fg, &a.sys, &counted, &diff, &a.batchNo)
		if !counted.Valid {
			uncounted++
			continue
		}
		a.counted = counted.Float64
		a.diff = diff.Float64
		adjs = append(adjs, a)
	}
	itemRows.Close()
	if uncounted > 0 {
		errJSON(w, 409, "仍有未录入实盘的行，不能确认")
		return
	}
	var totalDiff float64
	for _, a := range adjs {
		// 盘差调整作用于可售分量（冻结量是召回状态，不允许通过盘点冲销）
		tx.Exec(`UPDATE finished_stock SET qty_on_hand=GREATEST(qty_on_hand+$1,0),updated_at=now()
			WHERE finished_batch_id=$2`, a.diff, a.fg)
		var after float64
		tx.QueryRow(`SELECT qty_on_hand FROM finished_stock WHERE finished_batch_id=$1`, a.fg).Scan(&after)
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,$3,'STOCKTAKE_ADJ',$4)`, a.fg, a.diff, after, a.batchNo)
		totalDiff += a.diff
	}
	tx.Exec(`UPDATE stocktakes SET status='confirmed',counted_at=now() WHERE id=$1`, id)
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"stocktake_id": id, "status": "confirmed",
		"adjusted_lines": len(adjs), "total_diff_kg": totalDiff,
	})
}
