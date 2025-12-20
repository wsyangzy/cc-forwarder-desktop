package endpoint

import (
	"testing"
	"time"

	"cc-forwarder/config"
)

func TestGetHealthyEndpoints_PriorityScopedToActiveChannel(t *testing.T) {
	cfg := &config.Config{
		Strategy: config.StrategyConfig{Type: "priority"},
		Health:   config.HealthConfig{Timeout: 5 * time.Second},
		Endpoints: []config.EndpointConfig{
			{Name: "a1", URL: "http://example.invalid", Channel: "A", Priority: 10, Timeout: 1 * time.Second},
			{Name: "a2", URL: "http://example.invalid", Channel: "A", Priority: 1, Timeout: 1 * time.Second},
			{Name: "b1", URL: "http://example.invalid", Channel: "B", Priority: 0, Timeout: 1 * time.Second},
		},
	}

	m := NewManager(cfg)
	for _, ep := range m.GetAllEndpoints() {
		ep.mutex.Lock()
		ep.Status.Healthy = true
		ep.Status.NeverChecked = false
		ep.mutex.Unlock()
	}

	if err := m.ManualActivateGroup("A"); err != nil {
		t.Fatalf("ManualActivateGroup(A) error: %v", err)
	}

	healthy := m.GetHealthyEndpoints()
	if len(healthy) != 2 {
		t.Fatalf("expected 2 healthy endpoints in active channel A, got %d", len(healthy))
	}
	if healthy[0].Config.Name != "a2" || healthy[1].Config.Name != "a1" {
		t.Fatalf("unexpected order: got [%s, %s], want [a2, a1]",
			healthy[0].Config.Name, healthy[1].Config.Name)
	}
}

func TestGetHealthyEndpoints_FailoverEnabledScopedToActiveChannel(t *testing.T) {
	failoverFalse := false
	failoverTrue := true

	cfg := &config.Config{
		Strategy: config.StrategyConfig{Type: "priority"},
		Health:   config.HealthConfig{Timeout: 5 * time.Second},
		Endpoints: []config.EndpointConfig{
			{Name: "a1", URL: "http://example.invalid", Channel: "A", Priority: 1, FailoverEnabled: &failoverFalse, Timeout: 1 * time.Second},
			{Name: "a2", URL: "http://example.invalid", Channel: "A", Priority: 2, FailoverEnabled: &failoverTrue, Timeout: 1 * time.Second},
			{Name: "b1", URL: "http://example.invalid", Channel: "B", Priority: 0, FailoverEnabled: &failoverTrue, Timeout: 1 * time.Second},
		},
	}

	m := NewManager(cfg)
	for _, ep := range m.GetAllEndpoints() {
		ep.mutex.Lock()
		ep.Status.Healthy = true
		ep.Status.NeverChecked = false
		ep.mutex.Unlock()
	}

	if err := m.ManualActivateGroup("A"); err != nil {
		t.Fatalf("ManualActivateGroup(A) error: %v", err)
	}

	healthy := m.GetHealthyEndpoints()
	if len(healthy) != 1 {
		t.Fatalf("expected 1 healthy endpoint in active channel A (a2 only), got %d", len(healthy))
	}
	if healthy[0].Config.Name != "a2" {
		t.Fatalf("unexpected endpoint: got %s, want a2", healthy[0].Config.Name)
	}

	// 组级暂停：只有组内全部 failover_enabled=false 才应暂停
	var groupA, groupB *GroupInfo
	for _, g := range m.groupManager.GetAllGroups() {
		if g.Name == "A" {
			groupA = g
		}
		if g.Name == "B" {
			groupB = g
		}
	}
	if groupA == nil || groupB == nil {
		t.Fatalf("expected groups A and B to exist")
	}
	if groupA.ManuallyPaused {
		t.Fatalf("group A should not be ManuallyPaused when at least one endpoint participates in failover")
	}
	if groupB.ManuallyPaused {
		t.Fatalf("group B should not be ManuallyPaused when its endpoint participates in failover")
	}
}

