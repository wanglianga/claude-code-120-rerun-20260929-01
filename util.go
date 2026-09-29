package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func hashPassword(p string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(b), err
}

func checkPassword(hash, p string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(p)) == nil
}

// nextDocNo 按前缀+日期在 PG 取号，Redis 分布式锁防止多产线并发重号。
// 形如 RC-20260929-0007
func (a *App) nextDocNo(ctx context.Context, prefix string) (string, error) {
	day := time.Now().Format("20060102")
	lockKey := "lock:docseq:" + prefix + ":" + day
	tok := uuid.NewString()
	ok, err := a.rdb.SetNX(ctx, lockKey, tok, 5*time.Second).Result()
	if err != nil {
		return "", err
	}
	if !ok {
		// 等待并重试一次
		time.Sleep(50 * time.Millisecond)
		return a.nextDocNoRetry(ctx, prefix, day)
	}
	defer a.releaseLock(ctx, lockKey, tok)
	return a.advanceSeq(ctx, prefix, day)
}

func (a *App) nextDocNoRetry(ctx context.Context, prefix, day string) (string, error) {
	var v int64
	err := a.db.QueryRowContext(ctx,
		`INSERT INTO doc_sequences(prefix,day,value) VALUES($1,$2,1)
		 ON CONFLICT (prefix,day) DO UPDATE SET value=doc_sequences.value+1 RETURNING value`,
		prefix, day).Scan(&v)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%04d", prefix, day, v), nil
}

func (a *App) advanceSeq(ctx context.Context, prefix, day string) (string, error) {
	var v int64
	err := a.db.QueryRowContext(ctx,
		`INSERT INTO doc_sequences(prefix,day,value) VALUES($1,$2,1)
		 ON CONFLICT (prefix,day) DO UPDATE SET value=doc_sequences.value+1 RETURNING value`,
		prefix, day).Scan(&v)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%04d", prefix, day, v), nil
}

func (a *App) releaseLock(ctx context.Context, key, tok string) {
	// 简单 Lua：仅持有者可释放
	const script = `if redis.call("get",KEYS[1])==ARGV[1] then return redis.call("del",KEYS[1]) else return 0 end`
	_ = a.rdb.Eval(ctx, script, []string{key}, tok).Err()
}

// idempotency: 用 Redis 保证写请求幂等（相同 Idempotency-Key 返回缓存结果）
func (a *App) idempotent(ctx context.Context, key string) (cached string, ok bool) {
	if key == "" {
		return "", false
	}
	v, err := a.rdb.Get(ctx, "idem:"+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false
	}
	return v, err == nil
}

func (a *App) markIdempotent(ctx context.Context, key, payload string) {
	if key != "" {
		_ = a.rdb.Set(ctx, "idem:"+key, payload, time.Hour).Err()
	}
}

func parseInt64(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// itoa helper
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
