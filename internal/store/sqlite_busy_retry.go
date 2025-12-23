package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	// 统一用字符串判断，避免引入 driver 具体错误类型耦合。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "database is locked")
}

func queryRowsWithSQLiteBusyRetry(ctx context.Context, queryFn func() (*sql.Rows, error)) (*sql.Rows, error) {
	if ctx == nil {
		return queryFn()
	}

	backoff := 30 * time.Millisecond
	for {
		rows, err := queryFn()
		if err == nil || !isSQLiteBusyError(err) {
			return rows, err
		}

		// 若上层已取消/超时，直接返回最后一次的 busy 错误，便于诊断。
		if ctx.Err() != nil {
			return nil, err
		}

		wait := backoff
		if wait > 500*time.Millisecond {
			wait = 500 * time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}

		backoff *= 2
	}
}

