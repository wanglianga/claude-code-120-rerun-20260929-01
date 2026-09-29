package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// 端到端集成测试：需要真实 PostgreSQL + Redis。
// 运行前由 scripts/run-tests.sh 准备独立测试库 seafood_test。
func setupTestServer(t *testing.T) (*httptest.Server, map[string]string) {
	dsn := os.Getenv("TEST_PG_DSN")
	addr := os.Getenv("TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("跳过集成测试：需要设置 TEST_PG_DSN 与 TEST_REDIS_ADDR")
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("REDIS_ADDR", addr)
	t.Setenv("REDIS_DB", "1")
	cfg := loadConfig()
	app, err := NewApp(cfg)
	if err != nil {
		t.Fatalf("初始化应用失败: %v", err)
	}
	srv := httptest.NewServer(app.routes())
	t.Cleanup(func() { srv.Close(); app.close() })

	tokens := map[string]string{}
	for _, u := range [][2]string{{"admin", "admin123"}, {"qa", "qa123"}, {"prod", "prod123"}, {"wh", "wh123"}, {"sales", "sales123"}, {"finance", "finance123"}} {
		st, body := doReq(t, srv.URL, "POST", "/api/v1/auth/login", "", map[string]any{"username": u[0], "password": u[1]})
		if st != 200 {
			t.Fatalf("登录 %s 失败: %d %v", u[0], st, body)
		}
		tokens[u[0]] = asString(body["token"])
	}
	return srv, tokens
}

func doReq(t *testing.T, base, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败 %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return resp.StatusCode, m
}

func mustStatus(t *testing.T, st, want int, body map[string]any) {
	t.Helper()
	if st != want {
		t.Fatalf("期望状态 %d，实际 %d：%v", want, st, body)
	}
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asArray(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return []any{}
}

func asFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// TestFullLifecycle 覆盖需求全部主场景
func TestFullLifecycle(t *testing.T) {
	srv, tok := setupTestServer(t)
	const (
		admin = "admin"
	)
	_ = admin
	ts := time.Now().UnixNano() / int64(time.Millisecond)
	box := func(n int) string { return fmt.Sprintf("BX%d-%d", ts, n) }

	// ---------- 1. 采购入库：正常虾仁 + 超标扇贝 ----------
	st, b := doReq(t, srv.URL, "POST", "/api/v1/raw-batches", tok["wh"], map[string]any{
		"supplier_id": 1, "material_type": "SHRIMP",
		"catch_or_production_date": "2026-09-20", "inbound_temp_c": -20,
		"quarantine_cert_no": "QC-S-0001", "inspector": "wh",
		"boxes": []map[string]any{
			{"box_no": box(1) + "-R1", "weight_kg": 20, "location_code": "DK-A-01"},
			{"box_no": box(1) + "-R2", "weight_kg": 20, "location_code": "DK-A-01"},
		},
	})
	mustStatus(t, st, 201, b)
	rmGood := asString(b["batch_no"])
	if asBool(b["temp_violation"]) {
		t.Fatal("虾仁入库温度不应超标")
	}

	st, b = doReq(t, srv.URL, "POST", "/api/v1/raw-batches", tok["wh"], map[string]any{
		"supplier_id": 2, "material_type": "SCALLOP",
		"catch_or_production_date": "2026-09-21", "inbound_temp_c": -15,
		"quarantine_cert_no": "QC-C-0002", "inspector": "wh",
		"boxes": []map[string]any{
			{"box_no": box(1) + "-R3", "weight_kg": 15, "location_code": "DK-A-02"},
			{"box_no": box(1) + "-R4", "weight_kg": 15, "location_code": "DK-A-02"},
		},
	})
	mustStatus(t, st, 201, b)
	rmBad := asString(b["batch_no"])
	if !asBool(b["temp_violation"]) || b["status"] != "ON_HOLD" || asString(b["quality_event_no"]) == "" {
		t.Fatal("扇贝入库温度超标应冻结批次并生成质量事件")
	}
	qeInbound := asString(b["quality_event_no"])

	// 仓储巡检：虾仁正常、扇贝再次超标
	st, b = doReq(t, srv.URL, "POST", "/api/v1/raw-batches/"+rmGood+"/temp-logs", tok["wh"],
		map[string]any{"phase": "STORAGE", "temp_c": -19, "recorded_by": "wh", "note": "夜班巡检"})
	mustStatus(t, st, 201, b)
	if asBool(b["violation"]) {
		t.Fatal("-19℃ 不应判违规")
	}
	st, b = doReq(t, srv.URL, "POST", "/api/v1/raw-batches/"+rmBad+"/temp-logs", tok["qa"],
		map[string]any{"phase": "STORAGE", "temp_c": -14, "recorded_by": "qa", "note": "库温回升"})
	mustStatus(t, st, 201, b)
	if !asBool(b["violation"]) {
		t.Fatal("-14℃ 应判违规")
	}

	// 质量事件列表：扇贝相关 TEMP_EXCEEDED OPEN 至少 2 个
	st, b = doReq(t, srv.URL, "GET", "/api/v1/quality-events?type=TEMP_EXCEEDED&status=OPEN", tok["qa"], nil)
	mustStatus(t, st, 200, b)
	if len(asArray(b["items"])) < 2 {
		t.Fatalf("温度超标质量事件数量不足: %v", b)
	}

	// ---------- 2. 解冻前冻结拦截：超标扇贝禁止解冻 ----------
	st, b = doReq(t, srv.URL, "POST", "/api/v1/production-plans", tok["prod"], map[string]any{
		"line_code": "L3", "product_code": "P-SC-01", "product_name": "蒜蓉粉丝扇贝",
		"planned_qty_kg": 25, "expected_seasoning_batch_no": "SEA-260901",
	})
	mustStatus(t, st, 201, b)
	planBad := asString(b["plan_no"])
	st, _ = doReq(t, srv.URL, "POST", "/api/v1/production-plans/"+planBad+"/start", tok["prod"], nil)
	if st != 200 {
		t.Fatalf("开工失败: %d", st)
	}
	st, b = doReq(t, srv.URL, "POST", "/api/v1/thaw-records", tok["prod"], map[string]any{
		"plan_no": planBad, "raw_batch_no": rmBad, "water_batch_no": "W-001",
		"operator": "prod", "raw_weight_kg": 10,
	})
	if st != 409 {
		t.Fatalf("ON_HOLD 原料解冻应被 409 拦截，实际 %d %v", st, b)
	}

	// 品控驳回质量事件 → 批次解冻
	st, _ = doReq(t, srv.URL, "POST", "/api/v1/quality-events/"+qeInbound+"/confirm", tok["qa"],
		map[string]any{"confirm": false, "conclusion": "测温探头误报，复测-20℃"})
	if st != 200 {
		t.Fatalf("驳回事件失败: %d", st)
	}
	// 另一个巡检事件也驳回，解除冻结
	st, b = doReq(t, srv.URL, "GET", "/api/v1/quality-events?status=OPEN&type=TEMP_EXCEEDED", tok["qa"], nil)
	for _, it := range asArray(b["items"]) {
		ev := asMap(it)
		if asString(ev["raw_batch_no"]) == rmBad {
			doReq(t, srv.URL, "POST", "/api/v1/quality-events/"+asString(ev["event_no"])+"/confirm", tok["qa"],
				map[string]any{"confirm": false, "conclusion": "整改后复测合格"})
		}
	}

	// ---------- 3. 两条产线并行生产 ----------
	st, b = doReq(t, srv.URL, "POST", "/api/v1/production-plans", tok["prod"], map[string]any{
		"line_code": "L1", "product_code": "P-SP-01", "product_name": "蒜香虾仁",
		"planned_qty_kg": 20, "expected_seasoning_batch_no": "SEA-260901",
	})
	mustStatus(t, st, 201, b)
	plan1 := asString(b["plan_no"])
	st, b = doReq(t, srv.URL, "POST", "/api/v1/production-plans", tok["prod"], map[string]any{
		"line_code": "L2", "product_code": "P-SP-02", "product_name": "椒盐虾仁",
		"planned_qty_kg": 10, "expected_seasoning_batch_no": "SEA-260910",
	})
	mustStatus(t, st, 201, b)
	plan2 := asString(b["plan_no"])

	if st, _ = doReq(t, srv.URL, "POST", "/api/v1/production-plans/"+plan1+"/start", tok["prod"], nil); st != 200 {
		t.Fatalf("L1 开工失败 %d", st)
	}
	if st, _ = doReq(t, srv.URL, "POST", "/api/v1/production-plans/"+plan2+"/start", tok["prod"], nil); st != 200 {
		t.Fatalf("L2 应能与 L1 并行开工，实际 %d", st)
	}

	// L1 解冻虾仁：5小时前开始，超时；中心温度合格但时长超限
	st, b = doReq(t, srv.URL, "POST", "/api/v1/thaw-records", tok["prod"], map[string]any{
		"plan_no": plan1, "raw_batch_no": rmGood, "water_batch_no": "W-101",
		"operator": "prod-A", "raw_weight_kg": 20,
		"start_at": time.Now().Add(-5 * time.Hour).Format(time.RFC3339),
	})
	mustStatus(t, st, 201, b)
	thaw1 := int(asFloat(b["id"]))
	st, b = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/thaw-records/%d/finish", thaw1), tok["prod"],
		map[string]any{"end_at": time.Now().Format(time.RFC3339), "center_temp_c": 5, "note": "解冻偏慢"})
	mustStatus(t, st, 200, b)
	if !asBool(b["timeout"]) || asString(b["quality_event_no"]) == "" {
		t.Fatal("300 分钟解冻应判超时并开质量事件")
	}

	// L2 解冻：正常
	st, b = doReq(t, srv.URL, "POST", "/api/v1/thaw-records", tok["prod"], map[string]any{
		"plan_no": plan2, "raw_batch_no": rmGood, "water_batch_no": "W-102",
		"operator": "prod-B", "raw_weight_kg": 10,
	})
	mustStatus(t, st, 201, b)
	thaw2 := int(asFloat(b["id"]))
	st, b = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/thaw-records/%d/finish", thaw2), tok["prod"],
		map[string]any{"center_temp_c": 3})
	mustStatus(t, st, 200, b)
	if asBool(b["timeout"]) {
		t.Fatal("正常解冻不应超时")
	}

	// L1 全工序：调味料用错 + 金检报警 + 包装漏气
	proc := func(plan, stage string, extra map[string]any) {
		req := map[string]any{"plan_no": plan, "stage": stage, "operator": "prod-A"}
		for k, v := range extra {
			req[k] = v
		}
		st, b := doReq(t, srv.URL, "POST", "/api/v1/processing-records", tok["prod"], req)
		mustStatus(t, st, 201, b)
	}
	alarm := true
	leak := true
	proc(plan1, "CLEAN", nil)
	proc(plan1, "CUT", nil)
	proc(plan1, "SEASON", map[string]any{"seasoning_batch_no": "SEA-260910"}) // 用错（计划 SEA-260901）
	proc(plan1, "WEIGH", map[string]any{"weight_kg": 18})
	proc(plan1, "METAL_CHECK", map[string]any{"metal_alarm": &alarm})
	proc(plan1, "PACK", map[string]any{"packaging_spec_code": "PKG-RT01", "package_leak": &leak})
	proc(plan1, "SAMPLE", map[string]any{"sample_qty_g": 125})
	proc(plan1, "COLD_IN", nil)

	st, b = doReq(t, srv.URL, "GET", "/api/v1/quality-events?status=OPEN", tok["qa"], nil)
	typeCounts := map[string]int{}
	for _, it := range asArray(b["items"]) {
		typeCounts[asString(asMap(it)["type"])]++
	}
	for _, typ := range []string{"THAW_TIMEOUT", "SEASONING_MISMATCH", "METAL_ALARM", "PACKAGE_LEAK"} {
		if typeCounts[typ] < 1 {
			t.Fatalf("缺少质量事件类型 %s，已有 %+v", typ, typeCounts)
		}
	}

	// L2 工序正常（调味料正确）
	proc2 := func(stage string, extra map[string]any) {
		req := map[string]any{"plan_no": plan2, "stage": stage, "operator": "prod-B"}
		for k, v := range extra {
			req[k] = v
		}
		st, b := doReq(t, srv.URL, "POST", "/api/v1/processing-records", tok["prod"], req)
		mustStatus(t, st, 201, b)
	}
	proc2("CLEAN", nil)
	proc2("SEASON", map[string]any{"seasoning_batch_no": "SEA-260910"})
	proc2("WEIGH", map[string]any{"weight_kg": 9})
	proc2("SAMPLE", map[string]any{"sample_qty_g": 100})
	proc2("COLD_IN", nil)

	// 完工成成品批次
	st, b = doReq(t, srv.URL, "POST", "/api/v1/production-plans/"+plan1+"/complete", tok["prod"], map[string]any{
		"cold_location_code": "CP-B-01",
		"boxes": []map[string]any{
			{"box_no": box(11), "weight_kg": 6, "packaging_spec_code": "PKG-RT01"},
			{"box_no": box(12), "weight_kg": 6, "packaging_spec_code": "PKG-RT01"},
			{"box_no": box(13), "weight_kg": 6, "packaging_spec_code": "PKG-RT01"},
		},
	})
	mustStatus(t, st, 201, b)
	if int(asFloat(b["samples"])) < 1 {
		t.Fatal("完工应自动生成留样")
	}
	pb1 := asString(b["product_batch_no"])
	b1, b2, b3 := box(11), box(12), box(13)

	st, b = doReq(t, srv.URL, "POST", "/api/v1/production-plans/"+plan2+"/complete", tok["prod"], map[string]any{
		"cold_location_code": "CP-B-02",
		"boxes": []map[string]any{
			{"box_no": box(21), "weight_kg": 9, "packaging_spec_code": "PKG-LV01"},
		},
	})
	mustStatus(t, st, 201, b)
	pb2 := asString(b["product_batch_no"])
	b4 := box(21)

	// ---------- 4. 留样检测合格 ----------
	st, b = doReq(t, srv.URL, "GET", "/api/v1/product-batches/"+pb1+"/samples", tok["qa"], nil)
	samples := asArray(b["items"])
	if len(samples) == 0 {
		t.Fatal("留样为空")
	}
	sampleID := int(asFloat(asMap(samples[0])["id"]))
	st, _ = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/samples/%d/lab-result", sampleID), tok["qa"],
		map[string]any{"lab_result": "PASS", "lab_report_no": "LAB-1", "conclusion": "微生物/感官合格"})
	if st != 200 {
		t.Fatalf("留样录入失败 %d", st)
	}

	// ---------- 5. 渠道定制包装发货 + 已售订单 + 渠道反馈 ----------
	st, b = doReq(t, srv.URL, "POST", "/api/v1/shipments", tok["sales"], map[string]any{
		"channel_code": "CH001", "product_batch_no": pb1, "box_nos": []string{b1, b2},
		"packaging_spec_code": "PKG-RT01", "vehicle_no": "鲁B12345", "operator": "sales",
	})
	mustStatus(t, st, 201, b)
	if int(asFloat(b["boxes"])) != 2 {
		t.Fatal("发货箱数应为2")
	}
	st, b = doReq(t, srv.URL, "POST", "/api/v1/shipments", tok["sales"], map[string]any{
		"channel_code": "CH004", "product_batch_no": pb2, "box_nos": []string{b4},
		"packaging_spec_code": "PKG-LV01", "operator": "sales",
	})
	mustStatus(t, st, 201, b)

	mkOrder := func(channel, ship, bn string, amt float64) string {
		st, b = doReq(t, srv.URL, "POST", "/api/v1/sales-orders", tok["sales"], map[string]any{
			"channel_code": channel, "ship_no": ship, "customer_ref": "cust-" + bn,
			"amount_cny": amt, "items": []map[string]any{{"box_no": bn, "qty": 1}},
		})
		mustStatus(t, st, 201, b)
		return asString(b["order_no"])
	}
	ship1 := func() string {
		st, b = doReq(t, srv.URL, "GET", "/api/v1/shipments?batchNo="+pb1, tok["sales"], nil)
		return asString(asMap(asArray(b["items"])[0])["ship_no"])
	}()
	ship2 := func() string {
		st, b = doReq(t, srv.URL, "GET", "/api/v1/shipments?batchNo="+pb2, tok["sales"], nil)
		return asString(asMap(asArray(b["items"])[0])["ship_no"])
	}()
	o1 := mkOrder("CH001", ship1, b1, 300)
	o2 := mkOrder("CH001", ship1, b2, 300)
	o3 := mkOrder("CH004", ship2, b4, 200)

	// 渠道反馈异味
	st, b = doReq(t, srv.URL, "POST", "/api/v1/feedbacks", tok["sales"], map[string]any{
		"channel_code": "CH001", "order_no": o2, "product_batch_no": pb1, "box_no": b2,
		"source": "CHANNEL", "type": "OFF_ODOR", "description": "消费者反映开袋有氨味",
	})
	mustStatus(t, st, 201, b)
	if asString(b["quality_event_no"]) == "" {
		t.Fatal("异味反馈应自动开质量事件")
	}
	// 直播间集中退货
	st, b = doReq(t, srv.URL, "POST", "/api/v1/feedbacks", tok["sales"], map[string]any{
		"channel_code": "CH004", "order_no": o3, "product_batch_no": pb2,
		"source": "LIVESTREAM", "type": "BATCH_RETURN", "description": "直播评论区集中投诉，申请整批退货",
	})
	mustStatus(t, st, 201, b)
	if asString(b["quality_event_no"]) == "" {
		t.Fatal("直播间集中退货应自动开质量事件")
	}

	// ---------- 6. 监管抽样不合格 → 召回 ----------
	st, b = doReq(t, srv.URL, "POST", "/api/v1/regulator-samplings", tok["qa"], map[string]any{
		"regulator_name": "市市场监管局", "batch_no": pb1, "box_no": b1,
		"sample_qty_g": 500, "purpose": "年度监督抽检", "sampler": "reg-张",
	})
	mustStatus(t, st, 201, b)
	gsID := int(asFloat(b["id"]))
	st, b = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/regulator-samplings/%d/result", gsID), tok["qa"],
		map[string]any{"result": "FAIL", "lab_report_no": "GJ-2026-09", "note": "菌落总数超标"})
	mustStatus(t, st, 200, b)
	regEvent := asString(b["quality_event_no"])
	if regEvent == "" {
		t.Fatal("监管抽检不合格应生成已确认质量事件")
	}

	// 以监管事件触发召回，范围应精准锁定 PB1（不含 PB2）
	st, b = doReq(t, srv.URL, "POST", "/api/v1/recalls", tok["qa"], map[string]any{
		"reason_type": "REGULATOR_RECALL", "level": "CLASS_I", "trigger_event_no": regEvent,
		"regulator_ref_no": "GJ-2026-09", "description": "监管抽检菌落总数超标，责令召回",
		"action": "STOP_SALE_RETURN",
	})
	mustStatus(t, st, 201, b)
	rc1 := asString(b["recall_no"])
	if int(asFloat(b["affected_product_batches"])) != 1 {
		t.Fatalf("监管事件召回应仅圈定 PB1，实际 %v", b)
	}
	st, b = doReq(t, srv.URL, "POST", "/api/v1/recalls/"+rc1+"/release", tok["qa"], nil)
	mustStatus(t, st, 200, b)
	if int(asFloat(b["frozen_in_stock_boxes"])) != 1 {
		t.Fatalf("应冻结在库 1 箱(B3)，实际 %v", b)
	}
	if int(asFloat(b["affected_shipments"])) != 1 || int(asFloat(b["affected_orders"])) != 2 {
		t.Fatalf("应影响 1 发货单 2 订单，实际 %v", b)
	}

	// 召回详情：精准到渠道、订单、箱、留样
	st, b = doReq(t, srv.URL, "GET", "/api/v1/recalls/"+rc1, tok["qa"], nil)
	mustStatus(t, st, 200, b)
	batches := asArray(b["batches"])
	if len(batches) != 1 {
		t.Fatalf("RC1 应只有1个成品批次")
	}
	pb1Detail := asMap(batches[0])
	if asString(pb1Detail["batch_no"]) != pb1 {
		t.Fatal("召回批次错误")
	}
	orders := asArray(pb1Detail["sold_orders"])
	if len(orders) != 2 {
		t.Fatalf("应定位到2个已售订单，实际 %d", len(orders))
	}
	for _, o := range orders {
		if len(asArray(asMap(o)["boxes"])) != 1 {
			t.Fatal("订单应精准到箱")
		}
	}
	if len(asArray(pb1Detail["shipped_channels"])) != 1 {
		t.Fatal("应定位已发货渠道")
	}
	samples2 := asArray(pb1Detail["samples"])
	if len(samples2) == 0 || asString(asMap(samples2[0])["lab_result"]) != "PASS" {
		t.Fatal("召回详情应含留样合格结果")
	}
	docs := asArray(b["documents"])
	if len(docs) < 3 {
		t.Fatalf("发布应自动生成停售/退货/监管报告/追溯材料，实际 %d", len(docs))
	}

	// 批次冻结期间禁止再发货
	st, _ = doReq(t, srv.URL, "POST", "/api/v1/shipments", tok["sales"], map[string]any{
		"channel_code": "CH002", "product_batch_no": pb1, "box_nos": []string{b3}})
	if st != 409 {
		t.Fatalf("召回冻结批次发货应被拦截，实际 %d", st)
	}

	// ---------- 7. 退货回厂三态处置，库存不失真 ----------
	recv := func(rc, bn, source string) int {
		st, b := doReq(t, srv.URL, "POST", "/api/v1/returns", tok["wh"],
			map[string]any{"recall_no": rc, "box_no": bn, "source": source, "received_by": "wh"})
		mustStatus(t, st, 201, b)
		return int(asFloat(b["id"]))
	}
	rB1 := recv(rc1, b1, "CHANNEL_RETURN")
	rB2 := recv(rc1, b2, "CHANNEL_RETURN")
	rB3 := recv(rc1, b3, "WAREHOUSE_RECALL")

	// 可复检 → 合格 → 重新入库
	st, _ = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/returns/%d/decision", rB1), tok["qa"],
		map[string]any{"decision": "RETEST"})
	if st != 200 {
		t.Fatalf("复检决定失败 %d", st)
	}
	st, _ = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/returns/%d/retest-result", rB1), tok["qa"],
		map[string]any{"result": "PASS", "note": "复检合格，可重新入库"})
	if st != 200 {
		t.Fatalf("复检录入失败 %d", st)
	}
	st, b = doReq(t, srv.URL, "GET", "/api/v1/stock/boxes?status=IN_STOCK&location_code=CP-B-01", tok["wh"], nil)
	found := false
	for _, x := range asArray(b["items"]) {
		if asString(asMap(x)["box_no"]) == b1 {
			found = true
		}
	}
	if !found {
		t.Fatal("复检合格箱应回到成品冷库在库")
	}

	// 必须销毁
	st, _ = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/returns/%d/decision", rB2), tok["qa"],
		map[string]any{"decision": "DESTROY", "note": "异味确认，销毁"})
	if st != 200 {
		t.Fatalf("销毁决定失败 %d", st)
	}

	// 可返工 → 返工完成回库
	st, _ = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/returns/%d/decision", rB3), tok["qa"],
		map[string]any{"decision": "REWORK", "note": "重新调味包装"})
	if st != 200 {
		t.Fatalf("返工决定失败 %d", st)
	}
	st, _ = doReq(t, srv.URL, "POST", fmt.Sprintf("/api/v1/returns/%d/decision", rB3), tok["qa"],
		map[string]any{"decision": "REWORK", "note": "返工完成"})
	if st != 200 {
		t.Fatalf("返工完成失败 %d", st)
	}
	st, b = doReq(t, srv.URL, "GET", "/api/v1/returns/summary", tok["qa"], nil)
	mustStatus(t, st, 200, b)
	if !asBool(asMap(b["stock_integrity"])["consistent"]) {
		t.Fatalf("处置中库存状态应一致: %v", b)
	}

	// ---------- 8. 供应商追责扣款 + 客服赔付 + 召回成本 ----------
	st, b = doReq(t, srv.URL, "POST", "/api/v1/supplier-claims", tok["finance"], map[string]any{
		"supplier_code": "SUP001", "recall_no": rc1, "raw_batch_no": rmGood,
		"reason": "原料菌落总数超标导致监管召回", "claim_amount": 8000,
	})
	mustStatus(t, st, 201, b)
	claimNo := asString(b["claim_no"])
	st, _ = doReq(t, srv.URL, "POST", "/api/v1/supplier-claims/"+claimNo+"/deduct", tok["finance"],
		map[string]any{"deduct_amount": 6000, "payment_ref": "9月货款抵扣", "blacklist_supplier": true})
	if st != 200 {
		t.Fatalf("供应商扣款失败 %d", st)
	}
	st, b = doReq(t, srv.URL, "GET", "/api/v1/suppliers", tok["admin"], nil)
	for _, s := range asArray(b["items"]) {
		if asString(asMap(s)["code"]) == "SUP001" && !asBool(asMap(s)["blacklisted"]) {
			t.Fatal("扣款后供应商应被拉黑")
		}
	}

	st, b = doReq(t, srv.URL, "POST", "/api/v1/compensations", tok["finance"], map[string]any{
		"recall_no": rc1, "order_no": o1, "amount_cny": 300, "reason": "召回先行赔付",
	})
	mustStatus(t, st, 201, b)
	compNo := asString(b["comp_no"])
	st, _ = doReq(t, srv.URL, "POST", "/api/v1/compensations/"+compNo+"/pay", tok["finance"], nil)
	if st != 200 {
		t.Fatalf("赔付支付失败 %d", st)
	}

	st, b = doReq(t, srv.URL, "GET", "/api/v1/finance/recall-cost/"+rc1, tok["finance"], nil)
	mustStatus(t, st, 200, b)
	if asFloat(b["supplier_deducted_recovered"]) != 6000 || asFloat(b["customer_compensation_paid"]) != 300 {
		t.Fatalf("召回成本汇总错误: %v", b)
	}
	if asFloat(b["destroyed_boxes"]) != 1 {
		t.Fatalf("应统计1箱销毁: %v", b)
	}

	// ---------- 9. 正反向谱系 ----------
	st, b = doReq(t, srv.URL, "GET", "/api/v1/trace/raw/"+rmGood, tok["qa"], nil)
	mustStatus(t, st, 200, b)
	if len(asArray(b["downstream_products"])) != 2 {
		t.Fatalf("虾仁原料应追溯到2个成品批次，实际 %v", b["downstream_products"])
	}
	if !asBool(asMap(b["samples_summary"])["all_pass"]) {
		t.Fatal("留样汇总应判定全部合格（PB2 无留样、PB1 PASS）")
	}
	st, b = doReq(t, srv.URL, "GET", "/api/v1/trace/product/"+pb1, tok["qa"], nil)
	mustStatus(t, st, 200, b)
	if len(asArray(b["raw_materials"])) != 1 || asString(asMap(asArray(b["raw_materials"])[0])["batch_no"]) != rmGood {
		t.Fatal("成品反查原料失败")
	}
	if len(asArray(b["channel_feedback"])) < 1 {
		t.Fatal("成品谱系应包含渠道反馈")
	}

	// ---------- 10. 按原料批次发起二次召回：谱系扩散到 PB1+PB2 ----------
	st, b = doReq(t, srv.URL, "POST", "/api/v1/recalls", tok["qa"], map[string]any{
		"reason_type": "OFF_ODOR", "level": "CLASS_II", "raw_batch_no": rmGood,
		"description": "虾仁原料批次疑受污染，按原料批次扩大召回全部成品",
	})
	mustStatus(t, st, 201, b)
	rc2 := asString(b["recall_no"])
	if int(asFloat(b["affected_product_batches"])) != 2 {
		t.Fatalf("按原料召回应扩散到2个成品批次，实际 %v", b)
	}
	st, b = doReq(t, srv.URL, "POST", "/api/v1/recalls/"+rc2+"/release", tok["qa"], nil)
	mustStatus(t, st, 200, b)
	// PB2 的直播订单被召回；PB1 订单已赔付/召回不再重复统计
	if int(asFloat(b["affected_orders"])) != 1 {
		t.Fatalf("二次召回应影响 PB2 的1个订单，实际 %v", b)
	}
	// B1 复检合格回库 + B3 返工回库被再次冻结 = 2
	if int(asFloat(b["frozen_in_stock_boxes"])) != 2 {
		t.Fatalf("二次召回应冻结2箱(B1复检回库+B3返工回库)，实际 %v", b)
	}
	st, b = doReq(t, srv.URL, "POST", "/api/v1/recalls/"+rc2+"/documents/TRACE_REPORT", tok["qa"], nil)
	mustStatus(t, st, 201, b)
	if asString(b["content"]) == "" {
		t.Fatal("追溯报告内容为空")
	}

	// ---------- 11. 冷库盘点 ----------
	st, b = doReq(t, srv.URL, "POST", "/api/v1/stocktakes", tok["wh"], map[string]any{
		"location_code": "CP-B-01", "counted_by": "wh", "note": "月末盘点",
		"actuals": []map[string]any{
			{"box_kind": "PRODUCT", "box_no": b1, "actual_kg": 5.9, "found": true},
		},
	})
	mustStatus(t, st, 201, b)
	takeNo := asString(b["take_no"])
	if int(asFloat(b["lines"])) != 2 {
		t.Fatalf("CP-B-01 应有2箱账面对象(B1,B3)，实际 %v", b)
	}
	st, b = doReq(t, srv.URL, "POST", "/api/v1/stocktakes/"+takeNo+"/confirm", tok["wh"], nil)
	mustStatus(t, st, 200, b)

	// ---------- 12. 仪表盘 ----------
	st, b = doReq(t, srv.URL, "GET", "/api/v1/dashboard", tok["admin"], nil)
	mustStatus(t, st, 200, b)
	if asFloat(b["recalls_total"]) < 2 || asFloat(b["destroyed_boxes"]) < 1 {
		t.Fatalf("看板统计错误: %v", b)
	}
	t.Logf("全链路场景验证通过: %s %s, 召回 %s/%s", pb1, pb2, rc1, rc2)
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
