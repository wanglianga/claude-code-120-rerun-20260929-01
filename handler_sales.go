package main

import (
	"database/sql"
	"net/http"
)

// ---------- 发货 ----------

type shipItemReq struct {
	FinishedBatchNo string  `json:"finished_batch_no"`
	QtyKg           float64 `json:"qty_kg"`
}

type shipmentReq struct {
	ChannelCode string        `json:"channel_code"`
	Note        string        `json:"note"`
	Items       []shipItemReq `json:"items"`
}

// createShipment 渠道发货：扣减可售库存、累计已发、写台账，多成品批次可拼车
func (s *Server) createShipment(w http.ResponseWriter, r *http.Request) {
	var req shipmentReq
	if !decode(w, r, &req) || req.ChannelCode == "" || len(req.Items) == 0 {
		errJSON(w, 400, "channel_code 与 items(非空) 必填")
		return
	}
	by := currentUser(r).Username
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var chID int64
	var chStatus string
	if err = tx.QueryRow(`SELECT id,status FROM channels WHERE code=$1`,
		req.ChannelCode).Scan(&chID, &chStatus); err != nil {
		errJSON(w, 400, "渠道不存在: "+req.ChannelCode)
		return
	}
	if chStatus == "suspended" {
		errJSON(w, 409, "渠道已停用: "+req.ChannelCode)
		return
	}
	shipNo, _ := s.nextDocNo("SH")
	var shipID int64
	if err = tx.QueryRow(`INSERT INTO shipments(shipment_no,channel_id,note)
		VALUES ($1,$2,$3) RETURNING id`, shipNo, chID, req.Note).Scan(&shipID); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	fgIDs := []int64{}
	for _, it := range req.Items {
		if it.FinishedBatchNo == "" || it.QtyKg <= 0 {
			errJSON(w, 400, "每项 finished_batch_no 与 qty_kg(>0) 必填")
			return
		}
		var fgID int64
		var status string
		var onHand, total float64
		if err = tx.QueryRow(`SELECT fb.id,fb.status,fs.qty_on_hand,fb.qty_kg
			FROM finished_batches fb JOIN finished_stock fs ON fs.finished_batch_id=fb.id
			WHERE fb.batch_no=$1 FOR UPDATE OF fs`, it.FinishedBatchNo).
			Scan(&fgID, &status, &onHand, &total); err != nil {
			errJSON(w, 400, "成品批次不存在: "+it.FinishedBatchNo)
			return
		}
		if status != "released" && status != "shipped" {
			errJSON(w, 409, "成品批次 "+it.FinishedBatchNo+" 未放行（留样合格）或已召回，不能发货")
			return
		}
		if onHand < it.QtyKg-0.001 {
			errJSON(w, 409, "成品批次 "+it.FinishedBatchNo+" 可售库存不足: 可售"+
				strconvFormat(onHand)+"kg / 申请"+strconvFormat(it.QtyKg)+"kg")
			return
		}
		if _, err = tx.Exec(`INSERT INTO shipment_items(shipment_id,finished_batch_id,qty_kg)
			VALUES ($1,$2,$3)`, shipID, fgID, it.QtyKg); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		tx.Exec(`UPDATE finished_stock SET qty_on_hand=qty_on_hand-$1,qty_shipped=qty_shipped+$1,
			updated_at=now() WHERE finished_batch_id=$2`, it.QtyKg, fgID)
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,(SELECT qty_on_hand FROM finished_stock WHERE finished_batch_id=$1),'SHIP_OUT',$3)`,
			fgID, -it.QtyKg, shipNo)
		tx.Exec(`UPDATE finished_batches SET status='shipped' WHERE id=$1 AND status='released'`, fgID)
		fgIDs = append(fgIDs, fgID)
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if s.cache != nil {
		s.cache.invalidateTrace(r.Context(), nil, fgIDs)
	}
	writeJSON(w, 201, map[string]any{"id": shipID, "shipment_no": shipNo,
		"channel_code": req.ChannelCode, "item_count": len(req.Items), "created_by": by})
}

// ---------- 终端订单 ----------

type orderReq struct {
	OrderNo         string  `json:"order_no"`
	ChannelCode     string  `json:"channel_code"`
	ShipmentNo      string  `json:"shipment_no"`
	FinishedBatchNo string  `json:"finished_batch_no"`
	Buyer           string  `json:"buyer"`
	QtyKg           float64 `json:"qty_kg"`
	Amount          float64 `json:"amount"`
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	var req orderReq
	if !decode(w, r, &req) || req.OrderNo == "" || req.ChannelCode == "" ||
		req.FinishedBatchNo == "" || req.QtyKg <= 0 {
		errJSON(w, 400, "order_no/channel_code/finished_batch_no/qty_kg 必填")
		return
	}
	var chID, fgID, shipID sql.NullInt64
	if err := s.db.QueryRow(`SELECT id FROM channels WHERE code=$1`, req.ChannelCode).
		Scan(&chID); err != nil {
		errJSON(w, 400, "渠道不存在")
		return
	}
	if err := s.db.QueryRow(`SELECT id FROM finished_batches WHERE batch_no=$1`,
		req.FinishedBatchNo).Scan(&fgID); err != nil {
		errJSON(w, 400, "成品批次不存在")
		return
	}
	if req.ShipmentNo != "" {
		var sid int64
		if err := s.db.QueryRow(`SELECT id FROM shipments WHERE shipment_no=$1`,
			req.ShipmentNo).Scan(&sid); err != nil {
			errJSON(w, 400, "发货单不存在")
			return
		}
		shipID = nullInt64(sid)
	}
	// finished_batch_no 字段放错位置：orderReq.FinishedBatchNo 的 JSON tag 错了，下面单独接收
	var id int64
	if err := s.db.QueryRow(`INSERT INTO customer_orders
		(order_no,channel_id,shipment_id,finished_batch_id,buyer,qty_kg,amount)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		req.OrderNo, chID, shipID, fgID, nullString(req.Buyer), req.QtyKg, req.Amount).Scan(&id); err != nil {
		errJSON(w, 409, "订单创建失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "order_no": req.OrderNo})
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	q := `SELECT o.id,o.order_no,c.code AS channel_code,c.name AS channel_name,
		fb.batch_no AS finished_batch_no,o.buyer,o.qty_kg,o.amount,o.status,o.created_at
		FROM customer_orders o JOIN channels c ON c.id=o.channel_id
		JOIN finished_batches fb ON fb.id=o.finished_batch_id ORDER BY o.id DESC LIMIT 500`
	rows, err := s.db.Query(q)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}
