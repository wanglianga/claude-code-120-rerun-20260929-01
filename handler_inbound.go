package main

import (
	"database/sql"
	"net/http"
)

func (a *App) createRawBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SupplierID            int64   `json:"supplier_id"`
		MaterialType          string  `json:"material_type"`
		CatchOrProductionDate string  `json:"catch_or_production_date"`
		InboundTempC          float64 `json:"inbound_temp_c"`
		QuarantineCertNo      string  `json:"quarantine_cert_no"`
		Inspector             string  `json:"inspector"`
		Note                  string  `json:"note"`
		Boxes                 []struct {
			BoxNo    string  `json:"box_no"`
			WeightKg float64 `json:"weight_kg"`
			Location string  `json:"location_code"`
		} `json:"boxes"`
	}
	if err := decode(r, &req); err != nil || req.SupplierID == 0 || req.MaterialType == "" ||
		req.CatchOrProductionDate == "" || req.QuarantineCertNo == "" || req.Inspector == "" {
		writeErr(w, 400, "supplier_id/material_type/catch_or_production_date/quarantine_cert_no/inspector 必填")
		return
	}
	valid := map[string]bool{"SHRIMP": true, "SCALLOP": true, "SQUID": true, "FISH_FILLET": true}
	if !valid[req.MaterialType] {
		writeErr(w, 400, "material_type 必须为 SHRIMP/SCALLOP/SQUID/FISH_FILLET")
		return
	}

	var limit float64
	if err := a.db.QueryRow(`SELECT value::float8 FROM system_config WHERE key='inbound_temp_limit_c'`).Scan(&limit); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	u := currentUser(r)
	batchNo, err := a.nextDocNo(r.Context(), "RM")
	if err != nil {
		writeErr(w, 500, "生成批次号失败: "+err.Error())
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	status := "FROZEN_IN"
	if req.InboundTempC > limit {
		status = "ON_HOLD"
	}
	var total float64
	for _, b := range req.Boxes {
		total += b.WeightKg
	}

	var batchID int64
	err = tx.QueryRow(`INSERT INTO raw_material_batches
		(batch_no,supplier_id,material_type,catch_or_production_date,inbound_temp_c,temp_limit_c,quarantine_cert_no,inspector,total_weight_kg,status,note)
		VALUES($1,$2,$3,$4::date,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		batchNo, req.SupplierID, req.MaterialType, req.CatchOrProductionDate, req.InboundTempC, limit,
		req.QuarantineCertNo, req.Inspector, total, status, req.Note).Scan(&batchID)
	if err != nil {
		writeErr(w, 409, "原料批次创建失败: "+err.Error())
		return
	}

	// 入库温度记录（超标即违规）
	if _, err := tx.Exec(`INSERT INTO temp_logs(batch_id,phase,temp_c,limit_c,violation,recorded_by,note)
		VALUES($1,'INBOUND',$2,$3,$4,$5,'入库测温')`,
		batchID, req.InboundTempC, limit, req.InboundTempC > limit, req.Inspector); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	for _, b := range req.Boxes {
		var locID sql.NullInt64
		if b.Location != "" {
			if err := tx.QueryRow(`SELECT id FROM storage_locations WHERE code=$1`, b.Location).Scan(&locID); err != nil {
				writeErr(w, 400, "库位不存在: "+b.Location)
				return
			}
		}
		if _, err := tx.Exec(`INSERT INTO raw_boxes(batch_id,box_no,weight_kg,location_id) VALUES($1,$2,$3,$4)`,
			batchID, b.BoxNo, b.WeightKg, locID); err != nil {
			writeErr(w, 409, "箱号录入失败("+b.BoxNo+"): "+err.Error())
			return
		}
	}

	// 温度超标 -> 自动开具质量事件，批次冻结 ON_HOLD
	var eventNo sql.NullString
	if req.InboundTempC > limit {
		en, _ := a.nextDocNo(r.Context(), "QE")
		if _, err := tx.Exec(`INSERT INTO quality_events
			(event_no,type,severity,raw_batch_id,description,status,raised_by)
			VALUES($1,'TEMP_EXCEEDED','HIGH',$2,$3,'OPEN',$4)`,
			en, batchID, "入库温度 "+ftoa(req.InboundTempC)+"℃ 高于限值 "+ftoa(limit)+"℃，批次待检冻结", u.Username); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		eventNo = sql.NullString{String: en, Valid: true}
	}

	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"batch_no": batchNo, "id": batchID, "status": status,
		"total_weight_kg": total, "temp_violation": req.InboundTempC > limit,
		"quality_event_no": eventNo.String,
	})
}

func (a *App) listRawBatches(w http.ResponseWriter, r *http.Request) {
	q := `SELECT b.id,b.batch_no,b.supplier_id,s.name AS supplier_name,b.material_type,
		b.catch_or_production_date,b.arrival_at,b.inbound_temp_c,b.temp_limit_c,
		b.quarantine_cert_no,b.inspector,b.total_weight_kg,b.status,b.note
		FROM raw_material_batches b JOIN suppliers s ON s.id=b.supplier_id
		WHERE ($1='' OR b.batch_no=$1) AND ($2='' OR b.status=$2) AND ($3=0 OR b.supplier_id=$3)
		ORDER BY b.id DESC`
	rows, err := a.db.Query(q, r.URL.Query().Get("batchNo"), r.URL.Query().Get("status"), atoi64(r.URL.Query().Get("supplierId")))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) getRawBatch(w http.ResponseWriter, r *http.Request) {
	batchNo := r.PathValue("batchNo")
	batch, err := a.queryOneMap(`SELECT b.id,b.batch_no,b.supplier_id,s.name AS supplier_name,b.material_type,
		b.catch_or_production_date,b.arrival_at,b.inbound_temp_c,b.temp_limit_c,b.quarantine_cert_no,
		b.inspector,b.total_weight_kg,b.status,b.note
		FROM raw_material_batches b JOIN suppliers s ON s.id=b.supplier_id WHERE b.batch_no=$1`, batchNo)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "原料批次不存在")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	boxRows, _ := a.db.Query(`SELECT rb.id,rb.box_no,rb.weight_kg,rb.location_id,COALESCE(l.code,'') AS location_code,rb.status,rb.inbound_at
		FROM raw_boxes rb LEFT JOIN storage_locations l ON l.id=rb.location_id
		WHERE rb.batch_id=$1 ORDER BY rb.box_no`, batch["id"])
	boxes := scanRows(boxRows)
	boxRows.Close()

	logRows, _ := a.db.Query(`SELECT id,phase,temp_c,limit_c,violation,recorded_by,recorded_at,note
		FROM temp_logs WHERE batch_id=$1 ORDER BY id`, batch["id"])
	logs := scanRows(logRows)
	logRows.Close()

	writeJSON(w, 200, map[string]any{"batch": batch, "boxes": boxes, "temp_logs": logs})
}

func (a *App) addRawBoxes(w http.ResponseWriter, r *http.Request) {
	batchNo := r.PathValue("batchNo")
	var req struct {
		Boxes []struct {
			BoxNo    string  `json:"box_no"`
			WeightKg float64 `json:"weight_kg"`
			Location string  `json:"location_code"`
		} `json:"boxes"`
	}
	if err := decode(r, &req); err != nil || len(req.Boxes) == 0 {
		writeErr(w, 400, "boxes 必填且至少一箱")
		return
	}
	var batchID int64
	if err := a.db.QueryRow(`SELECT id FROM raw_material_batches WHERE batch_no=$1`, batchNo).Scan(&batchID); err != nil {
		writeErr(w, 404, "原料批次不存在")
		return
	}
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	var added int
	var totalAdd float64
	for _, b := range req.Boxes {
		var locID sql.NullInt64
		if b.Location != "" {
			if err := tx.QueryRow(`SELECT id FROM storage_locations WHERE code=$1`, b.Location).Scan(&locID); err != nil {
				writeErr(w, 400, "库位不存在: "+b.Location)
				return
			}
		}
		if _, err := tx.Exec(`INSERT INTO raw_boxes(batch_id,box_no,weight_kg,location_id) VALUES($1,$2,$3,$4)`,
			batchID, b.BoxNo, b.WeightKg, locID); err != nil {
			writeErr(w, 409, "箱号录入失败("+b.BoxNo+"): "+err.Error())
			return
		}
		added++
		totalAdd += b.WeightKg
	}
	if _, err := tx.Exec(`UPDATE raw_material_batches SET total_weight_kg=total_weight_kg+$2 WHERE id=$1`, batchID, totalAdd); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"added": added, "batch_no": batchNo})
}

// addTempLog 仓储巡检温控；超标自动开具质量事件并冻结批次
func (a *App) addTempLog(w http.ResponseWriter, r *http.Request) {
	batchNo := r.PathValue("batchNo")
	var req struct {
		Phase      string  `json:"phase"`
		TempC      float64 `json:"temp_c"`
		RecordedBy string  `json:"recorded_by"`
		Note       string  `json:"note"`
		BoxNo      string  `json:"box_no"`
	}
	if err := decode(r, &req); err != nil || req.RecordedBy == "" {
		writeErr(w, 400, "temp_c/recorded_by 必填")
		return
	}
	if req.Phase == "" {
		req.Phase = "STORAGE"
	}
	var batchID int64
	if err := a.db.QueryRow(`SELECT id FROM raw_material_batches WHERE batch_no=$1`, batchNo).Scan(&batchID); err != nil {
		writeErr(w, 404, "原料批次不存在")
		return
	}

	var limit float64
	a.db.QueryRow(`SELECT value::float8 FROM system_config WHERE key='storage_temp_limit_c'`).Scan(&limit)
	violation := req.TempC > limit

	var boxID sql.NullInt64
	if req.BoxNo != "" {
		_ = a.db.QueryRow(`SELECT id FROM raw_boxes WHERE batch_id=$1 AND box_no=$2`, batchID, req.BoxNo).Scan(&boxID)
	}

	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO temp_logs(batch_id,box_id,phase,temp_c,limit_c,violation,recorded_by,note)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, batchID, boxID, req.Phase, req.TempC, limit, violation, req.RecordedBy, req.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	eventNo := ""
	if violation {
		// 无论批次此前是否已冻结，每次温度违规都开具质量事件留痕
		tx.Exec(`UPDATE raw_material_batches SET status='ON_HOLD' WHERE id=$1 AND status<>'CONSUMED' AND status<>'CLOSED'`, batchID)
		en, _ := a.nextDocNo(r.Context(), "QE")
		tx.Exec(`INSERT INTO quality_events(event_no,type,severity,raw_batch_id,box_no,description,status,raised_by)
			VALUES($1,'TEMP_EXCEEDED','HIGH',$2,$3,$4,'OPEN',$5)`,
			en, batchID, nz(req.BoxNo, "-"),
			"仓储巡检温度 "+ftoa(req.TempC)+"℃ 高于限值 "+ftoa(limit)+"℃ "+req.Note, req.RecordedBy)
		eventNo = en
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"violation": violation, "quality_event_no": eventNo})
}
