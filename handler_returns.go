package main

import (
	"database/sql"
	"net/http"
)

type returnReq struct {
	RecallNo        string  `json:"recall_no"`
	FinishedBatchNo string  `json:"finished_batch_no"`
	ChannelCode     string  `json:"channel_code"`
	OrderNo         string  `json:"order_no"`
	QtyKg           float64 `json:"qty_kg"`
	Reason          string  `json:"reason"`
}

// createReturn 退货回厂：从已发量冲回，进入隔离待处置区（不计入可售，避免库存状态失真）
func (s *Server) createReturn(w http.ResponseWriter, r *http.Request) {
	var req returnReq
	if !decode(w, r, &req) || req.FinishedBatchNo == "" || req.QtyKg <= 0 {
		errJSON(w, 400, "finished_batch_no 与 qty_kg(>0) 必填")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var fgID int64
	if err = tx.QueryRow(`SELECT id FROM finished_batches WHERE batch_no=$1`,
		req.FinishedBatchNo).Scan(&fgID); err != nil {
		errJSON(w, 400, "成品批次不存在")
		return
	}
	var recallID, channelID, orderID sql.NullInt64
	if req.RecallNo != "" {
		id, e := lookupID(tx, `SELECT id FROM recalls WHERE recall_no=$1`, req.RecallNo)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		recallID = nullInt64(id)
	}
	if req.ChannelCode != "" {
		id, e := lookupID(tx, `SELECT id FROM channels WHERE code=$1`, req.ChannelCode)
		if e != nil {
			errJSON(w, 400, e.Error())
			return
		}
		channelID = nullInt64(id)
	}
	if req.OrderNo != "" {
		var oid int64
		var ofg int64
		if err = tx.QueryRow(`SELECT id,finished_batch_id FROM customer_orders WHERE order_no=$1`,
			req.OrderNo).Scan(&oid, &ofg); err != nil {
			errJSON(w, 400, "订单不存在")
			return
		}
		if ofg != fgID {
			errJSON(w, 409, "订单不属于该成品批次")
			return
		}
		orderID = nullInt64(oid)
	}
	// 已发数量校验：累计回厂量 + 本次 <= 已发
	var shipped, already float64
	tx.QueryRow(`SELECT qty_shipped,qty_quarantined FROM finished_stock
		WHERE finished_batch_id=$1 FOR UPDATE`, fgID).Scan(&shipped, &already)
	if already+req.QtyKg > shipped+0.001 {
		errJSON(w, 409, "回厂数量超过该批次已发数量: 已发"+strconvFormat(shipped)+
			"kg，已回厂"+strconvFormat(already)+"kg")
		return
	}
	no, _ := s.nextDocNo("RT")
	var retID int64
	if err = tx.QueryRow(`INSERT INTO return_goods
		(return_no,recall_id,finished_batch_id,channel_id,order_id,qty_kg,status,reason)
		VALUES ($1,$2,$3,$4,$5,$6,'arrived',$7) RETURNING id`,
		no, recallID, fgID, channelID, orderID, req.QtyKg, nullString(req.Reason)).Scan(&retID); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	tx.Exec(`UPDATE finished_stock SET qty_shipped=qty_shipped-$1,qty_quarantined=qty_quarantined+$1,
		updated_at=now() WHERE finished_batch_id=$2`, req.QtyKg, fgID)
	tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
		VALUES ($1,$2,(SELECT qty_quarantined FROM finished_stock WHERE finished_batch_id=$1),
		'RETURN_QUARANTINE_IN',$3)`, fgID, req.QtyKg, no)
	if orderID.Valid {
		tx.Exec(`UPDATE customer_orders SET status='returned' WHERE id=$1 AND status='recall_notified'`,
			orderID.Int64)
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if s.cache != nil {
		s.cache.invalidateTrace(r.Context(), nil, []int64{fgID})
	}
	writeJSON(w, 201, map[string]any{
		"id": retID, "return_no": no, "status": "arrived",
		"note": "退货已入隔离待处置区，等待品控判定：REINSPECT 可复检 / REWORK 可返工 / DESTROY 必须销毁",
	})
}

type dispositionReq struct {
	Disposition string `json:"disposition"`
	Note        string `json:"note"`
}

// decideReturn 品控对回厂退货判定处置路径；DESTROY 与 REWORK 当场执行，REINSPECT 进入待检
func (s *Server) decideReturn(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var req dispositionReq
	if !decode(w, r, &req) ||
		(req.Disposition != "REINSPECT" && req.Disposition != "REWORK" && req.Disposition != "DESTROY") {
		errJSON(w, 400, "disposition 必须为 REINSPECT/REWORK/DESTROY")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var fgID int64
	var qty float64
	var status string
	if err = tx.QueryRow(`SELECT finished_batch_id,qty_kg,status FROM return_goods WHERE id=$1 FOR UPDATE`,
		id).Scan(&fgID, &qty, &status); err != nil {
		errJSON(w, 404, "退货记录不存在")
		return
	}
	if status != "arrived" {
		errJSON(w, 409, "退货已判定处置，当前状态: "+status)
		return
	}
	var batchNo string
	tx.QueryRow(`SELECT batch_no FROM finished_batches WHERE id=$1`, fgID).Scan(&batchNo)

	switch req.Disposition {
	case "REINSPECT":
		tx.Exec(`UPDATE return_goods SET status='reinspect_pending',disposition='REINSPECT',
			reason=COALESCE(reason,'')||$2,decided_at=now() WHERE id=$1`, id, noteSuffix(req.Note))
		if err = tx.Commit(); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "status": "reinspect_pending",
			"next": "调用 POST /returns/{id}/reinspect 录入复检结论"})
		return
	case "DESTROY":
		moveOutOfQuarantine(tx, fgID, qty, batchNo)
		tx.Exec(`UPDATE finished_stock SET qty_destroyed=qty_destroyed+$1 WHERE finished_batch_id=$2`, qty, fgID)
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,(SELECT qty_destroyed FROM finished_stock WHERE finished_batch_id=$1),
			'RETURN_DESTROY_OUT',$3)`, fgID, qty, batchNo)
		tx.Exec(`UPDATE return_goods SET status='destroyed',disposition='DESTROY',
			decided_at=now(),processed_at=now() WHERE id=$1`, id)
	case "REWORK":
		moveOutOfQuarantine(tx, fgID, qty, batchNo)
		tx.Exec(`UPDATE finished_stock SET qty_reworked=qty_reworked+$1 WHERE finished_batch_id=$2`, qty, fgID)
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,(SELECT qty_reworked FROM finished_stock WHERE finished_batch_id=$1),
			'RETURN_REWORK_OUT',$3)`, fgID, qty, batchNo)
		// 返工产出新成品批次（rework_of 溯源链），重新留样待检
		var productID, woID int64
		var packSpec string
		var channel sql.NullInt64
		tx.QueryRow(`SELECT product_id,work_order_id,pack_spec,pack_channel_id FROM finished_batches WHERE id=$1`,
			fgID).Scan(&productID, &woID, &packSpec, &channel)
		newNo, _ := s.nextDocNo("FG")
		var newID int64
		if err = tx.QueryRow(`INSERT INTO finished_batches
			(batch_no,product_id,work_order_id,qty_kg,pack_spec,pack_channel_id,status,rework_of_id)
			VALUES ($1,$2,$3,$4,$5,$6,'produced',$7) RETURNING id`,
			newNo, productID, woID, qty, packSpec, channel, fgID).Scan(&newID); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		var locID sql.NullInt64
		tx.QueryRow(`SELECT location_id FROM finished_stock WHERE finished_batch_id=$1`, fgID).Scan(&locID)
		tx.Exec(`INSERT INTO finished_stock(finished_batch_id,location_id,qty_on_hand) VALUES ($1,$2,$3)`,
			newID, locID, qty)
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,$3,'REWORK_OUTPUT_IN',$4)`, newID, qty, qty, newNo)
		spNo, _ := s.nextDocNo("SP")
		tx.Exec(`INSERT INTO sample_retentions(sample_no,finished_batch_id) VALUES ($1,$2)`, spNo, newID)
		tx.Exec(`UPDATE return_goods SET status='reworked',disposition='REWORK',
			decided_at=now(),processed_at=now() WHERE id=$1`, id)
		tx.Exec(`UPDATE finished_batches SET status='reworked' WHERE id=$1`, fgID)
		defer func() {
			if s.cache != nil {
				s.cache.invalidateTrace(r.Context(), nil, []int64{fgID, newID})
			}
		}()
		writeJSONDefer := func() {
			if err2 := tx.Commit(); err2 != nil {
				errJSON(w, 500, err2.Error())
				return
			}
			writeJSON(w, 200, map[string]any{
				"id": id, "status": "reworked",
				"reworked_from_batch_no": batchNo,
				"new_finished_batch_no":  newNo,
				"new_sample_no":          spNo,
				"note":                   "返工产出新批次已自动留样，合格放行后方可销售",
			})
		}
		writeJSONDefer()
		return
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if s.cache != nil {
		s.cache.invalidateTrace(r.Context(), nil, []int64{fgID})
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "destroyed", "disposition": "DESTROY",
		"destroyed_kg": qty, "batch_no": batchNo})
}

// moveOutOfQuarantine 将退货数量从隔离分量扣出
func moveOutOfQuarantine(tx *sql.Tx, fgID int64, qty float64, _ string) {
	tx.Exec(`UPDATE finished_stock SET qty_quarantined=qty_quarantined-$1,updated_at=now()
		WHERE finished_batch_id=$2`, qty, fgID)
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return "｜判定备注：" + note
}

type reinspectReq struct {
	Pass bool   `json:"pass"`
	Note string `json:"note"`
}

// reinspectReturn 录入复检结论：合格回可售；不合格转销毁
func (s *Server) reinspectReturn(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var req reinspectReq
	if !decode(w, r, &req) {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var fgID int64
	var qty float64
	var status string
	if err = tx.QueryRow(`SELECT finished_batch_id,qty_kg,status FROM return_goods WHERE id=$1 FOR UPDATE`,
		id).Scan(&fgID, &qty, &status); err != nil {
		errJSON(w, 404, "退货记录不存在")
		return
	}
	if status != "reinspect_pending" {
		errJSON(w, 409, "当前退货状态不可复检: "+status)
		return
	}
	var batchNo string
	tx.QueryRow(`SELECT batch_no FROM finished_batches WHERE id=$1`, fgID).Scan(&batchNo)
	if req.Pass {
		tx.Exec(`UPDATE finished_stock SET qty_quarantined=qty_quarantined-$1,qty_on_hand=qty_on_hand+$1,
			updated_at=now() WHERE finished_batch_id=$2`, qty, fgID)
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,(SELECT qty_on_hand FROM finished_stock WHERE finished_batch_id=$1),
			'REINSPECT_PASS_IN',$3)`, fgID, qty, batchNo)
		tx.Exec(`UPDATE return_goods SET status='reinspect_pass',processed_at=now() WHERE id=$1`, id)
		tx.Exec(`UPDATE finished_batches SET status='returned' WHERE id=$1 AND status='recalled'`, fgID)
	} else {
		moveOutOfQuarantine(tx, fgID, qty, batchNo)
		tx.Exec(`UPDATE finished_stock SET qty_destroyed=qty_destroyed+$1 WHERE finished_batch_id=$2`, qty, fgID)
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,(SELECT qty_destroyed FROM finished_stock WHERE finished_batch_id=$1),
			'REINSPECT_FAIL_DESTROY',$3)`, fgID, qty, batchNo)
		tx.Exec(`UPDATE return_goods SET status='reinspect_fail',processed_at=now() WHERE id=$1`, id)
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if s.cache != nil {
		s.cache.invalidateTrace(r.Context(), nil, []int64{fgID})
	}
	if req.Pass {
		writeJSON(w, 200, map[string]any{"id": id, "status": "reinspect_pass",
			"restored_on_hand_kg": qty, "note": "复检合格，数量回到可售库存"})
	} else {
		writeJSON(w, 200, map[string]any{"id": id, "status": "reinspect_fail",
			"destroyed_kg": qty, "note": "复检不合格，已转入必须销毁"})
	}
}

func (s *Server) listReturns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT rg.id,rg.return_no,COALESCE(rc.recall_no,'') AS recall_no,
		fb.batch_no AS finished_batch_no,COALESCE(c.code,'') AS channel_code,
		COALESCE(o.order_no,'') AS order_no,rg.qty_kg,rg.status,rg.disposition,
		rg.reason,rg.arrived_at,rg.decided_at,rg.processed_at
		FROM return_goods rg JOIN finished_batches fb ON fb.id=rg.finished_batch_id
		LEFT JOIN recalls rc ON rc.id=rg.recall_id
		LEFT JOIN channels c ON c.id=rg.channel_id
		LEFT JOIN customer_orders o ON o.id=rg.order_id
		ORDER BY rg.id DESC LIMIT 500`)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}
