package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed web/*
var webFS embed.FS

//go:embed openapi.yaml
var openapiSpec []byte

type App struct {
	db  *sql.DB
	rdb *redis.Client
}

func NewApp(cfg Config) (*App, error) {
	db, err := sql.Open("postgres", cfg.PGDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)

	// 等待数据库就绪（compose 中数据库可能晚启动）
	for i := 0; i < 30; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}

	rdb := newRedisClient(cfg)
	for i := 0; i < 30; i++ {
		if err = rdb.Ping(context.Background()).Err(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("连接 Redis 失败: %w", err)
	}

	a := &App{db: db, rdb: rdb}
	if err := a.migrate(); err != nil {
		return nil, err
	}
	if err := a.seed(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) close() {
	_ = a.db.Close()
	_ = a.rdb.Close()
}

func (a *App) migrate() error {
	if _, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := a.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("迁移 %s 失败: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(filename) VALUES($1)`, name); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		fmt.Println("已执行迁移:", name)
	}
	return nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("POST /api/v1/auth/login", a.login)

	// 文档
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(openapiSpec)
	})
	webSub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /swagger/", http.StripPrefix("/swagger/", http.FileServer(http.FS(webSub))))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeErr(w, 404, "资源不存在")
			return
		}
		http.Redirect(w, r, "/swagger/", http.StatusFound)
	})

	api := "/api/v1"

	// 主数据
	mux.HandleFunc("POST "+api+"/suppliers", a.requireRole(a.createSupplier, "admin"))
	mux.HandleFunc("GET "+api+"/suppliers", a.requireRole(a.listSuppliers, "admin", "qa", "production", "warehouse", "sales", "finance", "regulator"))
	mux.HandleFunc("POST "+api+"/channels", a.requireRole(a.createChannel, "admin"))
	mux.HandleFunc("GET "+api+"/channels", a.requireAuth(a.listChannels))
	mux.HandleFunc("POST "+api+"/production-lines", a.requireRole(a.createLine, "admin"))
	mux.HandleFunc("GET "+api+"/production-lines", a.requireAuth(a.listLines))
	mux.HandleFunc("POST "+api+"/storage-locations", a.requireRole(a.createLocation, "admin", "warehouse"))
	mux.HandleFunc("GET "+api+"/storage-locations", a.requireAuth(a.listLocations))
	mux.HandleFunc("POST "+api+"/seasoning-batches", a.requireRole(a.createSeasoningBatch, "admin", "warehouse"))
	mux.HandleFunc("GET "+api+"/seasoning-batches", a.requireAuth(a.listSeasoningBatches))
	mux.HandleFunc("POST "+api+"/packaging-specs", a.requireRole(a.createPackagingSpec, "admin", "sales"))
	mux.HandleFunc("GET "+api+"/packaging-specs", a.requireAuth(a.listPackagingSpecs))
	mux.HandleFunc("GET "+api+"/config", a.requireAuth(a.getConfig))
	mux.HandleFunc("PUT "+api+"/config", a.requireRole(a.updateConfig, "admin", "qa"))

	// 采购入库 / 仓储温控
	mux.HandleFunc("POST "+api+"/raw-batches", a.requireRole(a.createRawBatch, "admin", "warehouse", "qa"))
	mux.HandleFunc("GET "+api+"/raw-batches", a.requireAuth(a.listRawBatches))
	mux.HandleFunc("GET "+api+"/raw-batches/{batchNo}", a.requireAuth(a.getRawBatch))
	mux.HandleFunc("POST "+api+"/raw-batches/{batchNo}/boxes", a.requireRole(a.addRawBoxes, "admin", "warehouse"))
	mux.HandleFunc("POST "+api+"/raw-batches/{batchNo}/temp-logs", a.requireRole(a.addTempLog, "admin", "warehouse", "qa"))

	// 生产计划 / 解冻 / 工序 / 成品
	mux.HandleFunc("POST "+api+"/production-plans", a.requireRole(a.createPlan, "admin", "production"))
	mux.HandleFunc("GET "+api+"/production-plans", a.requireAuth(a.listPlans))
	mux.HandleFunc("POST "+api+"/production-plans/{planNo}/start", a.requireRole(a.startPlan, "admin", "production"))
	mux.HandleFunc("POST "+api+"/thaw-records", a.requireRole(a.createThaw, "admin", "production", "qa"))
	mux.HandleFunc("POST "+api+"/thaw-records/{id}/finish", a.requireRole(a.finishThaw, "admin", "production", "qa"))
	mux.HandleFunc("POST "+api+"/processing-records", a.requireRole(a.createProcessing, "admin", "production", "qa"))
	mux.HandleFunc("POST "+api+"/production-plans/{planNo}/complete", a.requireRole(a.completePlan, "admin", "production", "qa"))
	mux.HandleFunc("GET "+api+"/product-batches", a.requireAuth(a.listProductBatches))
	mux.HandleFunc("GET "+api+"/product-batches/{batchNo}", a.requireAuth(a.getProductBatch))

	// 留样 / 监管抽样
	mux.HandleFunc("POST "+api+"/product-batches/{batchNo}/samples", a.requireRole(a.createSample, "admin", "qa"))
	mux.HandleFunc("GET "+api+"/product-batches/{batchNo}/samples", a.requireAuth(a.listSamples))
	mux.HandleFunc("POST "+api+"/samples/{id}/lab-result", a.requireRole(a.reportLabResult, "admin", "qa"))
	mux.HandleFunc("POST "+api+"/regulator-samplings", a.requireRole(a.createRegulatorSampling, "admin", "qa", "regulator"))
	mux.HandleFunc("GET "+api+"/regulator-samplings", a.requireAuth(a.listRegulatorSamplings))
	mux.HandleFunc("POST "+api+"/regulator-samplings/{id}/result", a.requireRole(a.reportRegulatorResult, "admin", "qa", "regulator"))

	// 冷库库存 / 盘点
	mux.HandleFunc("GET "+api+"/stock/boxes", a.requireAuth(a.listStockBoxes))
	mux.HandleFunc("GET "+api+"/stock/summary", a.requireAuth(a.stockSummary))
	mux.HandleFunc("POST "+api+"/stocktakes", a.requireRole(a.createStocktake, "admin", "warehouse"))
	mux.HandleFunc("GET "+api+"/stocktakes", a.requireAuth(a.listStocktakes))
	mux.HandleFunc("POST "+api+"/stocktakes/{takeNo}/confirm", a.requireRole(a.confirmStocktake, "admin", "warehouse"))

	// 发货 / 订单 / 渠道反馈
	mux.HandleFunc("POST "+api+"/shipments", a.requireRole(a.createShipment, "admin", "sales", "warehouse"))
	mux.HandleFunc("GET "+api+"/shipments", a.requireAuth(a.listShipments))
	mux.HandleFunc("POST "+api+"/sales-orders", a.requireRole(a.createOrder, "admin", "sales"))
	mux.HandleFunc("GET "+api+"/sales-orders", a.requireAuth(a.listOrders))
	mux.HandleFunc("POST "+api+"/feedbacks", a.requireRole(a.createFeedback, "admin", "sales", "qa", "production"))
	mux.HandleFunc("GET "+api+"/feedbacks", a.requireAuth(a.listFeedbacks))

	// 质量事件
	mux.HandleFunc("POST "+api+"/quality-events", a.requireAuth(a.createQualityEvent))
	mux.HandleFunc("GET "+api+"/quality-events", a.requireAuth(a.listQualityEvents))
	mux.HandleFunc("POST "+api+"/quality-events/{eventNo}/confirm", a.requireRole(a.confirmQualityEvent, "admin", "qa"))

	// 召回
	mux.HandleFunc("POST "+api+"/recalls", a.requireRole(a.createRecall, "admin", "qa"))
	mux.HandleFunc("GET "+api+"/recalls", a.requireAuth(a.listRecalls))
	mux.HandleFunc("GET "+api+"/recalls/{recallNo}", a.requireAuth(a.getRecall))
	mux.HandleFunc("POST "+api+"/recalls/{recallNo}/release", a.requireRole(a.releaseRecall, "admin", "qa"))
	mux.HandleFunc("GET "+api+"/recalls/{recallNo}/documents", a.requireAuth(a.listRecallDocuments))
	mux.HandleFunc("POST "+api+"/recalls/{recallNo}/documents/{docType}", a.requireRole(a.generateRecallDocument, "admin", "qa", "sales", "finance"))

	// 退货回厂分级处置
	mux.HandleFunc("POST "+api+"/returns", a.requireRole(a.receiveReturn, "admin", "warehouse", "qa"))
	mux.HandleFunc("GET "+api+"/returns", a.requireAuth(a.listReturns))
	mux.HandleFunc("POST "+api+"/returns/{id}/decision", a.requireRole(a.decideReturn, "admin", "qa"))
	mux.HandleFunc("POST "+api+"/returns/{id}/retest-result", a.requireRole(a.retestReturn, "admin", "qa"))
	mux.HandleFunc("GET "+api+"/returns/summary", a.requireAuth(a.returnsSummary))

	// 财务：供应商追责/扣款、客服赔付
	mux.HandleFunc("POST "+api+"/supplier-claims", a.requireRole(a.createSupplierClaim, "admin", "finance", "qa"))
	mux.HandleFunc("GET "+api+"/supplier-claims", a.requireAuth(a.listSupplierClaims))
	mux.HandleFunc("POST "+api+"/supplier-claims/{claimNo}/deduct", a.requireRole(a.deductSupplierClaim, "admin", "finance"))
	mux.HandleFunc("POST "+api+"/compensations", a.requireRole(a.createCompensation, "admin", "finance", "sales"))
	mux.HandleFunc("GET "+api+"/compensations", a.requireAuth(a.listCompensations))
	mux.HandleFunc("POST "+api+"/compensations/{compNo}/pay", a.requireRole(a.payCompensation, "admin", "finance"))
	mux.HandleFunc("GET "+api+"/finance/recall-cost/{recallNo}", a.requireAuth(a.recallCost))

	// 谱系追溯与仪表盘
	mux.HandleFunc("GET "+api+"/trace/raw/{batchNo}", a.requireAuth(a.traceRaw))
	mux.HandleFunc("GET "+api+"/trace/product/{batchNo}", a.requireAuth(a.traceProduct))
	mux.HandleFunc("GET "+api+"/dashboard", a.requireAuth(a.dashboard))

	return a.logging(a.recoverMiddleware(mux))
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	resp := map[string]any{"service": "seafood-trace", "status": "ok"}
	if err := a.db.PingContext(ctx); err != nil {
		writeJSON(w, 503, map[string]any{"status": "degraded", "postgres": err.Error()})
		return
	}
	if err := a.rdb.Ping(ctx).Err(); err != nil {
		writeJSON(w, 503, map[string]any{"status": "degraded", "redis": err.Error()})
		return
	}
	writeJSON(w, 200, resp)
}

type ctxKey string

const userCtxKey ctxKey = "user"

type sessionUser struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	FullName string `json:"full_name"`
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeErr(w, 401, "缺少认证令牌")
			return
		}
		tok := strings.TrimPrefix(h, "Bearer ")
		raw, err := a.rdb.Get(r.Context(), "token:"+tok).Result()
		if err != nil || raw == "" {
			writeErr(w, 401, "认证令牌无效或已过期")
			return
		}
		var u sessionUser
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			writeErr(w, 401, "会话解析失败")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, &u)
		next(w, r.WithContext(ctx))
	}
}

func (a *App) requireRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := r.Context().Value(userCtxKey).(*sessionUser)
		for _, role := range roles {
			if u.Role == role {
				next(w, r)
				return
			}
		}
		writeErr(w, 403, fmt.Sprintf("角色 %s 无权执行该操作", u.Role))
	})
}

func currentUser(r *http.Request) *sessionUser {
	return r.Context().Value(userCtxKey).(*sessionUser)
}
