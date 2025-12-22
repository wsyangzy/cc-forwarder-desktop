package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cc-forwarder/config"
	"cc-forwarder/internal/endpoint"
)

type authCapturingServer struct {
	server  *httptest.Server
	mu      sync.Mutex
	auths   []string
	request int
	failN   int
}

func newAuthCapturingServer(failN int) *authCapturingServer {
	s := &authCapturingServer{failN: failN}

	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.request++
		current := s.request
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()

		if current <= s.failN {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"mock server error"}`))
			return
		}

		// 成功响应：模拟流式 SSE（足够让 proxy handler 走通）
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\n")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"model":"claude-3-5-haiku"}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_delta\n")
		fmt.Fprint(w, `data: {"type":"message_delta","usage":{"input_tokens":1,"output_tokens":1}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, "data: {}\n\n")
	}))

	return s
}

func (s *authCapturingServer) Close() {
	if s.server != nil {
		s.server.Close()
	}
}

func (s *authCapturingServer) Auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.auths))
	copy(out, s.auths)
	return out
}

func (s *authCapturingServer) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.request
}

func boolPtr(v bool) *bool { return &v }

func TestChannelFailover_SwitchesAuthorizationToken(t *testing.T) {
	// 同一渠道(A)下两个端点，分别配置不同 token。
	// 端点1：总是失败；端点2：成功。
	// 期望：请求会从端点1故障转移到端点2，且 Authorization 头会随端点切换。
	s1 := newAuthCapturingServer(999) // 永远失败
	defer s1.Close()
	s2 := newAuthCapturingServer(0) // 立即成功
	defer s2.Close()

	cfg := &config.Config{
		Strategy: config.StrategyConfig{
			Type: "priority",
		},
		Retry: config.RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   10 * time.Millisecond,
			MaxDelay:    10 * time.Millisecond,
			Multiplier:  1.0,
		},
		Failover: config.FailoverConfig{
			Enabled: false, // 仅验证“渠道内端点切换”，不触发跨渠道逻辑
		},
		Endpoints: []config.EndpointConfig{
			{
				Name:            "ep-1",
				URL:             s1.server.URL,
				Channel:         "A",
				Priority:        1,
				Timeout:         3 * time.Second,
				Token:           "token-A",
				FailoverEnabled: boolPtr(true),
			},
			{
				Name:            "ep-2",
				URL:             s2.server.URL,
				Channel:         "A",
				Priority:        2,
				Timeout:         3 * time.Second,
				Token:           "token-B",
				FailoverEnabled: boolPtr(true),
			},
		},
	}

	endpointManager := endpoint.NewManager(cfg)
	handler := NewHandler(endpointManager, cfg)

	// 标记端点健康，并手动激活渠道 A（避免与自动激活/健康检查时序耦合）
	for _, ep := range endpointManager.GetAllEndpoints() {
		ep.Status.Healthy = true
		ep.Status.NeverChecked = false
		ep.Status.LastCheck = time.Now()
	}
	if err := endpointManager.ManualActivateGroup("A"); err != nil {
		t.Fatalf("ManualActivateGroup(A) error: %v", err)
	}

	body := `{"message":"test streaming request"}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer client-token") // 应被端点 token 覆盖

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handler.ServeHTTP(rec, req.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d, body=%s", rec.Code, rec.Body.String())
	}

	if s1.RequestCount() == 0 || s2.RequestCount() == 0 {
		t.Fatalf("expected requests to hit both endpoints, got s1=%d s2=%d", s1.RequestCount(), s2.RequestCount())
	}

	for _, got := range s1.Auths() {
		if got != "Bearer token-A" {
			t.Fatalf("endpoint-1 Authorization mismatch: want %q, got %q", "Bearer token-A", got)
		}
	}
	for _, got := range s2.Auths() {
		if got != "Bearer token-B" {
			t.Fatalf("endpoint-2 Authorization mismatch: want %q, got %q", "Bearer token-B", got)
		}
	}
}

func TestChannelFailover_FastestStrategy_SwitchesAuthorizationToken(t *testing.T) {
	// 同一渠道(A)下两个端点，分别配置不同 token。
	// fastest 策略下应先选择“更快”的端点；当其失败后，应故障转移到另一个端点，
	// 且 Authorization 头随端点切换而切换。
	s1 := newAuthCapturingServer(999) // 永远失败
	defer s1.Close()
	s2 := newAuthCapturingServer(0) // 立即成功
	defer s2.Close()

	cfg := &config.Config{
		Strategy: config.StrategyConfig{
			Type:            "fastest",
			FastTestEnabled: false, // 固化“基于健康检查 response_time”的排序路径
		},
		Retry: config.RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   10 * time.Millisecond,
			MaxDelay:    10 * time.Millisecond,
			Multiplier:  1.0,
		},
		Failover: config.FailoverConfig{
			Enabled: false, // 仅验证“渠道内端点切换”，不触发跨渠道逻辑
		},
		Endpoints: []config.EndpointConfig{
			{
				Name:            "ep-fast",
				URL:             s1.server.URL,
				Channel:         "A",
				Priority:        1,
				Timeout:         3 * time.Second,
				Token:           "token-fast",
				FailoverEnabled: boolPtr(true),
			},
			{
				Name:            "ep-slow",
				URL:             s2.server.URL,
				Channel:         "A",
				Priority:        2,
				Timeout:         3 * time.Second,
				Token:           "token-slow",
				FailoverEnabled: boolPtr(true),
			},
		},
	}

	endpointManager := endpoint.NewManager(cfg)
	handler := NewHandler(endpointManager, cfg)

	// 标记端点健康，并设置健康检查的响应时间（fastest 策略依赖此字段排序）。
	// 注意：这里“快”与“慢”只影响排序，不代表 token 的可用性判断。
	for _, ep := range endpointManager.GetAllEndpoints() {
		ep.Status.Healthy = true
		ep.Status.NeverChecked = false
		ep.Status.LastCheck = time.Now()
		switch ep.Config.Name {
		case "ep-fast":
			ep.Status.ResponseTime = 10 * time.Millisecond
		case "ep-slow":
			ep.Status.ResponseTime = 30 * time.Millisecond
		}
	}
	if err := endpointManager.ManualActivateGroup("A"); err != nil {
		t.Fatalf("ManualActivateGroup(A) error: %v", err)
	}

	body := `{"message":"test streaming request"}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handler.ServeHTTP(rec, req.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d, body=%s", rec.Code, rec.Body.String())
	}

	if s1.RequestCount() == 0 || s2.RequestCount() == 0 {
		t.Fatalf("expected requests to hit both endpoints, got s1=%d s2=%d", s1.RequestCount(), s2.RequestCount())
	}

	for _, got := range s1.Auths() {
		if got != "Bearer token-fast" {
			t.Fatalf("ep-fast Authorization mismatch: want %q, got %q", "Bearer token-fast", got)
		}
	}
	for _, got := range s2.Auths() {
		if got != "Bearer token-slow" {
			t.Fatalf("ep-slow Authorization mismatch: want %q, got %q", "Bearer token-slow", got)
		}
	}
}
