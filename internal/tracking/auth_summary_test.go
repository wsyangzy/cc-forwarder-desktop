package tracking

import (
	"context"
	"testing"
	"time"
)

func TestUsageSummaryByAuth_Basic(t *testing.T) {
	cfg := &Config{
		Enabled:         true,
		DatabasePath:    ":memory:",
		BufferSize:      100,
		BatchSize:       10,
		FlushInterval:   50 * time.Millisecond,
		MaxRetry:        3,
		CleanupInterval: 24 * time.Hour,
		RetentionDays:   30,
		ModelPricing: map[string]ModelPricing{
			"claude-3-5-haiku-20241022": {
				Input:         1.00,
				Output:        5.00,
				CacheCreation: 1.25,
				CacheRead:     0.10,
			},
		},
	}

	tracker, err := NewUsageTracker(cfg)
	if err != nil {
		t.Fatalf("Failed to create usage tracker: %v", err)
	}
	defer tracker.Close()

	type reqCase struct {
		requestID string
		authKey   string
	}
	cases := []reqCase{
		{requestID: "req-auth-001", authKey: "token1@aaaaaaaaaaaaaaaa"},
		{requestID: "req-auth-002", authKey: "token2@bbbbbbbbbbbbbbbb"},
	}

	for _, c := range cases {
		tracker.RecordRequestStart(c.requestID, "127.0.0.1", "test-agent", "POST", "/v1/messages", false)
		tracker.RecordRequestUpdate(c.requestID, UpdateOptions{
			Channel:      stringPtr("channel-a"),
			EndpointName: stringPtr("endpoint-1"),
			GroupName:    stringPtr("group-a"),
			AuthType:     stringPtr("token"),
			AuthKey:      stringPtr(c.authKey),
			Status:       stringPtr("forwarding"),
			RetryCount:   intPtr(0),
			HttpStatus:   intPtr(200),
		})

		tracker.RecordRequestSuccess(c.requestID, "claude-3-5-haiku-20241022", &TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
		}, 150*time.Millisecond)
	}

	time.Sleep(200 * time.Millisecond) // 等待热池归档写入完成

	// 触发汇总更新（包含 usage_summary_by_auth）
	tracker.updateUsageSummary()
	time.Sleep(150 * time.Millisecond)

	db := tracker.GetReadDB()
	if db == nil {
		t.Fatalf("read database not initialized")
	}

	ctx := context.Background()

	// 1) request_logs 应该写入 auth_type/auth_key
	for _, c := range cases {
		var authType, authKey string
		if err := db.QueryRowContext(ctx, "SELECT COALESCE(auth_type, ''), COALESCE(auth_key, '') FROM request_logs WHERE request_id = ?", c.requestID).Scan(&authType, &authKey); err != nil {
			t.Fatalf("failed to query request_logs auth fields: %v", err)
		}
		if authType != "token" {
			t.Fatalf("unexpected authType for %s: %q", c.requestID, authType)
		}
		if authKey != c.authKey {
			t.Fatalf("unexpected authKey for %s: want %q, got %q", c.requestID, c.authKey, authKey)
		}
	}

	// 2) usage_summary_by_auth 应该按 auth_key 维度分开统计
	rows, err := db.QueryContext(ctx, "SELECT auth_key, request_count, total_cost_usd FROM usage_summary_by_auth")
	if err != nil {
		t.Fatalf("failed to query usage_summary_by_auth: %v", err)
	}
	defer rows.Close()

	got := map[string]struct {
		requestCount int
		totalCost    float64
	}{}
	for rows.Next() {
		var authKey string
		var requestCount int
		var totalCost float64
		if err := rows.Scan(&authKey, &requestCount, &totalCost); err != nil {
			t.Fatalf("failed to scan usage_summary_by_auth row: %v", err)
		}
		got[authKey] = struct {
			requestCount int
			totalCost    float64
		}{requestCount: requestCount, totalCost: totalCost}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("error iterating usage_summary_by_auth rows: %v", err)
	}

	if len(got) != len(cases) {
		t.Fatalf("unexpected usage_summary_by_auth rows: want %d, got %d", len(cases), len(got))
	}
	for _, c := range cases {
		entry, ok := got[c.authKey]
		if !ok {
			t.Fatalf("missing summary row for auth_key=%q", c.authKey)
		}
		if entry.requestCount != 1 {
			t.Fatalf("unexpected request_count for auth_key=%q: %d", c.authKey, entry.requestCount)
		}
		if entry.totalCost <= 0 {
			t.Fatalf("unexpected total_cost_usd for auth_key=%q: %f", c.authKey, entry.totalCost)
		}
	}
}

func TestChannelTotals_IgnoreAuth(t *testing.T) {
	cfg := &Config{
		Enabled:         true,
		DatabasePath:    ":memory:",
		BufferSize:      100,
		BatchSize:       10,
		FlushInterval:   50 * time.Millisecond,
		MaxRetry:        3,
		CleanupInterval: 24 * time.Hour,
		RetentionDays:   30,
		ModelPricing: map[string]ModelPricing{
			"claude-3-5-haiku-20241022": {
				Input:         1.00,
				Output:        5.00,
				CacheCreation: 1.25,
				CacheRead:     0.10,
			},
		},
	}

	tracker, err := NewUsageTracker(cfg)
	if err != nil {
		t.Fatalf("Failed to create usage tracker: %v", err)
	}
	defer tracker.Close()

	// 同一渠道下两个端点，使用不同 auth_key（代表不同 token）
	// 期望：渠道统计按 channel 聚合，不受 auth_type/auth_key 影响。
	records := []struct {
		requestID string
		endpoint  string
		authKey   string
	}{
		{requestID: "req-ch-001", endpoint: "endpoint-a", authKey: "tokenA@aaaaaaaaaaaaaaaa"},
		{requestID: "req-ch-002", endpoint: "endpoint-b", authKey: "tokenB@bbbbbbbbbbbbbbbb"},
	}

	for _, r := range records {
		tracker.RecordRequestStart(r.requestID, "127.0.0.1", "test-agent", "POST", "/v1/messages", false)
		tracker.RecordRequestUpdate(r.requestID, UpdateOptions{
			Channel:      stringPtr("channel-x"),
			EndpointName: stringPtr(r.endpoint),
			GroupName:    stringPtr("group-x"),
			AuthType:     stringPtr("token"),
			AuthKey:      stringPtr(r.authKey),
			Status:       stringPtr("forwarding"),
			RetryCount:   intPtr(0),
			HttpStatus:   intPtr(200),
		})
		tracker.RecordRequestSuccess(r.requestID, "claude-3-5-haiku-20241022", &TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
		}, 120*time.Millisecond)
	}

	time.Sleep(200 * time.Millisecond)

	ctx := context.Background()
	totals, err := tracker.QueryUsageStatsTotals(ctx, &QueryOptions{Channel: "channel-x"})
	if err != nil {
		t.Fatalf("failed to query totals: %v", err)
	}
	if totals.TotalRequests != int64(len(records)) {
		t.Fatalf("unexpected total_requests: want %d, got %d", len(records), totals.TotalRequests)
	}
	if totals.TotalCostUSD <= 0 {
		t.Fatalf("unexpected total_cost_usd: %f", totals.TotalCostUSD)
	}
}
