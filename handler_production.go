package main

import (
	"database/sql"
	"net/http"
	"time"
)

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Now(), nil
	}
	return time.Parse(time.RFC3339, s)
}

// ---- 生产计划 ----

func (a *App) createPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LineCode     string  `json:"line_code"`
		ProductCode  string  `json:"product_code"`
		ProductName  string  `json:"product_name"`
		PlannedQtyKg float64 `json:"planned_qty_kg"`
		SeasoningNo  string  `json:"expected_seasoning_batch_no"`
	}
	if err := decode(r, &req); err != nil || req.LineCode == "" || req.ProductCode == "" || req.ProductName == "" {
		writeErr(w, 400, "line_code/product_code/product_name 必填")
		return
	}
	var lineID int64
	if err := a.db.QueryRow(`SELECT id FROM production_lines WHERE code=$1 AND active`, req.LineCode).Scan(&lineID); err != nil {
		writeErr(w, 400, "产线不存在或已停用: "+req.LineCode)
		return
	}
	var seaID sql.NullInt64
	if req.SeasoningNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM seasoning_batches WHERE batch_no=$1`, req.SeasoningNo).Scan(&seaID); err != nil {
			writeErr(w, 400, "调味料批次不存在: "+req.SeasoningNo)
			return
		}
	}
	planNo, err := a.nextDocNo(r.Context(), "PP")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	var id int64
	err = a.db.QueryRow(`INSERT INTO production_plans
		(plan_no,line_id,product_code,product_name,planned_qty_kg,expected_seasoning_batch_id,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		planNo, lineID, req.ProductCode, req.ProductName, req.PlannedQtyKg, seaID, u.Username).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "plan_no": planNo, "line_code": req.LineCode})
}

func (a *App) listPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT p.id,p.plan_no,p.line_id,l.code AS line_code,l.name AS line_name,
		p.product_code,p.product_name,p.planned_qty_kg,p.status,p.created_by,p.created_at,p.started_at,p.done_at
		FROM production_plans p JOIN production_lines l ON l.id=p.line_id
		WHERE ($1='' OR p.status=$1) ORDER BY p.id DESC`, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) startPlan(w http.ResponseWriter, r *http.Request) {
	planNo := r.PathValue("planNo")
	var id, lineID int64
	var status string
	if err := a.db.QueryRow(`SELECT id,line_id,status FROM production_plans WHERE plan_no=$1`, planNo).Scan(&id, &lineID, &status); err != nil {
		writeErr(w, 404, "生产计划不存在")
		return
	}
	if status != "ISSUED" {
		writeErr(w, 409, "计划状态为 "+status+"，不可开工")
		return
	}
	// 同一产线同时只能有一个在制计划；不同产线可并行
	var cnt int
	if err := a.db.QueryRow(`SELECT count(*) FROM production_plans WHERE line_id=$1 AND status='IN_PROGRESS'`, lineID).Scan(&cnt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if cnt > 0 {
		writeErr(w, 409, "该产线已有在制计划，支持多产线并行但同一产线需先完工")
		return
	}
	if _, err := a.db.Exec(`UPDATE production_plans SET status='IN_PROGRESS', started_at=COALESCE(started_at,now()) WHERE id=$1`, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"plan_no": planNo, "status": "IN_PROGRESS"})
}

// ---- 解冻 ----

func (a *App) createThaw(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanNo       string  `json:"plan_no"`
		RawBatchNo   string  `json:"raw_batch_no"`
		WaterBatchNo string  `json:"water_batch_no"`
		Operator     string  `json:"operator"`
		StartAt      string  `json:"start_at"`
		RawWeightKg  float64 `json:"raw_weight_kg"`
		Note         string  `json:"note"`
	}
	if err := decode(r, &req); err != nil || req.PlanNo == "" || req.RawBatchNo == "" || req.WaterBatchNo == "" || req.Operator == "" {
		writeErr(w, 400, "plan_no/raw_batch_no/water_batch_no/operator 必填")
		return
	}
	var planID, lineID int64
	var pstatus string
	if err := a.db.QueryRow(`SELECT id,line_id,status FROM production_plans WHERE plan_no=$1`, req.PlanNo).Scan(&planID, &lineID, &pstatus); err != nil {
		writeErr(w, 404, "生产计划不存在")
		return
	}
	if pstatus != "IN_PROGRESS" {
		writeErr(w, 409, "计划未开工，不能解冻")
		return
	}
	var rawID int64
	var rstatus string
	var totalKg float64
	if err := a.db.QueryRow(`SELECT id,status,total_weight_kg FROM raw_material_batches WHERE batch_no=$1`, req.RawBatchNo).
		Scan(&rawID, &rstatus, &totalKg); err != nil {
		writeErr(w, 404, "原料批次不存在")
		return
	}
	if rstatus == "ON_HOLD" {
		writeErr(w, 409, "原料批次温度超标待检冻结(ON_HOLD)，不得解冻，待品控确认质量事件")
		return
	}
	if rstatus == "CONSUMED" || rstatus == "CLOSED" {
		writeErr(w, 409, "原料批次已耗尽/关闭")
		return
	}
	if req.RawWeightKg > 0 && req.RawWeightKg > totalKg+0.01 {
		// 允许超投提示但不阻断；这里按严格校验
		writeErr(w, 409, "解冻投料重量超过批次库存重量")
		return
	}
	start, err := parseTime(req.StartAt)
	if err != nil {
		writeErr(w, 400, "start_at 需为 RFC3339 时间")
		return
	}
	var id int64
	err = a.db.QueryRow(`INSERT INTO thaw_records(plan_id,line_id,raw_batch_id,water_batch_no,operator,start_at,raw_weight_kg,note)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		planID, lineID, rawID, req.WaterBatchNo, req.Operator, start, req.RawWeightKg, req.Note).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "plan_no": req.PlanNo, "raw_batch_no": req.RawBatchNo, "started_at": start})
}

func (a *App) finishThaw(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id := atoi64(idStr)
	var req struct {
		EndAt      string  `json:"end_at"`
		CenterTemp float64 `json:"center_temp_c"`
		Note       string  `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求体格式错误: "+err.Error())
		return
	}
	var planID, rawID int64
	var startAt time.Time
	if err := a.db.QueryRow(`SELECT plan_id,raw_batch_id,start_at FROM thaw_records WHERE id=$1`, id).Scan(&planID, &rawID, &startAt); err != nil {
		writeErr(w, 404, "解冻记录不存在")
		return
	}
	end, err := parseTime(req.EndAt)
	if err != nil {
		writeErr(w, 400, "end_at 需为 RFC3339 时间")
		return
	}
	dur := int(end.Sub(startAt).Minutes())
	if dur < 0 {
		writeErr(w, 400, "结束时间早于开始时间")
		return
	}
	var maxMin int
	var centerLimit float64
	a.db.QueryRow(`SELECT value::int FROM system_config WHERE key='thaw_max_minutes'`).Scan(&maxMin)
	a.db.QueryRow(`SELECT value::float8 FROM system_config WHERE key='thaw_center_temp_limit_c'`).Scan(&centerLimit)
	timeout := dur > maxMin || req.CenterTemp > centerLimit

	u := currentUser(r)
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE thaw_records SET end_at=$2,center_temp_c=$3,duration_min=$4,timeout=$5,note=COALESCE(NULLIF($6,''),note) WHERE id=$1`,
		id, end, req.CenterTemp, dur, timeout, req.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	eventNo := ""
	if timeout {
		en, _ := a.nextDocNo(r.Context(), "QE")
		desc := "解冻超时：用时 " + itoa(int64(dur)) + " 分钟(限值 " + itoa(int64(maxMin)) + " 分钟)"
		if req.CenterTemp > centerLimit {
			desc += "；中心温度 " + ftoa(req.CenterTemp) + "℃ 高于限值 " + ftoa(centerLimit) + "℃"
		}
		tx.Exec(`INSERT INTO quality_events(event_no,type,severity,raw_batch_id,thaw_id,description,status,raised_by)
			VALUES($1,'THAW_TIMEOUT','HIGH',$2,$3,$4,'OPEN',$5)`, en, rawID, id, desc, u.Username)
		eventNo = en
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "duration_min": dur, "timeout": timeout, "quality_event_no": eventNo})
}

// ---- 加工工序 ----

var stageSeq = map[string]int{
	"CLEAN": 1, "CUT": 2, "SEASON": 3, "WEIGH": 4, "PACK": 5, "METAL_CHECK": 6, "SAMPLE": 7, "COLD_IN": 8,
}

func (a *App) createProcessing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanNo        string  `json:"plan_no"`
		Stage         string  `json:"stage"`
		Operator      string  `json:"operator"`
		StartedAt     string  `json:"started_at"`
		EndedAt       string  `json:"ended_at"`
		SeasoningNo   string  `json:"seasoning_batch_no"`
		WeightKg      float64 `json:"weight_kg"`
		PackagingCode string  `json:"packaging_spec_code"`
		PackageLeak   *bool   `json:"package_leak"`
		MetalAlarm    *bool   `json:"metal_alarm"`
		SampleQtyG    int     `json:"sample_qty_g"`
		Result        string  `json:"result"`
		Note          string  `json:"note"`
	}
	if err := decode(r, &req); err != nil || req.PlanNo == "" || req.Stage == "" || req.Operator == "" {
		writeErr(w, 400, "plan_no/stage/operator 必填")
		return
	}
	seq, ok := stageSeq[req.Stage]
	if !ok {
		writeErr(w, 400, "stage 必须为 CLEAN/CUT/SEASON/WEIGH/PACK/METAL_CHECK/SAMPLE/COLD_IN")
		return
	}
	var planID, lineID int64
	var expectedSea sql.NullInt64
	var pstatus string
	if err := a.db.QueryRow(`SELECT id,line_id,status,expected_seasoning_batch_id FROM production_plans WHERE plan_no=$1`, req.PlanNo).
		Scan(&planID, &lineID, &pstatus, &expectedSea); err != nil {
		writeErr(w, 404, "生产计划不存在")
		return
	}
	if pstatus != "IN_PROGRESS" {
		writeErr(w, 409, "计划未处于生产中")
		return
	}

	started, _ := parseTime(req.StartedAt)
	var ended sql.NullTime
	if req.EndedAt != "" {
		t, err := time.Parse(time.RFC3339, req.EndedAt)
		if err != nil {
			writeErr(w, 400, "ended_at 需为 RFC3339 时间")
			return
		}
		ended = sql.NullTime{Time: t, Valid: true}
	}

	var seaID sql.NullInt64
	if req.SeasoningNo != "" {
		if err := a.db.QueryRow(`SELECT id FROM seasoning_batches WHERE batch_no=$1`, req.SeasoningNo).Scan(&seaID); err != nil {
			writeErr(w, 400, "调味料批次不存在: "+req.SeasoningNo)
			return
		}
	}
	var pkgID sql.NullInt64
	if req.PackagingCode != "" {
		if err := a.db.QueryRow(`SELECT id FROM packaging_specs WHERE code=$1`, req.PackagingCode).Scan(&pkgID); err != nil {
			writeErr(w, 400, "包装规格不存在: "+req.PackagingCode)
			return
		}
	}

	result := "OK"
	if req.Result == "NG" {
		result = "NG"
	}
	var leak, alarm sql.NullBool
	if req.PackageLeak != nil {
		leak = sql.NullBool{Bool: *req.PackageLeak, Valid: true}
		if *req.PackageLeak {
			result = "NG"
		}
	}
	if req.MetalAlarm != nil {
		alarm = sql.NullBool{Bool: *req.MetalAlarm, Valid: true}
		if *req.MetalAlarm {
			result = "NG"
		}
	}

	u := currentUser(r)
	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()

	var recID int64
	err := tx.QueryRow(`INSERT INTO processing_records
		(plan_id,line_id,stage,seq,operator,started_at,ended_at,seasoning_batch_id,weight_kg,packaging_spec_id,package_leak,metal_alarm,sample_qty_g,result,note)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
		planID, lineID, req.Stage, seq, req.Operator, started, ended, seaID, nzf(req.WeightKg), pkgID, leak, alarm,
		nzInt(req.SampleQtyG), result, req.Note).Scan(&recID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	events := []map[string]any{}
	addEvent := func(etype, desc string, sev string) {
		en, _ := a.nextDocNo(r.Context(), "QE")
		tx.Exec(`INSERT INTO quality_events(event_no,type,severity,raw_batch_id,processing_id,description,status,raised_by)
			SELECT $1,$2,$3,rb.raw_batch_id,$4,$5,'OPEN',$6
			FROM (SELECT DISTINCT raw_batch_id FROM thaw_records WHERE plan_id=$7 LIMIT 1) rb`,
			en, etype, sev, recID, desc, u.Username, planID)
		events = append(events, map[string]any{"event_no": en, "type": etype})
	}

	// 调味料批次用错（与计划指定批次不一致）
	if req.Stage == "SEASON" && expectedSea.Valid && seaID.Valid && seaID.Int64 != expectedSea.Int64 {
		result = "NG"
		tx.Exec(`UPDATE processing_records SET result='NG' WHERE id=$1`, recID)
		var expectedNo, actualNo string
		a.db.QueryRow(`SELECT batch_no FROM seasoning_batches WHERE id=$1`, expectedSea.Int64).Scan(&expectedNo)
		a.db.QueryRow(`SELECT batch_no FROM seasoning_batches WHERE id=$1`, seaID.Int64).Scan(&actualNo)
		addEvent("SEASONING_MISMATCH",
			"调味料批次用错：计划要求 "+expectedNo+"，实际投放 "+actualNo, "HIGH")
	}
	if req.Stage == "METAL_CHECK" && req.MetalAlarm != nil && *req.MetalAlarm {
		addEvent("METAL_ALARM", "金检报警，相关在制品需隔离排查", "HIGH")
	}
	if req.Stage == "PACK" && req.PackageLeak != nil && *req.PackageLeak {
		addEvent("PACKAGE_LEAK", "包装漏气检测不合格", "MEDIUM")
	}

	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": recID, "stage": req.Stage, "result": result, "quality_events": events})
}

func nzf(f float64) sql.NullFloat64 {
	if f == 0 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: f, Valid: true}
}

func nzInt(n int) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(n), Valid: true}
}

// ---- 计划完工：生成成品批次/箱号/谱系/留样，转入成品冷库 ----

func (a *App) completePlan(w http.ResponseWriter, r *http.Request) {
	planNo := r.PathValue("planNo")
	var req struct {
		ColdLocation string `json:"cold_location_code"`
		Boxes        []struct {
			BoxNo         string  `json:"box_no"`
			WeightKg      float64 `json:"weight_kg"`
			PackagingCode string  `json:"packaging_spec_code"`
		} `json:"boxes"`
		Note string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求体格式错误: "+err.Error())
		return
	}
	var planID, lineID int64
	var status, productCode, productName string
	var planned float64
	if err := a.db.QueryRow(`SELECT id,line_id,status,product_code,product_name,planned_qty_kg FROM production_plans WHERE plan_no=$1`,
		planNo).Scan(&planID, &lineID, &status, &productCode, &productName, &planned); err != nil {
		writeErr(w, 404, "生产计划不存在")
		return
	}
	if status != "IN_PROGRESS" {
		writeErr(w, 409, "计划状态为 "+status+"，不可完工")
		return
	}

	// 解冻记录必须存在（谱系来源）
	thawRows, err := a.db.Query(`SELECT raw_batch_id, SUM(raw_weight_kg) FROM thaw_records WHERE plan_id=$1 GROUP BY raw_batch_id`, planID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type rawUse struct {
		id int64
		kg float64
	}
	uses := []rawUse{}
	for thawRows.Next() {
		var u rawUse
		var kg sql.NullFloat64
		if err := thawRows.Scan(&u.id, &kg); err == nil {
			u.kg = kg.Float64
			uses = append(uses, u)
		}
	}
	thawRows.Close()
	if len(uses) == 0 {
		writeErr(w, 409, "该计划无解冻投料记录，无法生成成品批次")
		return
	}

	var locID int64
	locCode := req.ColdLocation
	if locCode == "" {
		locCode = "CP-B-01"
	}
	if err := a.db.QueryRow(`SELECT id FROM storage_locations WHERE code=$1 AND zone='FINISHED_COLD'`, locCode).Scan(&locID); err != nil {
		writeErr(w, 400, "成品冷库库位不存在: "+locCode)
		return
	}

	// 成品重量取称重工序，缺省用计划量
	var qty sql.NullFloat64
	_ = a.db.QueryRow(`SELECT SUM(weight_kg) FROM processing_records WHERE plan_id=$1 AND stage='WEIGH'`, planID).Scan(&qty)
	qtyKg := qty.Float64
	if qtyKg == 0 {
		qtyKg = planned
	}

	tx, _ := a.db.BeginTx(r.Context(), nil)
	defer tx.Rollback()

	pbNo, _ := a.nextDocNo(r.Context(), "PB")
	var pbID int64
	if err := tx.QueryRow(`INSERT INTO product_batches(batch_no,plan_id,line_id,product_code,product_name,qty_kg,cold_location_id,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,'COLD_STORAGE') RETURNING id`,
		pbNo, planID, lineID, productCode, productName, qtyKg, locID).Scan(&pbID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// 谱系
	for _, use := range uses {
		if _, err := tx.Exec(`INSERT INTO material_genealogy(product_batch_id,raw_batch_id,qty_consumed_kg) VALUES($1,$2,$3)
			ON CONFLICT(product_batch_id,raw_batch_id) DO UPDATE SET qty_consumed_kg=material_genealogy.qty_consumed_kg+EXCLUDED.qty_consumed_kg`,
			pbID, use.id, use.kg); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// 原料批次消耗状态
		var consumed, total sql.NullFloat64
		tx.QueryRow(`SELECT COALESCE(SUM(qty_consumed_kg),0) FROM material_genealogy WHERE raw_batch_id=$1`, use.id).Scan(&consumed)
		tx.QueryRow(`SELECT total_weight_kg FROM raw_material_batches WHERE id=$1`, use.id).Scan(&total)
		newStatus := "RELEASED"
		if consumed.Float64+0.01 >= total.Float64 {
			newStatus = "CONSUMED"
		}
		tx.Exec(`UPDATE raw_material_batches SET status=$2 WHERE id=$1 AND status IN ('FROZEN_IN','RELEASED','ON_HOLD')`, use.id, newStatus)
	}

	// 成品箱
	totalBoxKg := 0.0
	for _, b := range req.Boxes {
		var pkg sql.NullInt64
		if b.PackagingCode != "" {
			if err := tx.QueryRow(`SELECT id FROM packaging_specs WHERE code=$1`, b.PackagingCode).Scan(&pkg); err != nil {
				writeErr(w, 400, "包装规格不存在: "+b.PackagingCode)
				return
			}
		}
		if b.BoxNo == "" {
			writeErr(w, 400, "成品箱 box_no 不能为空")
			return
		}
		if _, err := tx.Exec(`INSERT INTO product_boxes(batch_id,box_no,weight_kg,location_id,packaging_spec_id,status)
			VALUES($1,$2,$3,$4,$5,'IN_STOCK')`, pbID, b.BoxNo, b.WeightKg, locID, pkg); err != nil {
			writeErr(w, 409, "成品箱号录入失败("+b.BoxNo+"): "+err.Error())
			return
		}
		totalBoxKg += b.WeightKg
	}

	// 留样：从 SAMPLE 工序自动生成留样记录
	sampleRows, _ := tx.Query(`SELECT sample_qty_g,operator FROM processing_records WHERE plan_id=$1 AND stage='SAMPLE' AND sample_qty_g IS NOT NULL`, planID)
	type sample struct {
		g  int
		op string
	}
	samples := []sample{}
	for sampleRows.Next() {
		var g sql.NullInt64
		var op string
		if sampleRows.Scan(&g, &op) == nil {
			samples = append(samples, sample{int(g.Int64), op})
		}
	}
	sampleRows.Close()
	for i, s := range samples {
		sno, _ := a.nextDocNo(r.Context(), "RS")
		_ = sno
		_ = i
		var sid int64
		if err := tx.QueryRow(`INSERT INTO retained_samples(product_batch_id,sample_no,qty_g,storage_location,retained_by)
			VALUES($1,$2,$3,$4,$5) RETURNING id`, pbID, sno, s.g, locCode+"留样柜", s.op).Scan(&sid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	if _, err := tx.Exec(`UPDATE production_plans SET status='DONE', done_at=now() WHERE id=$1`, planID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// 成品箱净重缺省时以称重重量兜底回写（无箱明细时只生成批次）
	if len(req.Boxes) == 0 {
		a.db.Exec(`UPDATE product_batches SET qty_kg=$2 WHERE id=$1`, pbID, qtyKg)
	}
	_ = totalBoxKg
	writeJSON(w, 201, map[string]any{
		"product_batch_no": pbNo, "plan_no": planNo, "qty_kg": qtyKg,
		"boxes": len(req.Boxes), "samples": len(samples),
		"cold_location": locCode, "status": "COLD_STORAGE",
	})
}

// ---- 成品批次查询 ----

func (a *App) listProductBatches(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT pb.id,pb.batch_no,pb.plan_id,p.plan_no,pb.line_id,l.code AS line_code,
		pb.product_code,pb.product_name,pb.qty_kg,pb.status,pb.produced_at,
		loc.code AS cold_location
		FROM product_batches pb
		JOIN production_plans p ON p.id=pb.plan_id
		JOIN production_lines l ON l.id=pb.line_id
		LEFT JOIN storage_locations loc ON loc.id=pb.cold_location_id
		WHERE ($1='' OR pb.batch_no=$1) AND ($2='' OR pb.status=$3)
		ORDER BY pb.id DESC`,
		r.URL.Query().Get("batchNo"), r.URL.Query().Get("status"), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) getProductBatch(w http.ResponseWriter, r *http.Request) {
	batchNo := r.PathValue("batchNo")
	batch, err := a.queryOneMap(`SELECT pb.*, p.plan_no, l.code AS line_code, loc.code AS cold_location
		FROM product_batches pb
		JOIN production_plans p ON p.id=pb.plan_id
		JOIN production_lines l ON l.id=pb.line_id
		LEFT JOIN storage_locations loc ON loc.id=pb.cold_location_id
		WHERE pb.batch_no=$1`, batchNo)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "成品批次不存在")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	pbID := batch["id"]

	boxRows, _ := a.db.Query(`SELECT b.id,b.box_no,b.weight_kg,b.status,b.reworked,
		COALESCE(l.code,'') AS location_code, COALESCE(ps.code,'') AS packaging_spec_code
		FROM product_boxes b
		LEFT JOIN storage_locations l ON l.id=b.location_id
		LEFT JOIN packaging_specs ps ON ps.id=b.packaging_spec_id
		WHERE b.batch_id=$1 ORDER BY b.box_no`, pbID)
	boxes := scanRows(boxRows)
	boxRows.Close()

	rawRows, _ := a.db.Query(`SELECT g.raw_batch_id,rb.batch_no,rb.material_type,s.name AS supplier_name,g.qty_consumed_kg
		FROM material_genealogy g
		JOIN raw_material_batches rb ON rb.id=g.raw_batch_id
		JOIN suppliers s ON s.id=rb.supplier_id
		WHERE g.product_batch_id=$1`, pbID)
	raws := scanRows(rawRows)
	rawRows.Close()

	sampleRows, _ := a.db.Query(`SELECT id,sample_no,qty_g,lab_result,lab_report_no,tested_at,conclusion,retained_at
		FROM retained_samples WHERE product_batch_id=$1 ORDER BY id`, pbID)
	samples := scanRows(sampleRows)
	sampleRows.Close()

	writeJSON(w, 200, map[string]any{"batch": batch, "boxes": boxes, "raw_materials": raws, "samples": samples})
}
