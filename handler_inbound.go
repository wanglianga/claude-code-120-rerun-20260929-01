package main

import (
	"database/sql"
	"net/http"
)

type boxReq struct {
	BoxNo       string  `json:"box_no"`
	LocationCode string `json:"location_code"`
	WeightKg    float64 `json:"weight_kg"`
}

type rawBatchReq struct {
	MaterialCode            string   `json:"material_code"`
	SupplierCode            string   `json:"supplier_code"`
	CatchOrProductionDate   string   `json:"catch_or_production_date"`
	ArrivalAt               string   `json:"arrival_at"`
	InboundTempC            float64  `json:"inbound_temp_c"`
	QuarantineCertNo        string   `json:"quarantine_cert_no"`
	Boxes                   []boxReq `json:"boxes"`
}

func (s *Server) createRawBatch(w http.ResponseWriter, r *http.Request) {
	var req rawBatchReq
	if !decode(w, r, &req) {
		return
	}
	if req.MaterialCode == "" || req.SupplierCode == "" || req.CatchOrProductionDate == "" ||
		req.QuarantineCertNo == "" || len(req.Boxes) == 0 {
		errJSON(w, 400, "material_code/supplier_code/catch_or_production_date/quarantine_cert_no/boxes 必填")
		return
	}
	mid, err := s.getIDByCode("materials", "code", req.MaterialCode)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	supID, err := s.getIDByCode("suppliers", "code", req.SupplierCode)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	arrival := parseTime(req.ArrivalAt)
	batchNo, err := s.nextDocNo("RAW")
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	var total float64
	for _, b := range req.Boxes {
		total += b.WeightKg
	}

	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	// 入库温度超标 → 批次先行隔离（quarantined），否则正常在库
	status := "in_storage"
	tempMax := s.settingFloat("raw_inbound_temp_max_c", -15)
	if req.InboundTempC > tempMax {
		status = "quarantined"
	}
	var rbID int64
	if err = tx.QueryRow(`INSERT INTO raw_batches
		(batch_no,material_id,supplier_id,catch_or_production_date,arrival_at,inbound_temp_c,
		 quarantine_cert_no,total_weight_kg,status)
		VALUES ($1,$2,$3,$4::date,$5,$6,$7,$8,$9) RETURNING id`,
		batchNo, mid, supID, req.CatchOrProductionDate, arrival, req.InboundTempC,
		req.QuarantineCertNo, total, status).Scan(&rbID); err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	boxStatus := "in_storage"
	if status == "quarantined" {
		boxStatus = "quarantined"
	}
	type boxOut struct {
		BoxNo string `json:"box_no"`
		ID    int64  `json:"id"`
	}
	boxes := []boxOut{}
	for _, b := range req.Boxes {
		if b.BoxNo == "" || b.WeightKg <= 0 {
			errJSON(w, 400, "每箱 box_no 与 weight_kg(>0) 必填")
			return
		}
		var locID sql.NullInt64
		if b.LocationCode != "" {
			id, err := lookupID(tx, `SELECT id FROM locations WHERE code=$1`, b.LocationCode)
			if err != nil {
				errJSON(w, 400, err.Error())
				return
			}
			locID = sql.NullInt64{Int64: id, Valid: true}
		}
		var boxID int64
		if err = tx.QueryRow(`INSERT INTO raw_boxes(raw_batch_id,box_no,location_id,weight_kg,status)
			VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			rbID, b.BoxNo, locID, b.WeightKg, boxStatus).Scan(&boxID); err != nil {
			errJSON(w, 409, "箱号重复或无效: "+b.BoxNo+" ("+err.Error()+")")
			return
		}
		boxes = append(boxes, boxOut{BoxNo: b.BoxNo, ID: boxID})
	}
	if _, err = tx.Exec(`INSERT INTO storage_temp_logs(raw_batch_id,temp_c,recorder)
		VALUES ($1,$2,$3)`, rbID, req.InboundTempC, currentUser(r).Username); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	var eventNo sql.NullString
	if status == "quarantined" {
		en, _ := s.nextDocNo("QE")
		if _, err = tx.Exec(`INSERT INTO quality_events
			(event_no,type,severity,source,raw_batch_id,description,status,created_by)
			VALUES ($1,'RAW_TEMP_OVER','major','system',$2,$3,'open',$4)`,
			en, rbID,
			"冻品入库温度 "+f2(req.InboundTempC)+"℃ 超过上限 "+f2(tempMax)+"℃，批次先行隔离待检",
			currentUser(r).Username); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		eventNo = sql.NullString{String: en, Valid: true}
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	resp := map[string]any{
		"id": rbID, "batch_no": batchNo, "status": status,
		"total_weight_kg": total, "boxes": boxes,
	}
	if eventNo.Valid {
		resp["temperature_alert"] = map[string]any{
			"event_no": eventNo.String,
			"message":  "入库温度超标，批次已隔离并生成品控事件",
		}
	}
	writeJSON(w, 201, resp)
}

func (s *Server) listRawBatches(w http.ResponseWriter, r *http.Request) {
	q := `SELECT rb.id,rb.batch_no,m.code AS material_code,m.name AS material_name,
			s.code AS supplier_code,s.name AS supplier_name,
			rb.catch_or_production_date,rb.arrival_at,rb.inbound_temp_c,
			rb.quarantine_cert_no,rb.total_weight_kg,rb.status
		  FROM raw_batches rb JOIN materials m ON m.id=rb.material_id
		  JOIN suppliers s ON s.id=rb.supplier_id`
	args := []any{}
	if code := r.URL.Query().Get("batch_no"); code != "" {
		q += " WHERE rb.batch_no=$1"
		args = append(args, code)
	}
	q += " ORDER BY rb.id DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

func (s *Server) getRawBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var batchNo, mat, sup, cert, catchDate, status string
	var temp, total float64
	var arrival sql.NullTime
	err := s.db.QueryRow(`SELECT rb.batch_no,m.code,s.code,rb.catch_or_production_date::text,
			rb.arrival_at,rb.inbound_temp_c,rb.quarantine_cert_no,rb.total_weight_kg,rb.status
		FROM raw_batches rb JOIN materials m ON m.id=rb.material_id JOIN suppliers s ON s.id=rb.supplier_id
		WHERE rb.id=$1`, id).
		Scan(&batchNo, &mat, &sup, &catchDate, &arrival, &temp, &cert, &total, &status)
	if err == sql.ErrNoRows {
		errJSON(w, 404, "raw batch not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	rows, _ := s.db.Query(`SELECT rb.box_no,COALESCE(l.code,'') AS location_code,rb.weight_kg,rb.status
		FROM raw_boxes rb LEFT JOIN locations l ON l.id=rb.location_id
		WHERE rb.raw_batch_id=$1 ORDER BY rb.box_no`, id)
	boxes := scanMaps(rows)
	rows.Close()
	writeJSON(w, 200, map[string]any{
		"id": id, "batch_no": batchNo, "material_code": mat, "supplier_code": sup,
		"catch_or_production_date": catchDate, "arrival_at": arrival.Time,
		"inbound_temp_c": temp, "quarantine_cert_no": cert,
		"total_weight_kg": total, "status": status, "boxes": boxes,
	})
}

type storageTempReq struct {
	RawBatchID   int64   `json:"raw_batch_id"`
	LocationCode string  `json:"location_code"`
	TempC        float64 `json:"temp_c"`
}

func (s *Server) recordStorageTemp(w http.ResponseWriter, r *http.Request) {
	var req storageTempReq
	if !decode(w, r, &req) || req.RawBatchID == 0 {
		errJSON(w, 400, "raw_batch_id 与 temp_c 必填")
		return
	}
	var locID sql.NullInt64
	if req.LocationCode != "" {
		id, err := s.getIDByCode("locations", "code", req.LocationCode)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		locID = sql.NullInt64{Int64: id, Valid: true}
	}
	var logID int64
	if err := s.db.QueryRow(`INSERT INTO storage_temp_logs(raw_batch_id,location_id,temp_c,recorder)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		req.RawBatchID, locID, req.TempC, currentUser(r).Username).Scan(&logID); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	// 超过冷库温度告警上限 → 品控事件
	alert := map[string]any{}
	limit := s.settingFloat("storage_temp_max_c", -18)
	if req.TempC > limit {
		en, _ := s.nextDocNo("QE")
		s.db.Exec(`INSERT INTO quality_events
			(event_no,type,severity,source,raw_batch_id,description,status,created_by)
			VALUES ($1,'RAW_TEMP_OVER','minor','system',$2,$3,'open',$4)`,
			en, req.RawBatchID,
			"仓储温度 "+f2(req.TempC)+"℃ 超过告警上限 "+f2(limit)+"℃",
			currentUser(r).Username)
		alert = map[string]any{"event_no": en, "message": "仓储温度超标已生成品控事件"}
	}
	writeJSON(w, 201, map[string]any{"id": logID, "alert": alert})
}

func f2(f float64) string {
	return strconvFormat(f)
}
