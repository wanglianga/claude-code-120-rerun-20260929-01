package main

import (
	"database/sql"
	"strconv"
	"time"
)

func strconvFormat(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// parseTime 兼容 RFC3339 与 "2006-01-02 15:04:05"，空串取当前时间
func parseTime(s string) time.Time {
	if t, ok := parseTimePtr(s); ok {
		return t
	}
	return time.Now()
}

func parseTimePtr(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// createEventTx 在事务内写入品控事件并返回事件号
func createEventTx(tx *sql.Tx, eventNo, evType, severity, source string,
	rawID, fgID, woID, channelID sql.NullInt64, desc, by string) error {
	_, err := tx.Exec(`INSERT INTO quality_events
		(event_no,type,severity,source,raw_batch_id,finished_batch_id,work_order_id,channel_id,
		 description,status,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',$10)`,
		eventNo, evType, severity, source, rawID, fgID, woID, channelID, desc, by)
	return err
}
