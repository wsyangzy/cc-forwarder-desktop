package endpoint

import (
	"testing"
	"time"

	"cc-forwarder/config"
)

func TestTriggerRequestFailoverWithFailedEndpoints_SelectsFastestChannel(t *testing.T) {
	cfg := &config.Config{
		Strategy: config.StrategyConfig{
			Type: "fastest",
		},
		Failover: config.FailoverConfig{
			Enabled:         true,
			DefaultCooldown: 5 * time.Second,
		},
		EndpointsStorage: config.EndpointsStorageConfig{
			Type: "yaml",
		},
		Endpoints: []config.EndpointConfig{
			{Name: "a1", URL: "http://example.invalid/a1", Channel: "A", Priority: 1, Timeout: 2 * time.Second},
			{Name: "a2", URL: "http://example.invalid/a2", Channel: "A", Priority: 2, Timeout: 2 * time.Second},
			{Name: "b1", URL: "http://example.invalid/b1", Channel: "B", Priority: 1, Timeout: 2 * time.Second},
			{Name: "c1", URL: "http://example.invalid/c1", Channel: "C", Priority: 1, Timeout: 2 * time.Second},
		},
	}

	mgr := NewManager(cfg)

	// 设定健康状态与响应时间（模拟健康检查历史）
	for _, ep := range mgr.GetAllEndpoints() {
		ep.mutex.Lock()
		ep.Status.Healthy = true
		ep.Status.NeverChecked = false
		switch ep.Config.Channel {
		case "B":
			ep.Status.ResponseTime = 200 * time.Millisecond
		case "C":
			ep.Status.ResponseTime = 50 * time.Millisecond
		default:
			ep.Status.ResponseTime = 150 * time.Millisecond
		}
		ep.mutex.Unlock()
	}

	newChannel, err := mgr.TriggerRequestFailoverWithFailedEndpoints([]string{"a1", "a2"}, "all_retries_exhausted")
	if err != nil {
		t.Fatalf("TriggerRequestFailoverWithFailedEndpoints error: %v", err)
	}
	if newChannel != "C" {
		t.Fatalf("newChannel mismatch: got %q, want %q", newChannel, "C")
	}
}

func TestTriggerRequestFailoverWithFailedEndpoints_FastestSkipsPausedOrCooldown(t *testing.T) {
	failoverFalse := false

	cfg := &config.Config{
		Strategy: config.StrategyConfig{
			Type: "fastest",
		},
		Failover: config.FailoverConfig{
			Enabled:         true,
			DefaultCooldown: 5 * time.Second,
		},
		EndpointsStorage: config.EndpointsStorageConfig{
			Type: "yaml",
		},
		Endpoints: []config.EndpointConfig{
			{Name: "a1", URL: "http://example.invalid/a1", Channel: "A", Priority: 1, Timeout: 2 * time.Second},
			{Name: "b1", URL: "http://example.invalid/b1", Channel: "B", Priority: 1, Timeout: 2 * time.Second},
			// C 渠道端点不参与故障转移 => 组级暂停（ManuallyPaused=true）
			{Name: "c1", URL: "http://example.invalid/c1", Channel: "C", Priority: 1, FailoverEnabled: &failoverFalse, Timeout: 2 * time.Second},
		},
	}

	mgr := NewManager(cfg)

	for _, ep := range mgr.GetAllEndpoints() {
		ep.mutex.Lock()
		ep.Status.Healthy = true
		ep.Status.NeverChecked = false
		switch ep.Config.Channel {
		case "B":
			ep.Status.ResponseTime = 200 * time.Millisecond
		case "C":
			ep.Status.ResponseTime = 10 * time.Millisecond
		}
		ep.mutex.Unlock()
	}

	// 确认 C 组已被标记为暂停（因为全部 failover_enabled=false）
	var groupC *GroupInfo
	for _, g := range mgr.GetGroupManager().GetAllGroups() {
		if g.Name == "C" {
			groupC = g
			break
		}
	}
	if groupC == nil {
		t.Fatalf("expected group C to exist")
	}
	if !groupC.ManuallyPaused {
		t.Fatalf("expected group C to be ManuallyPaused when all endpoints are excluded from failover")
	}

	newChannel, err := mgr.TriggerRequestFailoverWithFailedEndpoints([]string{"a1"}, "all_retries_exhausted")
	if err != nil {
		t.Fatalf("TriggerRequestFailoverWithFailedEndpoints error: %v", err)
	}
	if newChannel != "B" {
		t.Fatalf("newChannel mismatch: got %q, want %q", newChannel, "B")
	}
}
