package endpoint

import (
	"testing"
	"time"

	"cc-forwarder/config"
)

func TestTriggerRequestFailoverWithFailedEndpoints_SetsCooldownForAllFailedEndpoints(t *testing.T) {
	cfg := &config.Config{
		Strategy: config.StrategyConfig{
			Type: "priority",
		},
		Failover: config.FailoverConfig{
			Enabled:         true,
			DefaultCooldown: 5 * time.Second,
		},
		EndpointsStorage: config.EndpointsStorageConfig{
			Type: "yaml",
		},
		Endpoints: []config.EndpointConfig{
			{
				Name:     "p1",
				URL:      "http://example.com/1",
				Channel:  "primary",
				Priority: 1,
				Timeout:  30 * time.Second,
			},
			{
				Name:     "p2",
				URL:      "http://example.com/2",
				Channel:  "primary",
				Priority: 2,
				Timeout:  30 * time.Second,
			},
			{
				Name:     "s1",
				URL:      "http://example.com/3",
				Channel:  "secondary",
				Priority: 10,
				Timeout:  30 * time.Second,
			},
		},
	}

	mgr := NewManager(cfg)

	failed := []string{"p1", "p2"}
	newChannel, err := mgr.TriggerRequestFailoverWithFailedEndpoints(failed, "all_retries_exhausted")
	if err != nil {
		t.Fatalf("TriggerRequestFailoverWithFailedEndpoints error: %v", err)
	}
	if newChannel != "secondary" {
		t.Fatalf("newChannel mismatch: got %q, want %q", newChannel, "secondary")
	}

	now := time.Now()
	for _, name := range failed {
		inCooldown, until, reason := mgr.GetEndpointCooldownInfo(name)
		if !inCooldown {
			t.Fatalf("endpoint %s should be in cooldown", name)
		}
		if !until.After(now) {
			t.Fatalf("endpoint %s cooldown until should be in the future, got %s", name, until.Format(time.RFC3339Nano))
		}
		if reason != "all_retries_exhausted" {
			t.Fatalf("endpoint %s cooldown reason mismatch: got %q, want %q", name, reason, "all_retries_exhausted")
		}
	}

	if !mgr.GetGroupManager().IsGroupInCooldown("primary") {
		t.Fatalf("group primary should be in cooldown after request failover")
	}
}
