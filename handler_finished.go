package main

import (
	"database/sql"
	"fmt"
	"net/http"
)

type finishedReq struct {
	WorkOrderNo  string  `json:"work_order_no"`
	QtyKg        float64 `json:"qty_kg"`
	PackSpec     string  `json:"pack_spec"`
	ChannelCode  string  `json:"channel_code"` // 渠道定制包装（可选）
	LocationCode string  `json:"location_code"`
}

// createFinishedBatch 工单产出成品：写成品批次、库存、台账、自动留样，并结单
func (s *Server) createFinishedBatch(w http.ResponseWriter, r *http.Request) {
	var req finishedReq
	if !decode(w, r, &req) || req.WorkOrderNo == "" || req.QtyKg <= 0 {
		errJSON(w, 400, "work_order_no 与 qty_kg(>0) 必填")
		return
	}
	code, payload, err := s.lockedByOrderNo(r.Context(), req.WorkOrderNo, func(tx *sql.Tx) (int, any, error) {
		woID, status, productID, err := s.resolveWO(tx, req.WorkOrderNo, 0)
		if err != nil {
			return 0, nil, apiErr(404, "工单不存在")
		}
		if status != "in_production" && status != "thawing" {
			return 0, nil, apiErr(409, "工单状态为 "+status+"，不能产出成品（需在产且未被冻结）")
		}
		var used float64
		tx.QueryRow(`SELECT COALESCE(SUM(qty_kg),0) FROM material_usages WHERE work_order_id=$1`,
			woID).Scan(&used)
		if used <= 0 {
			return 0, nil, apiErr(409, "工单尚无原料投料记录，不能产出成品")
		}
		// 渠道定制包装校验
		var channelID sql.NullInt64
		packSpec := req.PackSpec
		if req.ChannelCode != "" {
			var chID int64
			if err = tx.QueryRow(`SELECT id FROM channels WHERE code=$1`, req.ChannelCode).Scan(&chID); err != nil {
				return 0, nil, apiErr(400, "渠道不存在: "+req.ChannelCode)
			}
			var labelSpec, prodName string
			err = tx.QueryRow(`SELECT cp.label_spec,p.name FROM channel_packagings cp
				JOIN products p ON p.id=cp.product_id
				WHERE cp.channel_id=$1 AND cp.product_id=$2`, chID, productID).Scan(&labelSpec, &prodName)
			if err == sql.ErrNoRows {
				return 0, nil, apiErr(409, "渠道 "+req.ChannelCode+" 未配置该成品的定制包装")
			}
			if err != nil {
				return 0, nil, err
			}
			channelID = nullInt64(chID)
			if packSpec == "" {
				packSpec = labelSpec
			}
		}
		if packSpec == "" {
			tx.QueryRow(`SELECT COALESCE(default_pack_spec,'标准装 500g/袋') FROM products WHERE id=$1`,
				productID).Scan(&packSpec)
		}
		var locID int64
		if req.LocationCode != "" {
			locID, err = lookupID(tx, `SELECT id FROM locations WHERE code=$1`, req.LocationCode)
			if err != nil {
				return 0, nil, apiErr(400, err.Error())
			}
		} else {
			tx.QueryRow(`SELECT id FROM locations WHERE zone='FINISHED_COLD' ORDER BY id LIMIT 1`).Scan(&locID)
		}
		batchNo, _ := s.nextDocNo("FG")
		var fgID int64
		if err = tx.QueryRow(`INSERT INTO finished_batches
			(batch_no,product_id,work_order_id,qty_kg,pack_spec,pack_channel_id,status)
			VALUES ($1,$2,$3,$4,$5,$6,'produced') RETURNING id`,
			batchNo, productID, woID, req.QtyKg, packSpec, channelID).Scan(&fgID); err != nil {
			return 0, nil, err
		}
		if _, err = tx.Exec(`INSERT INTO finished_stock(finished_batch_id,location_id,qty_on_hand)
			VALUES ($1,$2,$3)`, fgID, locID, req.QtyKg); err != nil {
			return 0, nil, err
		}
		tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
			VALUES ($1,$2,$3,'PRODUCTION_IN',$4)`, fgID, req.QtyKg, req.QtyKg, batchNo)
		// 自动留样（合格后成品方可放行）
		sampleNo, _ := s.nextDocNo("SP")
		tx.Exec(`INSERT INTO sample_retentions(sample_no,finished_batch_id) VALUES ($1,$2)`,
			sampleNo, fgID)
		// 工单结单
		tx.Exec(`UPDATE work_orders SET status='finished',finished_at=COALESCE(finished_at,now())
			WHERE id=$1`, woID)
		// 同计划下工单全部完成 → 计划完成
		tx.Exec(`UPDATE production_plans p SET status='done'
			WHERE p.id=(SELECT plan_id FROM work_orders WHERE id=$1)
			AND NOT EXISTS (SELECT 1 FROM work_orders w
				WHERE w.plan_id=p.id AND w.status NOT IN ('finished','cancelled'))`, woID)
		// 收集原料批次用于缓存失效
		var rawIDs []int64
		rr, qerr := tx.Query(`SELECT DISTINCT raw_batch_id FROM material_usages WHERE work_order_id=$1`, woID)
		if qerr != nil {
			return 0, nil, fmt.Errorf("query raw ids: %w", qerr)
		}
		for rr.Next() {
			var x int64
			rr.Scan(&x)
			rawIDs = append(rawIDs, x)
		}
		rr.Close()
		return 201, map[string]any{
			"id": fgID, "batch_no": batchNo, "qty_kg": req.QtyKg,
			"pack_spec": packSpec, "sample_no": sampleNo,
			"status": "produced", "note": "已自动留样，留样合格后批次放行可售",
			"_raw_ids": rawIDs, "_fg_ids": []int64{fgID},
		}, nil
	})
	if err != nil {
		writeApiErr(w, err)
		return
	}
	if pm, ok := payload.(map[string]any); ok && s.cache != nil {
		s.cache.invalidateTrace(r.Context(), toInt64s(pm["_raw_ids"]), toInt64s(pm["_fg_ids"]))
		delete(pm, "_raw_ids")
		delete(pm, "_fg_ids")
	}
	writeJSON(w, code, payload)
}

type sampleReq struct {
	FinishedBatchNo string `json:"finished_batch_no"`
}

func (s *Server) createSample(w http.ResponseWriter, r *http.Request) {
	var req sampleReq
	if !decode(w, r, &req) || req.FinishedBatchNo == "" {
		errJSON(w, 400, "finished_batch_no 必填")
		return
	}
	var fgID int64
	if err := s.db.QueryRow(`SELECT id FROM finished_batches WHERE batch_no=$1`,
		req.FinishedBatchNo).Scan(&fgID); err != nil {
		errJSON(w, 404, "成品批次不存在")
		return
	}
	no, _ := s.nextDocNo("SP")
	var id int64
	if err := s.db.QueryRow(`INSERT INTO sample_retentions(sample_no,finished_batch_id)
		VALUES ($1,$2) RETURNING id`, no, fgID).Scan(&id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "sample_no": no, "result": "pending"})
}

type sampleResultReq struct {
	Result     string `json:"result"` // qualified / unqualified
	ReportNo   string `json:"report_no"`
	Lab        string `json:"lab"`
	Conclusion string `json:"conclusion"`
}

// resultSample 录入留样检测结论：合格→成品放行；不合格→冻结库存并生成品控事件
func (s *Server) resultSample(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	var req sampleResultReq
	if !decode(w, r, &req) || (req.Result != "qualified" && req.Result != "unqualified") {
		errJSON(w, 400, "result 必须为 qualified 或 unqualified")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var fgID int64
	var curResult string
	if err = tx.QueryRow(`SELECT finished_batch_id,result FROM sample_retentions WHERE id=$1 FOR UPDATE`,
		id).Scan(&fgID, &curResult); err != nil {
		errJSON(w, 404, "留样记录不存在")
		return
	}
	if curResult != "pending" {
		errJSON(w, 409, "留样已出结论: "+curResult)
		return
	}
	if _, err = tx.Exec(`UPDATE sample_retentions SET result=$1,report_no=$2,lab=$3,conclusion=$4,
		resulted_at=now() WHERE id=$5`, req.Result, nullString(req.ReportNo), nullString(req.Lab),
		nullString(req.Conclusion), id); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	var batchNo string
	tx.QueryRow(`SELECT batch_no FROM finished_batches WHERE id=$1`, fgID).Scan(&batchNo)
	alerts := []map[string]any{}
	if req.Result == "qualified" {
		tx.Exec(`UPDATE finished_batches SET status='released' WHERE id=$1 AND status='produced'`, fgID)
	} else {
		// 不合格：在库可售量全部转冻结，生成关键品控事件
		var onHand float64
		tx.QueryRow(`SELECT qty_on_hand FROM finished_stock WHERE finished_batch_id=$1 FOR UPDATE`,
			fgID).Scan(&onHand)
		if onHand > 0 {
			tx.Exec(`UPDATE finished_stock SET qty_on_hand=qty_on_hand-$1,qty_hold=qty_hold+$1,
				updated_at=now() WHERE finished_batch_id=$2`, onHand, fgID)
			tx.Exec(`INSERT INTO inventory_movements(finished_batch_id,change_qty,balance_after,reason,ref_no)
				VALUES ($1,$2,$3,'RECALL_HOLD',$4)`, fgID, -onHand, 0, batchNo)
		}
		en, _ := s.nextDocNo("QE")
		tx.Exec(`INSERT INTO quality_events
			(event_no,type,severity,source,finished_batch_id,description,status,created_by)
			VALUES ($1,'SAMPLE_UNQUALIFIED','critical','system',$2,$3,'open',$4)`,
			en, fgID, "成品批次 "+batchNo+" 留样检测不合格"+cPrefix(req.Conclusion),
			currentUser(r).Username)
		alerts = append(alerts, map[string]any{
			"type": "SAMPLE_UNQUALIFIED", "event_no": en,
			"message": "留样不合格，在库批次已冻结，建议发起召回评估",
		})
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if s.cache != nil {
		s.cache.invalidateTrace(r.Context(), nil, []int64{fgID})
	}
	writeJSON(w, 200, map[string]any{"sample_id": id, "result": req.Result, "alerts": alerts})
}

func (s *Server) listFinishedBatches(w http.ResponseWriter, r *http.Request) {
	q := `SELECT fb.id,fb.batch_no,p.code AS product_code,p.name AS product_name,
		fb.qty_kg,fb.pack_spec,COALESCE(c.code,'') AS channel_code,fb.status,
		w.order_no,fb.produced_at
		FROM finished_batches fb JOIN products p ON p.id=fb.product_id
		JOIN work_orders w ON w.id=fb.work_order_id
		LEFT JOIN channels c ON c.id=fb.pack_channel_id`
	args := []any{}
	if bn := r.URL.Query().Get("batch_no"); bn != "" {
		q += " WHERE fb.batch_no=$1"
		args = append(args, bn)
	}
	q += " ORDER BY fb.id DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

func (s *Server) getFinishedBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := idFromPath(r, "id")
	if !ok {
		errJSON(w, 400, "invalid id")
		return
	}
	rows, err := s.db.Query(`SELECT fb.id,fb.batch_no,p.code AS product_code,p.name AS product_name,
		fb.qty_kg,fb.pack_spec,COALESCE(c.code,'') AS channel_code,fb.status,fb.produced_at,
		w.order_no,l.code AS line_code
		FROM finished_batches fb JOIN products p ON p.id=fb.product_id
		JOIN work_orders w ON w.id=fb.work_order_id
		JOIN production_lines l ON l.id=w.line_id
		LEFT JOIN channels c ON c.id=fb.pack_channel_id
		WHERE fb.id=$1`, id)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	items := scanMaps(rows)
	rows.Close()
	if len(items) == 0 {
		errJSON(w, 404, "finished batch not found")
		return
	}
	m := items[0]
	load := func(qStr string, args ...any) []map[string]any {
		rr, e := s.db.Query(qStr, args...)
		if e != nil {
			return []map[string]any{}
		}
		defer rr.Close()
		return scanMaps(rr)
	}
	m["raw_batches"] = load(`SELECT rb.id,rb.batch_no,m.code AS material_code,
		s.code AS supplier_code, SUM(mu.qty_kg) AS qty_kg
		FROM finished_batches fb
		JOIN material_usages mu ON mu.work_order_id=fb.work_order_id
		JOIN raw_batches rb ON rb.id=mu.raw_batch_id
		JOIN materials m ON m.id=rb.material_id
		JOIN suppliers s ON s.id=rb.supplier_id
		WHERE fb.id=$1
		GROUP BY rb.id,rb.batch_no,m.code,s.code`, id)
	m["samples"] = load(`SELECT id,sample_no,result,report_no,lab,conclusion,sampled_at,resulted_at
		FROM sample_retentions WHERE finished_batch_id=$1`, id)
	m["stock"] = load(`SELECT fs.qty_on_hand,fs.qty_hold,fs.qty_quarantined,fs.qty_shipped,
		fs.qty_destroyed,fs.qty_reworked,COALESCE(l.code,'') location_code
		FROM finished_stock fs LEFT JOIN locations l ON l.id=fs.location_id
		WHERE fs.finished_batch_id=$1`, id)
	writeJSON(w, 200, m)
}
