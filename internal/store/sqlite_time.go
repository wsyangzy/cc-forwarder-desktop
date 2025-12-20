package store

import (
	"strings"
	"time"
)

func parseSQLiteDateTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}

	layouts := []string{
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05.999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}

	// 兜底：部分 SQLite 写法会存成 "YYYY-MM-DD HH:MM:SS.sss+08:00" 但 Go 解析需要 'T' 分隔
	// 尝试将空格替换为 'T' 再用 RFC3339Nano 解析（仅在末尾确实带时区时尝试，避免每次都走兜底）。
	if strings.Contains(value, " ") {
		tail := ""
		if len(value) > 19 {
			tail = value[19:]
		}
		if strings.Contains(tail, "+") || strings.Contains(tail, "-") || strings.Contains(tail, "Z") {
			if t, err := time.Parse(time.RFC3339Nano, strings.Replace(value, " ", "T", 1)); err == nil {
				return t
			}
		}
	}

	return time.Time{}
}
