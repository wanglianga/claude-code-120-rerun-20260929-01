package main

import "testing"

// TestRBAC 验证角色权限边界
func TestRBAC(t *testing.T) {
	srv, tok := setupTestServer(t)

	// qa 允许登记入库（admin/warehouse/qa）
	st, _ := doReq(t, srv.URL, "POST", "/api/v1/raw-batches", tok["qa"], map[string]any{
		"supplier_id": 1, "material_type": "SQUID",
		"catch_or_production_date": "2026-09-20", "inbound_temp_c": -20,
		"quarantine_cert_no": "Q-RBAC", "inspector": "qa",
	})
	if st != 201 {
		t.Fatalf("qa 应可登记入库，实际 %d", st)
	}

	// 无令牌访问受保护资源
	if st, _ := doReq(t, srv.URL, "GET", "/api/v1/dashboard", "", nil); st != 401 {
		t.Fatalf("无令牌应 401，实际 %d", st)
	}
	// 伪造令牌
	if st, _ := doReq(t, srv.URL, "GET", "/api/v1/dashboard", "fake-token", nil); st != 401 {
		t.Fatalf("伪造令牌应 401，实际 %d", st)
	}
	// 销售角色无入库权限
	if st, _ := doReq(t, srv.URL, "POST", "/api/v1/raw-batches", tok["sales"], map[string]any{
		"supplier_id": 1, "material_type": "SQUID",
		"catch_or_production_date": "2026-09-20", "inbound_temp_c": -20,
		"quarantine_cert_no": "Q2", "inspector": "sales",
	}); st != 403 {
		t.Fatalf("sales 入库应被 403 拒绝，实际 %d", st)
	}
	// 生产角色无财务扣款权限（404 单据不存在也说明进入鉴权后；这里直接对一个不存在索赔单也应先过角色）
	if st, _ := doReq(t, srv.URL, "POST", "/api/v1/supplier-claims/NOPE/deduct", tok["prod"], map[string]any{}); st != 403 {
		t.Fatalf("production 执行供应商扣款应被 403 拒绝，实际 %d", st)
	}
}
