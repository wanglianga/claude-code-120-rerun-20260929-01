package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	db    *sql.DB
	rdb   *redis.Client
	cache *Cache
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dsn := getenv("DATABASE_URL",
		"postgres://seafood@/seafood?host=/tmp&port=5433&sslmode=disable")
	addr := getenv("HTTP_ADDR", ":8080")
	redisAddr := getenv("REDIS_ADDR", "localhost:6379")

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(20)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := waitDB(db); err != nil {
		log.Fatalf("db not ready: %v", err)
	}

	migDir := getenv("MIGRATIONS_DIR", "migrations")
	if err := runMigrations(db, migDir); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	var cache *Cache
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Printf("WARN redis unavailable (%v); continuing with degraded cache/lock", err)
		rdb = nil
	} else {
		cache = newCache(rdb)
	}

	s := &Server{db: db, rdb: rdb, cache: cache}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	log.Printf("seafood factory service listening on %s", addr)
	log.Printf("Swagger UI: http://localhost%s/swagger/", addr)
	if err := http.ListenAndServe(addr, s.recoverWrap(s.reqLog(mux))); err != nil {
		log.Fatal(err)
	}
}

func waitDB(db *sql.DB) error {
	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return db.Ping()
}

func runMigrations(db *sql.DB, dir string) error {
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	for _, f := range files {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename=$1)`,
			filepath.Base(f)).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		log.Printf("applying migration %s", filepath.Base(f))
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(filename) VALUES ($1)`,
			filepath.Base(f)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---------- HTTP helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

type ctxKey string

const userKey ctxKey = "user"

type authUser struct {
	ID       int64
	Username string
	Role     string
}

func (s *Server) auth(roles ...string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			if tok == "" {
				errJSON(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			u := authUser{}
			err := s.db.QueryRow(`SELECT id, username, role FROM users WHERE api_token=$1`, tok).
				Scan(&u.ID, &u.Username, &u.Role)
			if err != nil {
				errJSON(w, http.StatusUnauthorized, "invalid token")
				return
			}
			if len(allowed) > 0 && !allowed[u.Role] {
				errJSON(w, http.StatusForbidden, "role not permitted: "+u.Role)
				return
			}
			ctx := context.WithValue(r.Context(), userKey, u)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func currentUser(r *http.Request) authUser {
	return r.Context().Value(userKey).(authUser)
}

// with 组合中间件与处理器
func with(h http.HandlerFunc, mws ...func(http.Handler) http.Handler) http.Handler {
	var hh http.Handler = h
	for i := len(mws) - 1; i >= 0; i-- {
		hh = mws[i](hh)
	}
	return hh
}

func (s *Server) recoverWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s: %v\n%s", r.URL.Path, rec, debug.Stack())
				errJSON(w, http.StatusInternalServerError, fmt.Sprintf("internal error: %v", rec))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) reqLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// idempotent 包装写操作：相同 Idempotency-Key 在 24h 内返回首次结果
func (s *Server) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next(w, r)
			return
		}
		// Redis 快速路径
		if s.rdb != nil {
			lock := "idem:lock:" + key
			ok, _ := s.rdb.SetNX(r.Context(), lock, "1", 10*time.Second).Result()
			if !ok {
				// 等待已有请求落库
				for i := 0; i < 50; i++ {
					if b, err := s.rdb.Get(r.Context(), "idem:resp:"+key).Bytes(); err == nil {
						var cached struct {
							Status int             `json:"status"`
							Body   json.RawMessage `json:"body"`
						}
						if json.Unmarshal(b, &cached) == nil {
							w.Header().Set("Idempotent-Replay", "true")
							writeJSON(w, cached.Status, cached.Body)
							return
						}
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		// PG 兜底：是否已有结果
		var status sql.NullInt64
		var body []byte
		err := s.db.QueryRow(`SELECT status_code, response FROM idempotency_keys WHERE idem_key=$1`, key).
			Scan(&status, &body)
		if err == nil {
			w.Header().Set("Idempotent-Replay", "true")
			var v any
			_ = json.Unmarshal(body, &v)
			writeJSON(w, int(status.Int64), v)
			return
		}
		rw := &captureRW{ResponseWriter: w, status: 200}
		next(rw, r)
		if rw.status >= 200 && rw.status < 300 {
			_, _ = s.db.Exec(
				`INSERT INTO idempotency_keys(idem_key, response, status_code) VALUES ($1,$2,$3)
				 ON CONFLICT (idem_key) DO NOTHING`,
				key, string(rw.buf), rw.status)
			if s.rdb != nil {
				s.rdb.Set(r.Context(), "idem:resp:"+key,
					fmt.Sprintf(`{"status":%d,"body":%s}`, rw.status, string(rw.buf)), 24*time.Hour)
				s.rdb.Del(r.Context(), "idem:lock:"+key)
			}
		}
	}
}

type captureRW struct {
	http.ResponseWriter
	status int
	buf    []byte
}

func (c *captureRW) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureRW) Write(b []byte) (int, error) {
	if c.status >= 200 && c.status < 300 {
		c.buf = append(c.buf, b...)
	}
	return c.ResponseWriter.Write(b)
}
