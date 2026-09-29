package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

// pqInt64Array 把 id 切片包装为 PostgreSQL 数组参数
func pqInt64Array(ids []int64) any { return pq.Array(ids) }

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func apiErr(code int, msg string) error { return &apiError{code: code, msg: msg} }

func writeApiErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		errJSON(w, ae.code, ae.msg)
		return
	}
	errJSON(w, 500, err.Error())
}

// woTxFn 在持锁事务内执行业务，返回 HTTP 状态码与响应载荷（提交成功后才写出）
type woTxFn func(tx *sql.Tx) (int, any, error)

// lockedByOrderNo 按工单号定位工单并在其分布式锁 + 行锁(SELECT FOR UPDATE)内执行事务
func (s *Server) lockedByOrderNo(ctx context.Context, orderNo string, fn woTxFn) (int, any, error) {
	var woID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM work_orders WHERE order_no=$1`, orderNo).
		Scan(&woID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, apiErr(404, "工单不存在: "+orderNo)
		}
		return 0, nil, err
	}
	return s.runWOLocked(ctx, woID, fn)
}

// lockedByThaw 按解冻记录定位工单加锁
func (s *Server) lockedByThaw(ctx context.Context, thawID int64, fn woTxFn) (int, any, error) {
	var woID int64
	if err := s.db.QueryRowContext(ctx, `SELECT work_order_id FROM thaw_records WHERE id=$1`, thawID).
		Scan(&woID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, apiErr(404, "解冻记录不存在")
		}
		return 0, nil, err
	}
	return s.runWOLocked(ctx, woID, fn)
}

func (s *Server) runWOLocked(ctx context.Context, woID int64, fn woTxFn) (int, any, error) {
	doTx := func() (int, any, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, nil, err
		}
		defer tx.Rollback()
		var status string
		if err := tx.QueryRow(`SELECT status FROM work_orders WHERE id=$1 FOR UPDATE`, woID).
			Scan(&status); err != nil {
			return 0, nil, err
		}
		code, payload, err := fn(tx)
		if err != nil {
			return 0, nil, err
		}
		if err := tx.Commit(); err != nil {
			return 0, nil, err
		}
		return code, payload, nil
	}
	if s.cache != nil {
		var code int
		var payload any
		err := s.cache.withLock(ctx, woID, func() error {
			var e error
			code, payload, e = doTx()
			return e
		})
		return code, payload, err
	}
	return doTx()
}

func itoa(i int) string { return strconv.Itoa(i) }

func cPrefix(note string) string {
	note = strings.TrimSpace(note)
	if note != "" {
		return "：" + note
	}
	return ""
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func toInt64s(v any) []int64 {
	rs, ok := v.([]int64)
	if !ok {
		return nil
	}
	return rs
}
