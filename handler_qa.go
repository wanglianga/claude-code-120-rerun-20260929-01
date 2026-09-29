package main

import (
	"database/sql"
	"net/http"
)

func (a *App) createSample(w http.ResponseWriter, r *http.Request) {
	batchNo := r.PathValue("batchNo")
	var req struct {
		BoxNo           string `json:"box_no"`
		QtyG            int    `json:"qty_g"`
		StorageLocation string `json:"storage_location"`
		RetainedBy      string `json:"retained_by"`
	}
	if err := decode(r, &req); err != nil || req.QtyG <= 0 || req.RetainedBy == "" {
		writeErr(w, 400, "qty_g/retained_by 必填")
		return
	}
	var pbID int64
	if err := a.db.QueryRow(`SELECT id FROM product_batches WHERE batch_no=$1`, batchNo).Scan(&pbID); err != nil {
		writeErr(w, 404, "成品批次不存在")
		return
	}
	var boxID sql.NullInt64
	if req.BoxNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM product_boxes WHERE batch_id=$1 AND box_no=$2`, pbID, req.BoxNo).Scan(&boxID); err != nil {
			writeErr(w, 400, "箱号不存在: "+req.BoxNo)
			return
		}
	}
	sno, err := a.nextDocNo(r.Context(), "RS")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var id int64
	err = a.db.QueryRow(`INSERT INTO retained_samples(product_batch_id,box_id,sample_no,qty_g,storage_location,retained_by)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id`,
		pbID, boxID, sno, req.QtyG, nz(req.StorageLocation, "成品留样柜"), req.RetainedBy).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "sample_no": sno, "lab_result": "PENDING"})
}

func (a *App) listSamples(w http.ResponseWriter, r *http.Request) {
	batchNo := r.PathValue("batchNo")
	rows, err := a.db.Query(`SELECT s.id,s.sample_no,s.qty_g,s.storage_location,s.retained_by,s.retained_at,
		s.lab_result,s.lab_report_no,s.tested_at,s.conclusion,
		pb.batch_no AS product_batch_no
		FROM retained_samples s JOIN product_batches pb ON pb.id=s.product_batch_id
		WHERE ($1='' OR pb.batch_no=$1) ORDER BY s.id DESC`, batchNo)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

// reportLabResult 留样检测结论；FAIL 时自动开质量事件
func (a *App) reportLabResult(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var req struct {
		LabResult   string `json:"lab_result"`
		LabReportNo string `json:"lab_report_no"`
		Conclusion  string `json:"conclusion"`
	}
	if err := decode(r, &req); err != nil || (req.LabResult != "PASS" && req.LabResult != "FAIL") {
		writeErr(w, 400, "lab_result 必须为 PASS/FAIL")
		return
	}
	var pbID int64
	var sampleNo string
	if err := a.db.QueryRow(`SELECT product_batch_id,sample_no FROM retained_samples WHERE id=$1`, id).Scan(&pbID, &sampleNo); err != nil {
		writeErr(w, 404, "留样不存在")
		return
	}
	u := currentUser(r)
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE retained_samples SET lab_result=$2,lab_report_no=$3,tested_at=now(),conclusion=$4 WHERE id=$1`,
		id, req.LabResult, nz(req.LabReportNo, "-"), req.Conclusion); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	eventNo := ""
	if req.LabResult == "FAIL" {
		en, _ := a.nextDocNo(r.Context(), "QE")
		tx.Exec(`INSERT INTO quality_events(event_no,type,severity,product_batch_id,description,status,raised_by)
			VALUES($1,'OFF_ODOR','HIGH',$2,$3,'OPEN',$4)`,
			en, pbID, "留样检测不合格("+sampleNo+"): "+nz(req.Conclusion, "实验室判定不合格"), u.Username)
		eventNo = en
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "lab_result": req.LabResult, "quality_event_no": eventNo})
}

// ---- 监管抽样 ----

func (a *App) createRegulatorSampling(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RegulatorName string `json:"regulator_name"`
		BatchNo       string `json:"batch_no"`
		BoxNo         string `json:"box_no"`
		SampleQtyG    int    `json:"sample_qty_g"`
		Purpose       string `json:"purpose"`
		Sampler       string `json:"sampler"`
	}
	if err := decode(r, &req); err != nil || req.RegulatorName == "" || req.BatchNo == "" || req.SampleQtyG <= 0 || req.Sampler == "" {
		writeErr(w, 400, "regulator_name/batch_no/sample_qty_g/sampler 必填")
		return
	}
	var pbID int64
	if err := a.db.QueryRow(`SELECT id FROM product_batches WHERE batch_no=$1`, req.BatchNo).Scan(&pbID); err != nil {
		writeErr(w, 404, "成品批次不存在")
		return
	}
	var boxID sql.NullInt64
	if req.BoxNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM product_boxes WHERE batch_id=$1 AND box_no=$2`, pbID, req.BoxNo).Scan(&boxID); err != nil {
			writeErr(w, 400, "箱号不存在: "+req.BoxNo)
			return
		}
	}
	sno, err := a.nextDocNo(r.Context(), "GS")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var id int64
	err = a.db.QueryRow(`INSERT INTO regulator_samplings
		(sampling_no,regulator_name,product_batch_id,box_id,sample_qty_g,purpose,sampler)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		sno, req.RegulatorName, pbID, boxID, req.SampleQtyG, req.Purpose, req.Sampler).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "sampling_no": sno, "result": "PENDING"})
}

func (a *App) listRegulatorSamplings(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT rs.id,rs.sampling_no,rs.regulator_name,rs.product_batch_id,pb.batch_no AS product_batch_no,
		rs.box_id,rs.sample_qty_g,rs.purpose,rs.sampler,rs.sampled_at,rs.result,rs.lab_report_no,rs.report_at,rs.note
		FROM regulator_samplings rs JOIN product_batches pb ON pb.id=rs.product_batch_id
		WHERE ($1='' OR rs.result=$1) ORDER BY rs.id DESC`, r.URL.Query().Get("result"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

// reportRegulatorResult 监管抽检结论；FAIL 自动开具 REGULATOR_RECALL 质量事件
func (a *App) reportRegulatorResult(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	var req struct {
		Result      string `json:"result"`
		LabReportNo string `json:"lab_report_no"`
		Note        string `json:"note"`
	}
	if err := decode(r, &req); err != nil || (req.Result != "PASS" && req.Result != "FAIL") {
		writeErr(w, 400, "result 必须为 PASS/FAIL")
		return
	}
	var pbID int64
	var samplingNo, regulator string
	if err := a.db.QueryRow(`SELECT product_batch_id,sampling_no,regulator_name FROM regulator_samplings WHERE id=$1`, id).
		Scan(&pbID, &samplingNo, &regulator); err != nil {
		writeErr(w, 404, "监管抽样记录不存在")
		return
	}
	u := currentUser(r)
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE regulator_samplings SET result=$2,lab_report_no=$3,report_at=now(),note=$4 WHERE id=$1`,
		id, req.Result, nz(req.LabReportNo, "-"), req.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	eventNo := ""
	var eventID int64
	if req.Result == "FAIL" {
		en, _ := a.nextDocNo(r.Context(), "QE")
		if err := tx.QueryRow(`INSERT INTO quality_events(event_no,type,severity,product_batch_id,description,status,raised_by)
			VALUES($1,'REGULATOR_RECALL','HIGH',$2,$3,'CONFIRMED',$4) RETURNING id`,
			en, pbID, regulator+"抽检不合格("+samplingNo+"): "+nz(req.Note, "监管报告判定不合格"), u.Username).Scan(&eventID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		eventNo = en
		tx.Exec(`UPDATE regulator_samplings SET created_event_id=$2 WHERE id=$1`, id, eventID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "result": req.Result, "quality_event_no": eventNo})
}
