package main

import (
	"database/sql"
	"net/http"
	"time"
)

// ---------- 生产计划 ----------

type planReq struct {
	ProductCode string  `json:"product_code"`
	PlannedQty  float64 `json:"planned_qty"`
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	var req planReq
	if !decode(w, r, &req) || req.ProductCode == "" || req.PlannedQty <= 0 {
		errJSON(w, 400, "product_code 与 planned_qty(>0) 必填")
		return
	}
	pid, err := s.getIDByCode("products", "code", req.ProductCode)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	no, _ := s.nextDocNo("PL")
	var id int64
	if err = s.db.QueryRow(`INSERT INTO production_plans(plan_no,product_id,planned_qty)
		VALUES ($1,$2,$3) RETURNING id`, no, pid, req.PlannedQty).Scan(&id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "plan_no": no})
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.db.Query(`SELECT p.id,p.plan_no,p2.code AS product_code,p2.name AS product_name,
		p.planned_qty,p.status,p.created_at FROM production_plans p
		JOIN products p2 ON p2.id=p.product_id ORDER BY p.id DESC`)
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

// ---------- 工单（多生产线并行） ----------

type woReq struct {
	PlanNo     string  `json:"plan_no"`
	LineCode   string  `json:"line_code"`
	PlannedQty float64 `json:"planned_qty"`
}

func (s *Server) createWorkOrder(w http.ResponseWriter, r *http.Request) {
	var req woReq
	if !decode(w, r, &req) || req.PlanNo == "" || req.LineCode == "" || req.PlannedQty <= 0 {
		errJSON(w, 400, "plan_no/line_code/planned_qty(>0) 必填")
		return
	}
	var planID, productID int64
	var planStatus string
	if err := s.db.QueryRow(`SELECT id,product_id,status FROM production_plans WHERE plan_no=$1`,
		req.PlanNo).Scan(&planID, &productID, &planStatus); err == sql.ErrNoRows {
		errJSON(w, 400, "生产计划不存在: "+req.PlanNo)
		return
	}
	lineID, err := s.getIDByCode("production_lines", "code", req.LineCode)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	no, _ := s.nextDocNo("WO")
	tx, _ := s.db.Begin()
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRow(`INSERT INTO work_orders(order_no,plan_id,line_id,product_id,planned_qty)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		no, planID, lineID, productID, req.PlannedQty).Scan(&id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	tx.Exec(`UPDATE production_plans SET status='in_progress' WHERE id=$1 AND status='issued'`, planID)
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "order_no": no, "line_code": req.LineCode})
}

func (s *Server) resolveWO(tx *sql.Tx, orderNo string, id int64) (int64, string, int64, error) {
	var woID, productID int64
	var status string
	if id > 0 {
		err := tx.QueryRow(`SELECT id,status,product_id FROM work_orders WHERE id=$1`, id).
			Scan(&woID, &status, &productID)
		return woID, status, productID, err
	}
	err := tx.QueryRow(`SELECT id,status,product_id FROM work_orders WHERE order_no=$1`, orderNo).
		Scan(&woID, &status, &productID)
	return woID, status, productID, err
}

func (s *Server) listWorkOrders(w http.ResponseWriter, r *http.Request) {
	q := `SELECT w.id,w.order_no,p.plan_no,l.code AS line_code,p2.code AS product_code,
		w.planned_qty,w.status,w.started_at,w.finished_at
		FROM work_orders w JOIN production_plans p ON p.id=w.plan_id
		JOIN production_lines l ON l.id=w.line_id
		JOIN products p2 ON p2.id=w.product_id ORDER BY w.id DESC`
	rows, err := s.db.Query(q)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

func (s *Server) getWorkOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var woID int64
	var orderNo, planNo, lineCode, productCode, status string
	var planned float64
	var started, finished sql.NullTime
	err := s.db.QueryRow(`SELECT w.id,w.order_no,p.plan_no,l.code AS line_code,p2.code AS product_code,
		w.planned_qty,w.status,w.started_at,w.finished_at
		FROM work_orders w JOIN production_plans p ON p.id=w.plan_id
		JOIN production_lines l ON l.id=w.line_id JOIN products p2 ON p2.id=w.product_id
		WHERE w.id=$1`, id).
		Scan(&woID, &orderNo, &planNo, &lineCode, &productCode, &planned, &status, &started, &finished)
	if err != nil {
		errJSON(w, 404, "work order not found")
		return
	}
	m := map[string]any{"id": woID, "order_no": orderNo, "plan_no": planNo, "line_code": lineCode,
		"product_code": productCode, "planned_qty": planned, "status": status,
		"started_at": started.Time, "finished_at": finished.Time}

	load := func(q string, args ...any) []map[string]any {
		rows, e := s.db.Query(q, args...)
		if e != nil {
			return []map[string]any{}
		}
		defer rows.Close()
		return scanMaps(rows)
	}
	m["material_usages"] = load(`SELECT rb.batch_no,m.code AS material_code,mu.qty_kg,mu.used_at,mu.operator
		FROM material_usages mu JOIN raw_batches rb ON rb.id=mu.raw_batch_id
		JOIN materials m ON m.id=rb.material_id WHERE mu.work_order_id=$1`, id)
	m["process_events"] = load(`SELECT stage,operator,occurred_at,detail FROM process_events
		WHERE work_order_id=$1 ORDER BY id`, id)
	m["metal_checks"] = load(`SELECT result,operator,checked_at,note FROM metal_detector_checks
		WHERE work_order_id=$1 ORDER BY id`, id)
	m["thaw_records"] = load(`SELECT t.id,rb.batch_no,t.start_at,t.end_at,t.start_center_temp_c,
		t.end_center_temp_c,t.max_center_temp_c,t.water_batch_no,t.operator,t.duration_minutes,t.timed_out
		FROM thaw_records t JOIN raw_batches rb ON rb.id=t.raw_batch_id
		WHERE t.work_order_id=$1 ORDER BY t.id`, id)
	m["finished_batches"] = load(`SELECT fb.batch_no,fb.qty_kg,fb.status,fb.produced_at
		FROM finished_batches fb WHERE fb.work_order_id=$1`, id)
	writeJSON(w, 200, m)
}

// ---------- 解冻 ----------

type thawStartReq struct {
	WorkOrderNo      string  `json:"work_order_no"`
	RawBatchNo       string  `json:"raw_batch_no"`
	StartAt          string  `json:"start_at"`
	StartCenterTempC float64 `json:"start_center_temp_c"`
	WaterBatchNo     string  `json:"water_batch_no"`
	Operator         string  `json:"operator"`
}

func (s *Server) startThaw(w http.ResponseWriter, r *http.Request) {
	var req thawStartReq
	if !decode(w, r, &req) || req.WorkOrderNo == "" || req.RawBatchNo == "" ||
		req.WaterBatchNo == "" || req.Operator == "" {
		errJSON(w, 400, "work_order_no/raw_batch_no/water_batch_no/operator 必填")
		return
	}
	code, payload, err := s.lockedByOrderNo(r.Context(), req.WorkOrderNo, func(tx *sql.Tx) (int, any, error) {
		woID, status, _, err := s.resolveWO(tx, req.WorkOrderNo, 0)
		if err != nil {
			return 0, nil, apiErr(404, "工单不存在: "+req.WorkOrderNo)
		}
		if status == "on_hold" {
			return 0, nil, apiErr(409, "工单处于冻结(on_hold)状态，需品控结论后放行")
		}
		if status == "finished" || status == "cancelled" {
			return 0, nil, apiErr(409, "工单已 "+status)
		}
		var rbID int64
		var rbStatus string
		if err = tx.QueryRow(`SELECT id,status FROM raw_batches WHERE batch_no=$1`,
			req.RawBatchNo).Scan(&rbID, &rbStatus); err != nil {
			return 0, nil, apiErr(404, "原料批次不存在: "+req.RawBatchNo)
		}
		if rbStatus == "quarantined" {
			return 0, nil, apiErr(409, "原料批次处于隔离状态，不得投入解冻: "+req.RawBatchNo)
		}
		if rbStatus == "consumed" || rbStatus == "destroyed" || rbStatus == "recalled" {
			return 0, nil, apiErr(409, "原料批次状态为 "+rbStatus+"，不可解冻")
		}
		startAt := parseTime(req.StartAt)
		var id int64
		if err = tx.QueryRow(`INSERT INTO thaw_records
			(work_order_id,raw_batch_id,start_at,start_center_temp_c,water_batch_no,operator)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			woID, rbID, startAt, req.StartCenterTempC, req.WaterBatchNo, req.Operator).Scan(&id); err != nil {
			return 0, nil, err
		}
		tx.Exec(`UPDATE work_orders SET status='thawing', started_at=COALESCE(started_at,$1)
			WHERE id=$2 AND status IN ('issued','thawing','in_production')`, startAt, woID)
		tx.Exec(`UPDATE raw_batches SET status='in_production' WHERE id=$1 AND status='in_storage'`, rbID)
		return 201, map[string]any{
			"id": id, "work_order_no": req.WorkOrderNo, "raw_batch_no": req.RawBatchNo,
			"start_at": startAt, "operator": req.Operator,
		}, nil
	})
	if err != nil {
		writeApiErr(w, err)
		return
	}
	writeJSON(w, code, payload)
}

type thawCompleteReq struct {
	EndAt      string  `json:"end_at"`
	EndCenterC float64 `json:"end_center_temp_c"`
	MaxCenterC float64 `json:"max_center_temp_c"`
}

func (s *Server) completeThaw(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var req thawCompleteReq
	if !decode(w, r, &req) {
		return
	}
	code, payload, err := s.lockedByThaw(r.Context(), id, func(tx *sql.Tx) (int, any, error) {
		var woID, rbID int64
		var startAt time.Time
		var endAt sql.NullTime
		if err := tx.QueryRow(`SELECT work_order_id,raw_batch_id,start_at,end_at FROM thaw_records WHERE id=$1`,
			id).Scan(&woID, &rbID, &startAt, &endAt); err != nil {
			return 0, nil, apiErr(404, "解冻记录不存在")
		}
		if endAt.Valid {
			return 0, nil, apiErr(409, "解冻记录已完结")
		}
		end := parseTime(req.EndAt)
		if end.Before(startAt) {
			return 0, nil, apiErr(400, "结束时间早于开始时间")
		}
		dur := int(end.Sub(startAt).Minutes())
		maxC := req.MaxCenterC
		if maxC == 0 {
			maxC = req.EndCenterC
		}
		timedOut := false
		maxDur := s.settingFloat("thaw_max_duration_min", 720)
		maxTemp := s.settingFloat("thaw_center_temp_max_c", 10)
		var alerts []map[string]any
		if float64(dur) > maxDur {
			timedOut = true
			en, _ := s.nextDocNo("QE")
			if err := createEventTx(tx, en, "THAW_TIMEOUT", "major", "system",
				nullInt64(rbID), sql.NullInt64{}, nullInt64(woID), sql.NullInt64{},
				"解冻时长 "+itoa(dur)+" 分钟，超过上限 "+itoa(int(maxDur))+" 分钟",
				currentUser(r).Username); err != nil {
				return 0, nil, err
			}
			alerts = append(alerts, map[string]any{"type": "THAW_TIMEOUT", "event_no": en})
		}
		if maxC > maxTemp {
			en, _ := s.nextDocNo("QE")
			if err := createEventTx(tx, en, "THAW_TEMP_OVER", "critical", "system",
				nullInt64(rbID), sql.NullInt64{}, nullInt64(woID), sql.NullInt64{},
				"解冻中心温度 "+strconvFormat(maxC)+"℃ 超过上限 "+strconvFormat(maxTemp)+"℃，工单冻结待品控处置",
				currentUser(r).Username); err != nil {
				return 0, nil, err
			}
			tx.Exec(`UPDATE work_orders SET status='on_hold' WHERE id=$1`, woID)
			alerts = append(alerts, map[string]any{"type": "THAW_TEMP_OVER", "event_no": en})
		}
		if _, err := tx.Exec(`UPDATE thaw_records SET end_at=$1,end_center_temp_c=$2,max_center_temp_c=$3,
			duration_minutes=$4,timed_out=$5 WHERE id=$6`,
			end, req.EndCenterC, maxC, dur, timedOut, id); err != nil {
			return 0, nil, err
		}
		tx.Exec(`INSERT INTO thaw_temp_logs(thaw_id,center_temp_c,read_at) VALUES ($1,$2,$3)`,
			id, maxC, end)
		return 200, map[string]any{
			"id": id, "duration_minutes": dur, "timed_out": timedOut, "alerts": alerts,
		}, nil
	})
	if err != nil {
		writeApiErr(w, err)
		return
	}
	writeJSON(w, code, payload)
}

// ---------- 投料 ----------

type usageReq struct {
	WorkOrderNo string  `json:"work_order_no"`
	RawBatchNo  string  `json:"raw_batch_no"`
	BoxNo       string  `json:"box_no"`
	QtyKg       float64 `json:"qty_kg"`
	Operator    string  `json:"operator"`
}

func (s *Server) recordMaterialUsage(w http.ResponseWriter, r *http.Request) {
	var req usageReq
	if !decode(w, r, &req) || req.WorkOrderNo == "" || req.RawBatchNo == "" || req.QtyKg <= 0 {
		errJSON(w, 400, "work_order_no/raw_batch_no/qty_kg(>0) 必填")
		return
	}
	if req.Operator == "" {
		req.Operator = currentUser(r).Username
	}
	code, payload, err := s.lockedByOrderNo(r.Context(), req.WorkOrderNo, func(tx *sql.Tx) (int, any, error) {
		woID, status, _, err := s.resolveWO(tx, req.WorkOrderNo, 0)
		if err != nil {
			return 0, nil, apiErr(404, "工单不存在")
		}
		if status == "on_hold" {
			return 0, nil, apiErr(409, "工单冻结中，不能投料")
		}
		if status == "finished" || status == "cancelled" {
			return 0, nil, apiErr(409, "工单已 "+status)
		}
		var rbID int64
		var rbStatus string
		if err = tx.QueryRow(`SELECT id,status FROM raw_batches WHERE batch_no=$1`,
			req.RawBatchNo).Scan(&rbID, &rbStatus); err != nil {
			return 0, nil, apiErr(404, "原料批次不存在")
		}
		if rbStatus != "in_storage" && rbStatus != "in_production" {
			return 0, nil, apiErr(409, "原料批次状态 "+rbStatus+" 不可投料")
		}
		var boxID sql.NullInt64
		if req.BoxNo != "" {
			var boxStatus string
			var boxWeight float64
			if err = tx.QueryRow(`SELECT id,weight_kg,status FROM raw_boxes
				WHERE raw_batch_id=$1 AND box_no=$2`, rbID, req.BoxNo).
				Scan(&boxID, &boxWeight, &boxStatus); err != nil {
				return 0, nil, apiErr(404, "箱号不存在: "+req.BoxNo)
			}
			if boxStatus != "in_storage" {
				// quarantined 箱也禁止使用
				return 0, nil, apiErr(409, "箱 "+req.BoxNo+" 状态为 "+boxStatus+"，不可投料")
			}
			var used float64
			tx.QueryRow(`SELECT COALESCE(SUM(qty_kg),0) FROM material_usages WHERE raw_box_id=$1`,
				boxID.Int64).Scan(&used)
			if used+req.QtyKg > boxWeight+0.001 {
				return 0, nil, apiErr(409, "投料量超过箱剩余重量")
			}
			if used+req.QtyKg >= boxWeight-0.001 {
				tx.Exec(`UPDATE raw_boxes SET status='used' WHERE id=$1`, boxID.Int64)
			}
		}
		if _, err = tx.Exec(`INSERT INTO material_usages(work_order_id,raw_batch_id,raw_box_id,qty_kg,operator)
			VALUES ($1,$2,$3,$4,$5)`, woID, rbID, boxID, req.QtyKg, req.Operator); err != nil {
			return 0, nil, err
		}
		tx.Exec(`UPDATE raw_batches SET status='in_production' WHERE id=$1`, rbID)
		var remain int
		tx.QueryRow(`SELECT COUNT(*) FROM raw_boxes WHERE raw_batch_id=$1 AND status='in_storage'`,
			rbID).Scan(&remain)
		if remain == 0 {
			tx.Exec(`UPDATE raw_batches SET status='consumed' WHERE id=$1 AND status='in_production'`, rbID)
		}
		tx.Exec(`UPDATE work_orders SET status='in_production',
			started_at=COALESCE(started_at,now()) WHERE id=$1 AND status IN ('issued','thawing')`, woID)
		return 201, map[string]any{
			"work_order_no": req.WorkOrderNo, "raw_batch_no": req.RawBatchNo,
			"box_no": req.BoxNo, "qty_kg": req.QtyKg,
		}, nil
	})
	if err != nil {
		writeApiErr(w, err)
		return
	}
	writeJSON(w, code, payload)
}

// ---------- 工序事件（清洗/切配/调味/称重/包装） ----------

type processReq struct {
	WorkOrderNo string         `json:"work_order_no"`
	Stage       string         `json:"stage"`
	Operator    string         `json:"operator"`
	OccurredAt  string         `json:"occurred_at"`
	Detail      map[string]any `json:"detail"`
}

var validStages = map[string]bool{
	"WASHING": true, "CUTTING": true, "SEASONING": true, "WEIGHING": true, "PACKAGING": true,
}

func (s *Server) recordProcessEvent(w http.ResponseWriter, r *http.Request) {
	var req processReq
	if !decode(w, r, &req) || req.WorkOrderNo == "" || !validStages[req.Stage] {
		errJSON(w, 400, "work_order_no 与合法 stage(WASHING/CUTTING/SEASONING/WEIGHING/PACKAGING) 必填")
		return
	}
	if req.Operator == "" {
		req.Operator = currentUser(r).Username
	}
	detail := req.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	by := currentUser(r).Username
	code, payload, err := s.lockedByOrderNo(r.Context(), req.WorkOrderNo, func(tx *sql.Tx) (int, any, error) {
		woID, status, productID, err := s.resolveWO(tx, req.WorkOrderNo, 0)
		if err != nil {
			return 0, nil, apiErr(404, "工单不存在")
		}
		if status == "on_hold" {
			return 0, nil, apiErr(409, "工单冻结中")
		}
		if status == "finished" || status == "cancelled" {
			return 0, nil, apiErr(409, "工单已 "+status)
		}
		var alerts []map[string]any
		if req.Stage == "SEASONING" {
			sc, _ := detail["seasoning_code"].(string)
			if sc == "" {
				return 0, nil, apiErr(400, "调味工序必须在 detail.seasoning_code 提供调味料批次/编码")
			}
			var okAllowed bool
			tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM product_seasonings ps
				JOIN seasonings sn ON sn.id=ps.seasoning_id
				WHERE ps.product_id=$1 AND sn.code=$2)`, productID, sc).Scan(&okAllowed)
			if !okAllowed {
				en, _ := s.nextDocNo("QE")
				if err = createEventTx(tx, en, "SEASONING_WRONG", "critical", "system",
					sql.NullInt64{}, sql.NullInt64{}, nullInt64(woID), sql.NullInt64{},
					"工单使用了非该成品允许的调味料批次/编码: "+sc, by); err != nil {
					return 0, nil, err
				}
				tx.Exec(`UPDATE work_orders SET status='on_hold' WHERE id=$1`, woID)
				alerts = append(alerts, map[string]any{
					"type": "SEASONING_WRONG", "event_no": en,
					"message": "调味料批次用错，工单已冻结",
				})
			}
		}
		if req.Stage == "PACKAGING" {
			if cc, _ := detail["channel_code"].(string); cc != "" {
				var chID int64
				if err = tx.QueryRow(`SELECT id FROM channels WHERE code=$1`, cc).Scan(&chID); err != nil {
					return 0, nil, apiErr(400, "渠道编码不存在: "+cc)
				}
				var spec string
				err = tx.QueryRow(`SELECT label_spec FROM channel_packagings
					WHERE channel_id=$1 AND product_id=$2`, chID, productID).Scan(&spec)
				if err == sql.ErrNoRows {
					return 0, nil, apiErr(409, "渠道 "+cc+" 未配置该成品的定制包装")
				}
				detail["label_spec"] = spec
			}
		}
		occurred := parseTime(req.OccurredAt)
		var id int64
		if err = tx.QueryRow(`INSERT INTO process_events(work_order_id,stage,operator,occurred_at,detail)
			VALUES ($1,$2,$3,$4,$5::jsonb) RETURNING id`,
			woID, req.Stage, req.Operator, occurred, jsonString(detail)).Scan(&id); err != nil {
			return 0, nil, err
		}
		return 201, map[string]any{"id": id, "stage": req.Stage, "alerts": alerts}, nil
	})
	if err != nil {
		writeApiErr(w, err)
		return
	}
	writeJSON(w, code, payload)
}

// ---------- 金检 ----------

type metalReq struct {
	WorkOrderNo string `json:"work_order_no"`
	Result      string `json:"result"`
	Operator    string `json:"operator"`
	Note        string `json:"note"`
}

func (s *Server) recordMetalCheck(w http.ResponseWriter, r *http.Request) {
	var req metalReq
	if !decode(w, r, &req) || req.WorkOrderNo == "" ||
		(req.Result != "PASS" && req.Result != "ALARM") {
		errJSON(w, 400, "work_order_no 与 result(PASS/ALARM) 必填")
		return
	}
	if req.Operator == "" {
		req.Operator = currentUser(r).Username
	}
	by := currentUser(r).Username
	code, payload, err := s.lockedByOrderNo(r.Context(), req.WorkOrderNo, func(tx *sql.Tx) (int, any, error) {
		woID, status, _, err := s.resolveWO(tx, req.WorkOrderNo, 0)
		if err != nil {
			return 0, nil, apiErr(404, "工单不存在")
		}
		if status == "on_hold" {
			return 0, nil, apiErr(409, "工单冻结中，金检工序暂停，需品控结论后放行")
		}
		if status == "finished" || status == "cancelled" {
			return 0, nil, apiErr(409, "工单已完结")
		}
		var id int64
		if err = tx.QueryRow(`INSERT INTO metal_detector_checks(work_order_id,result,operator,note)
			VALUES ($1,$2,$3,$4) RETURNING id`, woID, req.Result, req.Operator, req.Note).Scan(&id); err != nil {
			return 0, nil, err
		}
		alerts := []map[string]any{}
		if req.Result == "ALARM" {
			en, _ := s.nextDocNo("QE")
			if err = createEventTx(tx, en, "METAL_ALARM", "critical", "system",
				sql.NullInt64{}, sql.NullInt64{}, nullInt64(woID), sql.NullInt64{},
				"金检报警"+cPrefix(req.Note), by); err != nil {
				return 0, nil, err
			}
			tx.Exec(`UPDATE work_orders SET status='on_hold' WHERE id=$1`, woID)
			alerts = append(alerts, map[string]any{
				"type": "METAL_ALARM", "event_no": en, "message": "金检报警，工单已冻结待品控处置",
			})
		}
		return 201, map[string]any{"id": id, "result": req.Result, "alerts": alerts}, nil
	})
	if err != nil {
		writeApiErr(w, err)
		return
	}
	writeJSON(w, code, payload)
}
