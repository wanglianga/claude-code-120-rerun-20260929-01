package main

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/swagger
var swaggerAssets embed.FS

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// Swagger UI 与 OpenAPI 文档（无需登录，浏览器可直接打开）
	swFS, _ := fs.Sub(swaggerAssets, "web/swagger")
	mux.Handle("GET /swagger/", http.StripPrefix("/swagger/", http.FileServer(http.FS(swFS))))
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		b, _ := swaggerAssets.ReadFile("web/swagger/openapi.yaml")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		redisOK := s.rdb != nil
		writeJSON(w, 200, map[string]any{"status": "ok", "postgres": true, "redis": redisOK})
	})

	anyRole := s.auth("manager", "qc", "operator", "finance")
	managerQC := s.auth("manager", "qc")
	managerOnly := s.auth("manager")
	financeMgr := s.auth("manager", "finance")

	// ---------- 主数据 ----------
	mux.Handle("GET /api/v1/suppliers", with(s.listSuppliers, anyRole))
	mux.Handle("POST /api/v1/suppliers", with(s.idempotent(s.createSupplier), managerOnly))
	mux.Handle("GET /api/v1/materials", with(s.listMaterials, anyRole))
	mux.Handle("POST /api/v1/materials", with(s.idempotent(s.createMaterial), managerOnly))
	mux.Handle("GET /api/v1/products", with(s.listProducts, anyRole))
	mux.Handle("POST /api/v1/products", with(s.idempotent(s.createProduct), managerOnly))
	mux.Handle("GET /api/v1/lines", with(s.listLines, anyRole))
	mux.Handle("GET /api/v1/locations", with(s.listLocations, anyRole))
	mux.Handle("GET /api/v1/channels", with(s.listChannels, anyRole))
	mux.Handle("POST /api/v1/channels", with(s.idempotent(s.createChannel), managerOnly))
	mux.Handle("POST /api/v1/channels/packaging", with(s.idempotent(s.upsertChannelPackaging), managerOnly))

	// ---------- 采购入库 ----------
	mux.Handle("POST /api/v1/inbound/raw-batches", with(s.idempotent(s.createRawBatch), managerQC))
	mux.Handle("GET /api/v1/inbound/raw-batches", with(s.listRawBatches, anyRole))
	mux.Handle("GET /api/v1/inbound/raw-batches/{id}", with(s.getRawBatch, anyRole))
	mux.Handle("POST /api/v1/inbound/storage-temp-logs", with(s.recordStorageTemp, anyRole))

	// ---------- 生产计划与工单 ----------
	mux.Handle("POST /api/v1/production/plans", with(s.idempotent(s.createPlan), managerOnly))
	mux.Handle("GET /api/v1/production/plans", with(s.listPlans, anyRole))
	mux.Handle("POST /api/v1/production/work-orders", with(s.idempotent(s.createWorkOrder), managerOnly))
	mux.Handle("GET /api/v1/production/work-orders", with(s.listWorkOrders, anyRole))
	mux.Handle("GET /api/v1/production/work-orders/{id}", with(s.getWorkOrder, anyRole))

	// ---------- 解冻 ----------
	mux.Handle("POST /api/v1/production/thaw-records", with(s.idempotent(s.startThaw), anyRole))
	mux.Handle("POST /api/v1/production/thaw-records/{id}/complete", with(s.idempotent(s.completeThaw), anyRole))

	// ---------- 工序 / 投料 / 金检 / 成品 ----------
	mux.Handle("POST /api/v1/production/material-usages", with(s.idempotent(s.recordMaterialUsage), anyRole))
	mux.Handle("POST /api/v1/production/process-events", with(s.idempotent(s.recordProcessEvent), anyRole))
	mux.Handle("POST /api/v1/production/metal-checks", with(s.idempotent(s.recordMetalCheck), anyRole))
	mux.Handle("POST /api/v1/production/finished-batches", with(s.idempotent(s.createFinishedBatch), managerQC))
	mux.Handle("POST /api/v1/quality/samples", with(s.idempotent(s.createSample), anyRole))
	mux.Handle("POST /api/v1/quality/samples/{id}/result", with(s.idempotent(s.resultSample), managerQC))
	mux.Handle("GET /api/v1/production/finished-batches", with(s.listFinishedBatches, anyRole))
	mux.Handle("GET /api/v1/production/finished-batches/{id}", with(s.getFinishedBatch, anyRole))

	// ---------- 销售 ----------
	mux.Handle("POST /api/v1/sales/shipments", with(s.idempotent(s.createShipment), managerOnly))
	mux.Handle("POST /api/v1/sales/orders", with(s.idempotent(s.createOrder), managerOnly))
	mux.Handle("GET /api/v1/sales/orders", with(s.listOrders, anyRole))

	// ---------- 渠道反馈 ----------
	mux.Handle("POST /api/v1/channels/feedback", with(s.idempotent(s.createFeedback), anyRole))

	// ---------- 品控事件 ----------
	mux.Handle("POST /api/v1/quality/events", with(s.idempotent(s.createQualityEvent), anyRole))
	mux.Handle("GET /api/v1/quality/events", with(s.listQualityEvents, anyRole))
	mux.Handle("POST /api/v1/quality/events/{id}/conclusion", with(s.concludeQualityEvent, managerQC))

	// ---------- 监管抽样 ----------
	mux.Handle("POST /api/v1/regulatory/samples", with(s.idempotent(s.createRegulatorySample), managerQC))
	mux.Handle("POST /api/v1/regulatory/samples/{id}/result", with(s.idempotent(s.resultRegulatorySample), managerQC))

	// ---------- 召回 ----------
	mux.Handle("POST /api/v1/recalls", with(s.idempotent(s.createRecall), managerQC))
	mux.Handle("POST /api/v1/recalls/{id}/release", with(s.idempotent(s.releaseRecall), managerQC))
	mux.Handle("GET /api/v1/recalls", with(s.listRecalls, anyRole))
	mux.Handle("GET /api/v1/recalls/{id}", with(s.getRecall, anyRole))
	mux.Handle("GET /api/v1/recalls/{id}/documents", with(s.recallDocuments, anyRole))

	// ---------- 退货回厂 ----------
	mux.Handle("POST /api/v1/returns", with(s.idempotent(s.createReturn), anyRole))
	mux.Handle("POST /api/v1/returns/{id}/disposition", with(s.idempotent(s.decideReturn), managerQC))
	mux.Handle("POST /api/v1/returns/{id}/reinspect", with(s.idempotent(s.reinspectReturn), managerQC))
	mux.Handle("GET /api/v1/returns", with(s.listReturns, anyRole))

	// ---------- 财务 ----------
	mux.Handle("POST /api/v1/finance/compensations", with(s.idempotent(s.createCompensation), financeMgr))
	mux.Handle("POST /api/v1/finance/compensations/{id}/pay", with(s.payCompensation, financeMgr))
	mux.Handle("GET /api/v1/finance/compensations", with(s.listCompensations, anyRole))
	mux.Handle("POST /api/v1/finance/supplier-deductions", with(s.idempotent(s.createDeduction), financeMgr))
	mux.Handle("POST /api/v1/finance/supplier-deductions/{id}/settle", with(s.settleDeduction, financeMgr))
	mux.Handle("GET /api/v1/finance/supplier-deductions", with(s.listDeductions, anyRole))

	// ---------- 库存 / 盘点 ----------
	mux.Handle("GET /api/v1/inventory/stock", with(s.listStock, anyRole))
	mux.Handle("POST /api/v1/inventory/stocktakes", with(s.idempotent(s.createStocktake), managerQC))
	mux.Handle("POST /api/v1/inventory/stocktakes/{id}/count", with(s.recordStocktakeCount, anyRole))
	mux.Handle("POST /api/v1/inventory/stocktakes/{id}/confirm", with(s.confirmStocktake, managerQC))

	// ---------- 溯源 ----------
	mux.Handle("GET /api/v1/trace/raw-batch/{id}", with(s.traceRaw, anyRole))
	mux.Handle("GET /api/v1/trace/finished-batch/{id}", with(s.traceFinished, anyRole))
}
