package main

import (
	"database/sql"
	"net/http"
)

// ---- 冷库库存 ----

func (a *App) listStockBoxes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind") // RAW / PRODUCT，默认 PRODUCT
	zone := q.Get("zone")
	locCode := q.Get("location_code")
	status := q.Get("status")
	if kind == "RAW" {
		rows, err := a.db.Query(`SELECT b.id,rb.batch_no,b.box_no,b.weight_kg,b.status,l.code AS location_code,l.zone
			FROM raw_boxes b
			JOIN raw_material_batches rb ON rb.id=b.batch_id
			LEFT JOIN storage_locations l ON l.id=b.location_id
			WHERE ($1='' OR b.status=$1) AND ($2='' OR l.code=$2) AND ($3='' OR l.zone=$3)
			ORDER BY rb.batch_no,b.box_no`, status, locCode, zone)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer rows.Close()
		writeJSON(w, 200, map[string]any{"kind": "RAW", "items": scanRows(rows)})
		return
	}
	rows, err := a.db.Query(`SELECT b.id,pb.batch_no,b.box_no,b.weight_kg,b.status,b.reworked,l.code AS location_code,l.zone,
		COALESCE(ps.code,'') AS packaging_spec_code
		FROM product_boxes b
		JOIN product_batches pb ON pb.id=b.batch_id
		LEFT JOIN storage_locations l ON l.id=b.location_id
		LEFT JOIN packaging_specs ps ON ps.id=b.packaging_spec_id
		WHERE ($1='' OR b.status=$1) AND ($2='' OR l.code=$2) AND ($3='' OR l.zone=$3)
		ORDER BY pb.batch_no,b.box_no`, status, locCode, zone)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"kind": "PRODUCT", "items": scanRows(rows)})
}

func (a *App) stockSummary(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT COALESCE(l.code,'未上架') AS location_code, COALESCE(l.zone,'') AS zone, b.status,
		COUNT(*) AS boxes, COALESCE(SUM(b.weight_kg),0) AS total_kg
		FROM product_boxes b LEFT JOIN storage_locations l ON l.id=b.location_id
		GROUP BY l.code,l.zone,b.status ORDER BY l.code,b.status`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	pd := scanRows(rows)
	rows.Close()
	rows2, err := a.db.Query(`SELECT COALESCE(l.code,'未上架') AS location_code, COALESCE(l.zone,'') AS zone, b.status,
		COUNT(*) AS boxes, COALESCE(SUM(b.weight_kg),0) AS total_kg
		FROM raw_boxes b LEFT JOIN storage_locations l ON l.id=b.location_id
		GROUP BY l.code,l.zone,b.status ORDER BY l.code,b.status`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows2.Close()
	writeJSON(w, 200, map[string]any{"product_boxes": pd, "raw_boxes": scanRows(rows2)})
}

// ---- 盘点 ----

func (a *App) createStocktake(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocationCode string `json:"location_code"`
		CountedBy    string `json:"counted_by"`
		Note         string `json:"note"`
		// 可选：实际上报明细，缺省按系统账面全量生成待盘清单
		Actuals []struct {
			BoxKind  string  `json:"box_kind"`
			BoxNo    string  `json:"box_no"`
			ActualKg float64 `json:"actual_kg"`
			Found    *bool   `json:"found"`
			Note     string  `json:"note"`
		} `json:"actuals"`
	}
	if err := decode(r, &req); err != nil || req.LocationCode == "" || req.CountedBy == "" {
		writeErr(w, 400, "location_code/counted_by 必填")
		return
	}
	var locID int64
	var zone string
	if err := a.db.QueryRow(`SELECT id,zone FROM storage_locations WHERE code=$1`, req.LocationCode).Scan(&locID, &zone); err != nil {
		writeErr(w, 400, "库位不存在: "+req.LocationCode)
		return
	}
	takeNo, err := a.nextDocNo(r.Context(), "ST")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	var takeID int64
	if err := tx.QueryRow(`INSERT INTO stocktakes(take_no,location_id,counted_by,note) VALUES($1,$2,$3,$4) RETURNING id`,
		takeNo, locID, req.CountedBy, req.Note).Scan(&takeID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// 账面清单：该库位所有未销毁箱
	expected := []struct {
		kind   string
		boxID  int64
		boxNo  string
		weight float64
	}{}
	table := "product_boxes"
	_ = table
	collect := func(kind string) {
		var rows *sql.Rows
		var e error
		if kind == "RAW" {
			rows, e = tx.Query(`SELECT b.id,b.box_no,b.weight_kg FROM raw_boxes b WHERE b.location_id=$1 AND b.status<>'DESTROYED'`, locID)
		} else {
			rows, e = tx.Query(`SELECT b.id,b.box_no,b.weight_kg FROM product_boxes b WHERE b.location_id=$1 AND b.status NOT IN ('DESTROYED')`, locID)
		}
		if e != nil {
			return
		}
		for rows.Next() {
			var x struct {
				kind   string
				boxID  int64
				boxNo  string
				weight float64
			}
			_ = rows.Scan(&x.boxID, &x.boxNo, &x.weight)
			x.kind = kind
			expected = append(expected, x)
		}
		rows.Close()
	}
	if zone == "RAW_FROZEN" || zone == "THAW_ROOM" {
		collect("RAW")
	} else {
		collect("PRODUCT")
	}

	actualMap := map[string]struct {
		kg   float64
		fnd  bool
		has  bool
		note string
	}{}
	for _, ac := range req.Actuals {
		actualMap[ac.BoxKind+":"+ac.BoxNo] = struct {
			kg   float64
			fnd  bool
			has  bool
			note string
		}{ac.ActualKg, ac.Found == nil || *ac.Found, true, ac.Note}
	}

	lines := 0
	var varianceTotal float64
	for _, ex := range expected {
		actualKg := ex.weight
		found := true
		note := ""
		if am, ok := actualMap[ex.kind+":"+ex.boxNo]; ok && am.has {
			actualKg = am.kg
			found = am.fnd
			note = am.note
		}
		variance := 0.0
		if found {
			variance = actualKg - ex.weight
		} else {
			actualKg = 0
			variance = -ex.weight
		}
		if _, err := tx.Exec(`INSERT INTO stocktake_lines(stocktake_id,box_kind,box_id,expected_kg,actual_kg,found,variance_kg,note)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, takeID, ex.kind, ex.boxID, ex.weight, actualKg, found, variance, note); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		varianceTotal += variance
		lines++
	}

	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"take_no": takeNo, "location_code": req.LocationCode,
		"lines": lines, "variance_kg": varianceTotal, "status": "DRAFT",
	})
}

func (a *App) listStocktakes(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT st.id,st.take_no,st.location_id,l.code AS location_code,st.status,st.counted_by,st.counted_at,st.confirmed_at,st.note,
		COALESCE((SELECT SUM(variance_kg) FROM stocktake_lines WHERE stocktake_id=st.id),0) AS variance_kg
		FROM stocktakes st JOIN storage_locations l ON l.id=st.location_id ORDER BY st.id DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) confirmStocktake(w http.ResponseWriter, r *http.Request) {
	takeNo := r.PathValue("takeNo")
	var takeID, locID int64
	var status string
	if err := a.db.QueryRow(`SELECT id,location_id,status FROM stocktakes WHERE take_no=$1`, takeNo).Scan(&takeID, &locID, &status); err != nil {
		writeErr(w, 404, "盘点单不存在")
		return
	}
	if status != "DRAFT" {
		writeErr(w, 409, "盘点单已确认")
		return
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE stocktakes SET status='CONFIRMED', confirmed_at=now() WHERE id=$1`, takeID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 账实差异以实盘重量回写箱重（盘亏箱标记，不直接删除，保留可追溯）
	rows, _ := tx.Query(`SELECT box_kind,box_id,actual_kg,found FROM stocktake_lines WHERE stocktake_id=$1`, takeID)
	type ln struct {
		kind     string
		boxID    int64
		actualKg float64
		found    bool
	}
	lns := []ln{}
	for rows.Next() {
		var x ln
		if rows.Scan(&x.kind, &x.boxID, &x.actualKg, &x.found) == nil {
			lns = append(lns, x)
		}
	}
	rows.Close()
	for _, x := range lns {
		if x.kind == "PRODUCT" {
			tx.Exec(`UPDATE product_boxes SET weight_kg=$2 WHERE id=$1`, x.boxID, x.actualKg)
		} else {
			tx.Exec(`UPDATE raw_boxes SET weight_kg=$2 WHERE id=$1`, x.boxID, x.actualKg)
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"take_no": takeNo, "status": "CONFIRMED", "lines": len(lns)})
}

// ---- 发货（渠道定制包装） ----

func (a *App) createShipment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelCode   string   `json:"channel_code"`
		BatchNo       string   `json:"product_batch_no"`
		BoxNos        []string `json:"box_nos"`
		PackagingCode string   `json:"packaging_spec_code"` // 可整单改贴渠道定制包装
		VehicleNo     string   `json:"vehicle_no"`
		Operator      string   `json:"operator"`
	}
	if err := decode(r, &req); err != nil || req.ChannelCode == "" || req.BatchNo == "" || len(req.BoxNos) == 0 {
		writeErr(w, 400, "channel_code/product_batch_no/box_nos 必填")
		return
	}
	var channelID int64
	if err := a.db.QueryRow(`SELECT id FROM channels WHERE code=$1 AND active`, req.ChannelCode).Scan(&channelID); err != nil {
		writeErr(w, 400, "渠道不存在或已停用: "+req.ChannelCode)
		return
	}
	var pbID int64
	var pbStatus string
	if err := a.db.QueryRow(`SELECT id,status FROM product_batches WHERE batch_no=$1`, req.BatchNo).Scan(&pbID, &pbStatus); err != nil {
		writeErr(w, 404, "成品批次不存在")
		return
	}
	if pbStatus == "RECALL_HELD" || pbStatus == "RECALLED" {
		writeErr(w, 409, "批次处于召回冻结/召回状态，禁止发货")
		return
	}
	var pkgID sql.NullInt64
	if req.PackagingCode != "" {
		if err := a.db.QueryRow(`SELECT id FROM packaging_specs WHERE code=$1`, req.PackagingCode).Scan(&pkgID); err != nil {
			writeErr(w, 400, "包装规格不存在: "+req.PackagingCode)
			return
		}
	}
	op := nz(req.Operator, currentUser(r).Username)
	shipNo, err := a.nextDocNo(r.Context(), "SH")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	var shipID int64
	if err := tx.QueryRow(`INSERT INTO shipments(ship_no,channel_id,product_batch_id,packaging_spec_id,vehicle_no,operator,total_boxes)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		shipNo, channelID, pbID, pkgID, req.VehicleNo, op, len(req.BoxNos)).Scan(&shipID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	totalKg := 0.0
	for _, bn := range req.BoxNos {
		var boxID int64
		var weight float64
		var bstatus string
		if err := tx.QueryRow(`SELECT id,weight_kg,status FROM product_boxes WHERE batch_id=$1 AND box_no=$2`, pbID, bn).
			Scan(&boxID, &weight, &bstatus); err != nil {
			writeErr(w, 404, "箱号不存在于该批次: "+bn)
			return
		}
		if bstatus != "IN_STOCK" && bstatus != "REWORKED" {
			writeErr(w, 409, "箱号 "+bn+" 当前状态 "+bstatus+"，不可发货")
			return
		}
		if _, err := tx.Exec(`INSERT INTO shipment_boxes(shipment_id,box_id) VALUES($1,$2)`, shipID, boxID); err != nil {
			writeErr(w, 409, "箱号已在其他发货单中: "+bn)
			return
		}
		if pkgID.Valid {
			tx.Exec(`UPDATE product_boxes SET packaging_spec_id=$2, updated_at=now() WHERE id=$1`, boxID, pkgID.Int64)
		}
		tx.Exec(`UPDATE product_boxes SET status='SHIPPED', location_id=NULL, updated_at=now() WHERE id=$1`, boxID)
		totalKg += weight
	}
	tx.Exec(`UPDATE shipments SET total_kg=$2 WHERE id=$1`, shipID, totalKg)
	tx.Exec(`UPDATE product_batches SET status=CASE WHEN status='COLD_STORAGE' THEN 'PARTIAL_SHIPPED' ELSE status END WHERE id=$1`, pbID)
	// 若该批次全部箱已发，置 SHIPPED
	var inStock int
	tx.QueryRow(`SELECT COUNT(*) FROM product_boxes WHERE batch_id=$1 AND status IN ('IN_STOCK','RECALL_HELD','REWORKED')`, pbID).Scan(&inStock)
	if inStock == 0 {
		tx.Exec(`UPDATE product_batches SET status='SHIPPED' WHERE id=$1`, pbID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"ship_no": shipNo, "channel_code": req.ChannelCode, "product_batch_no": req.BatchNo,
		"boxes": len(req.BoxNos), "total_kg": totalKg, "custom_packaging": req.PackagingCode,
	})
}

func (a *App) listShipments(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT sh.id,sh.ship_no,sh.channel_id,c.code AS channel_code,c.name AS channel_name,
		sh.product_batch_id,pb.batch_no AS product_batch_no,COALESCE(ps.code,'') AS packaging_spec_code,
		sh.vehicle_no,sh.operator,sh.total_boxes,sh.total_kg,sh.status,sh.shipped_at
		FROM shipments sh
		JOIN channels c ON c.id=sh.channel_id
		JOIN product_batches pb ON pb.id=sh.product_batch_id
		LEFT JOIN packaging_specs ps ON ps.id=sh.packaging_spec_id
		WHERE ($1='' OR pb.batch_no=$1) AND ($2='' OR c.code=$2)
		ORDER BY sh.id DESC`, r.URL.Query().Get("batchNo"), r.URL.Query().Get("channelCode"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := scanRows(rows)
	writeJSON(w, 200, map[string]any{"items": items})
}

// ---- 销售订单（精确到箱，支持召回时定位已售订单） ----

func (a *App) createOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelCode string  `json:"channel_code"`
		ShipNo      string  `json:"ship_no"`
		CustomerRef string  `json:"customer_ref"`
		AmountCny   float64 `json:"amount_cny"`
		Items       []struct {
			BoxNo string `json:"box_no"`
			Qty   int    `json:"qty"`
		} `json:"items"`
	}
	if err := decode(r, &req); err != nil || req.ChannelCode == "" || len(req.Items) == 0 {
		writeErr(w, 400, "channel_code/items 必填")
		return
	}
	var channelID int64
	if err := a.db.QueryRow(`SELECT id FROM channels WHERE code=$1`, req.ChannelCode).Scan(&channelID); err != nil {
		writeErr(w, 400, "渠道不存在: "+req.ChannelCode)
		return
	}
	var shipID sql.NullInt64
	if req.ShipNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM shipments WHERE ship_no=$1`, req.ShipNo).Scan(&shipID); err != nil {
			writeErr(w, 400, "发货单不存在: "+req.ShipNo)
			return
		}
	}
	orderNo, err := a.nextDocNo(r.Context(), "SO")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	var orderID int64
	if err := tx.QueryRow(`INSERT INTO sales_orders(order_no,channel_id,shipment_id,customer_ref,amount_cny)
		VALUES($1,$2,$3,$4,$5) RETURNING id`, orderNo, channelID, shipID, req.CustomerRef, req.AmountCny).Scan(&orderID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, it := range req.Items {
		var boxID, pbID int64
		if err := tx.QueryRow(`SELECT id,batch_id FROM product_boxes WHERE box_no=$1`, it.BoxNo).Scan(&boxID, &pbID); err != nil {
			writeErr(w, 404, "箱号不存在: "+it.BoxNo)
			return
		}
		if _, err := tx.Exec(`INSERT INTO order_items(order_id,product_batch_id,box_id,qty) VALUES($1,$2,$3,$4)`,
			orderID, pbID, boxID, max1(it.Qty)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		tx.Exec(`UPDATE product_boxes SET status='SOLD' WHERE id=$1 AND status='SHIPPED'`, boxID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"order_no": orderNo, "items": len(req.Items)})
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

func (a *App) listOrders(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT o.id,o.order_no,o.channel_id,c.code AS channel_code,c.name AS channel_name,
		o.shipment_id,sh.ship_no,o.customer_ref,o.amount_cny,o.status,o.sold_at,
		COALESCE((SELECT string_agg(pb.batch_no,',') FROM order_items oi
		          JOIN product_batches pb ON pb.id=oi.product_batch_id WHERE oi.order_id=o.id),'') AS batches
		FROM sales_orders o
		JOIN channels c ON c.id=o.channel_id
		LEFT JOIN shipments sh ON sh.id=o.shipment_id
		WHERE ($1='' OR o.order_no=$1) AND ($2='' OR c.code=$2) AND ($3='' OR o.status=$3)
		ORDER BY o.id DESC`,
		r.URL.Query().Get("orderNo"), r.URL.Query().Get("channelCode"), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

// ---- 渠道反馈（异味、直播间集中退货等） ----

func (a *App) createFeedback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelCode string `json:"channel_code"`
		OrderNo     string `json:"order_no"`
		BatchNo     string `json:"product_batch_no"`
		BoxNo       string `json:"box_no"`
		Source      string `json:"source"`
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	if err := decode(r, &req); err != nil || req.ChannelCode == "" || req.Source == "" || req.Type == "" || req.Description == "" {
		writeErr(w, 400, "channel_code/source/type/description 必填")
		return
	}
	validSrc := map[string]bool{"CHANNEL": true, "LIVESTREAM": true, "REGULATOR": true, "CUSTOMER_SERVICE": true}
	validType := map[string]bool{"OFF_ODOR": true, "FOREIGN_OBJECT": true, "SPOILAGE": true, "PACKAGE_DAMAGE": true, "BATCH_RETURN": true, "OTHER": true}
	if !validSrc[req.Source] || !validType[req.Type] {
		writeErr(w, 400, "source/type 取值非法")
		return
	}
	var channelID int64
	if err := a.db.QueryRow(`SELECT id FROM channels WHERE code=$1`, req.ChannelCode).Scan(&channelID); err != nil {
		writeErr(w, 400, "渠道不存在: "+req.ChannelCode)
		return
	}
	var orderID sql.NullInt64
	if req.OrderNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM sales_orders WHERE order_no=$1`, req.OrderNo).Scan(&orderID); err != nil {
			writeErr(w, 400, "订单不存在: "+req.OrderNo)
			return
		}
	}
	var pbID sql.NullInt64
	if req.BatchNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM product_batches WHERE batch_no=$1`, req.BatchNo).Scan(&pbID); err != nil {
			writeErr(w, 400, "成品批次不存在: "+req.BatchNo)
			return
		}
	}

	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	var fbID int64
	if err := tx.QueryRow(`INSERT INTO channel_feedbacks(channel_id,order_id,product_batch_id,box_no,source,type,description)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		channelID, orderID, pbID, req.BoxNo, req.Source, req.Type, req.Description).Scan(&fbID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	eventNo := ""
	// 异味 / 直播间集中退货（批量退）自动开质量事件
	if req.Type == "OFF_ODOR" || (req.Source == "LIVESTREAM" && req.Type == "BATCH_RETURN") {
		etype := "OFF_ODOR"
		sev := "HIGH"
		if req.Source == "LIVESTREAM" && req.Type == "BATCH_RETURN" {
			etype = "LIVESTREAM_BATCH_RETURN"
		}
		en, _ := a.nextDocNo(r.Context(), "QE")
		tx.Exec(`INSERT INTO quality_events(event_no,type,severity,product_batch_id,box_no,feedback_id,description,status,raised_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,'OPEN',$8)`,
			en, etype, sev, pbID, nz(req.BoxNo, "-"), fbID,
			srcName(req.Source)+"反馈："+req.Description, u.Username)
		eventNo = en
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": fbID, "quality_event_no": eventNo})
}

func srcName(s string) string {
	switch s {
	case "LIVESTREAM":
		return "直播间"
	case "CHANNEL":
		return "渠道"
	case "REGULATOR":
		return "监管"
	case "CUSTOMER_SERVICE":
		return "客服"
	}
	return s
}

func (a *App) listFeedbacks(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT f.id,f.channel_id,c.code AS channel_code,c.name AS channel_name,
		f.order_id,o.order_no,f.product_batch_id,pb.batch_no AS product_batch_no,f.box_no,
		f.source,f.type,f.description,f.reported_at,f.handled
		FROM channel_feedbacks f
		JOIN channels c ON c.id=f.channel_id
		LEFT JOIN sales_orders o ON o.id=f.order_id
		LEFT JOIN product_batches pb ON pb.id=f.product_batch_id
		WHERE ($1='' OR f.type=$1) AND ($2='' OR f.source=$2)
		ORDER BY f.id DESC`, r.URL.Query().Get("type"), r.URL.Query().Get("source"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}
