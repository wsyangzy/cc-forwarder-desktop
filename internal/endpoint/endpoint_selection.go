// endpoint_selection.go - 端点选择/路由功能
// 包含健康端点获取、故障转移端点选择、排序策略等

package endpoint

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// GetHealthyEndpoints returns a list of healthy endpoints from active groups based on strategy.
// v6.0: 以“渠道(channel)”为单位路由，优先只返回当前激活渠道内的端点；
// 跨渠道切换由请求级故障转移触发（见 TriggerRequestFailover）。
func (m *Manager) GetHealthyEndpoints() []*Endpoint {
	// v5.0+: 使用快照机制
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	// 1. 首先尝试获取活跃组（当前激活渠道）的端点
	activeEndpoints := m.groupManager.FilterEndpointsByActiveGroups(snapshot)

	now := time.Now()
	var healthy []*Endpoint
	for _, endpoint := range activeEndpoints {
		// 检查是否参与故障转移（默认为 true），不参与则不作为代理候选
		failoverEnabled := true
		if endpoint.Config.FailoverEnabled != nil {
			failoverEnabled = *endpoint.Config.FailoverEnabled
		}
		if !failoverEnabled {
			continue
		}

		endpoint.mutex.RLock()
		isHealthy := endpoint.Status.Healthy
		// 检查是否在请求冷却中
		inCooldown := !endpoint.Status.CooldownUntil.IsZero() && now.Before(endpoint.Status.CooldownUntil)
		endpoint.mutex.RUnlock()

		if isHealthy && !inCooldown {
			healthy = append(healthy, endpoint)
		} else if inCooldown {
			slog.Debug(fmt.Sprintf("⏭️ [端点选择] 跳过冷却中的端点: %s", endpoint.Config.Name))
		}
	}

	// 2. 如果当前激活渠道有可用端点，直接返回
	if len(healthy) > 0 {
		return m.sortHealthyEndpoints(healthy, true)
	}

	// 当前渠道没有可用端点：由上层触发跨渠道故障转移
	return nil
}

// sortHealthyEndpoints sorts healthy endpoints based on strategy with optional logging
func (m *Manager) sortHealthyEndpoints(healthy []*Endpoint, showLogs bool) []*Endpoint {
	// Sort based on strategy
	switch m.config.Strategy.Type {
	case "priority":
		sort.Slice(healthy, func(i, j int) bool {
			return healthy[i].Config.Priority < healthy[j].Config.Priority
		})
	case "fastest":
		// Log endpoint latencies for fastest strategy (only if showLogs is true)
		if len(healthy) > 1 && showLogs {
			slog.Info("📊 [Fastest Strategy] 基于健康检查的端点延迟排序:")
			for _, ep := range healthy {
				ep.mutex.RLock()
				responseTime := ep.Status.ResponseTime
				ep.mutex.RUnlock()
				slog.Info(fmt.Sprintf("  ⏱️ %s - 延迟: %dms (来源: 定期健康检查)",
					ep.Config.Name, responseTime.Milliseconds()))
			}
		}

		sort.Slice(healthy, func(i, j int) bool {
			healthy[i].mutex.RLock()
			healthy[j].mutex.RLock()
			defer healthy[i].mutex.RUnlock()
			defer healthy[j].mutex.RUnlock()
			return healthy[i].Status.ResponseTime < healthy[j].Status.ResponseTime
		})
	}

	return healthy
}

// GetFastestEndpointsWithRealTimeTest returns endpoints from active groups sorted by real-time testing.
// v6.0: 以“渠道(channel)”为单位路由，只测试/排序当前激活渠道内端点。
func (m *Manager) GetFastestEndpointsWithRealTimeTest(ctx context.Context) []*Endpoint {
	// v5.0+: 使用快照机制
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	// 1. 首先尝试获取活跃组（当前激活渠道）的端点
	activeEndpoints := m.groupManager.FilterEndpointsByActiveGroups(snapshot)

	var healthy []*Endpoint
	for _, endpoint := range activeEndpoints {
		// 检查是否参与故障转移（默认为 true），不参与则不作为代理候选
		failoverEnabled := true
		if endpoint.Config.FailoverEnabled != nil {
			failoverEnabled = *endpoint.Config.FailoverEnabled
		}
		if !failoverEnabled {
			continue
		}

		endpoint.mutex.RLock()
		if endpoint.Status.Healthy {
			healthy = append(healthy, endpoint)
		}
		endpoint.mutex.RUnlock()
	}

	if len(healthy) == 0 {
		return healthy
	}

	// If not using fastest strategy or fast test disabled, apply sorting with logging
	if m.config.Strategy.Type != "fastest" || !m.config.Strategy.FastTestEnabled {
		return m.sortHealthyEndpoints(healthy, true) // Show logs
	}

	// Check if we have cached fast test results first
	testResults, usedCache := m.fastTester.TestEndpointsParallel(ctx, healthy)

	// Only show health check sorting if we're NOT using cache
	if !usedCache && m.config.Strategy.Type == "fastest" && len(healthy) > 1 {
		slog.InfoContext(ctx, "📊 [Fastest Strategy] 基于健康检查的活跃组端点延迟排序:")
		for _, ep := range healthy {
			ep.mutex.RLock()
			responseTime := ep.Status.ResponseTime
			group := ep.Config.Group
			ep.mutex.RUnlock()
			slog.InfoContext(ctx, fmt.Sprintf("  ⏱️ %s (组: %s) - 延迟: %dms (来源: 定期健康检查)",
				ep.Config.Name, group, responseTime.Milliseconds()))
		}
	}

	// Log ALL test results first (including failures) - but only if cache wasn't used
	if len(testResults) > 0 && !usedCache {
		slog.InfoContext(ctx, "🔍 [Fastest Response Mode] 活跃组端点性能测试结果:")
		successCount := 0
		for _, result := range testResults {
			group := result.Endpoint.Config.Group
			if result.Success {
				successCount++
				slog.InfoContext(ctx, fmt.Sprintf("  ✅ 健康 %s (组: %s) - 响应时间: %dms",
					result.Endpoint.Config.Name, group,
					result.ResponseTime.Milliseconds()))
			} else {
				errorMsg := ""
				if result.Error != nil {
					errorMsg = fmt.Sprintf(" - 错误: %s", result.Error.Error())
				}
				slog.InfoContext(ctx, fmt.Sprintf("  ❌ 异常 %s (组: %s) - 响应时间: %dms%s",
					result.Endpoint.Config.Name, group,
					result.ResponseTime.Milliseconds(),
					errorMsg))
			}
		}

		slog.InfoContext(ctx, fmt.Sprintf("📊 [测试摘要] 活跃组测试: %d个端点, 健康: %d个, 异常: %d个",
			len(testResults), successCount, len(testResults)-successCount))
	}

	// Sort by response time (only successful results)
	sortedResults := SortByResponseTime(testResults)

	if len(sortedResults) == 0 {
		slog.WarnContext(ctx, "⚠️ [Fastest Response Mode] 活跃组所有端点测试失败，回退到健康检查模式")
		return healthy // Fall back to health check results if no fast tests succeeded
	}

	// Convert back to endpoint slice
	endpoints := make([]*Endpoint, 0, len(sortedResults))
	for _, result := range sortedResults {
		endpoints = append(endpoints, result.Endpoint)
	}

	// Log the successful endpoint ranking
	if len(endpoints) > 0 {
		// Show the fastest endpoint selection
		fastestEndpoint := endpoints[0]
		var fastestTime int64
		for _, result := range sortedResults {
			if result.Endpoint == fastestEndpoint {
				fastestTime = result.ResponseTime.Milliseconds()
				break
			}
		}

		cacheIndicator := ""
		if usedCache {
			cacheIndicator = " (缓存)"
		}

		slog.InfoContext(ctx, fmt.Sprintf("🚀 [Fastest Response Mode] 选择最快端点: %s - %dms%s",
			fastestEndpoint.Config.Name, fastestTime, cacheIndicator))

		// Show other available endpoints if there are more than one
		if len(endpoints) > 1 && !usedCache {
			slog.InfoContext(ctx, "📋 [备用端点] 其他可用端点:")
			for i := 1; i < len(endpoints); i++ {
				ep := endpoints[i]
				var responseTime int64
				var epGroup string
				for _, result := range sortedResults {
					if result.Endpoint == ep {
						responseTime = result.ResponseTime.Milliseconds()
						epGroup = result.Endpoint.Config.Group
						break
					}
				}
				slog.InfoContext(ctx, fmt.Sprintf("  🔄 备用 %s (组: %s) - 响应时间: %dms",
					ep.Config.Name, epGroup, responseTime))
			}
		}
	}

	return endpoints
}

// GetEndpointByName returns an endpoint by key, only from active groups.
// 兼容：YAML 模式下 key == name。
func (m *Manager) GetEndpointByName(endpointKey string) *Endpoint {
	// v5.0+: 使用快照机制
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	// First filter by active groups
	activeEndpoints := m.groupManager.FilterEndpointsByActiveGroups(snapshot)

	// Then find by name
	for _, endpoint := range activeEndpoints {
		if endpointKeyFromConfig(endpoint.Config) == endpointKey {
			return endpoint
		}
	}
	if endpointKey != "" && !strings.Contains(endpointKey, endpointKeySeparator) {
		var found *Endpoint
		for _, endpoint := range activeEndpoints {
			if endpoint.Config.Name != endpointKey {
				continue
			}
			if found != nil {
				return nil
			}
			found = endpoint
		}
		return found
	}
	return nil
}

// GetEndpointByNameAny returns an endpoint by key from all endpoints (ignoring group status)
// 兼容：YAML 模式下 key == name。
func (m *Manager) GetEndpointByNameAny(endpointKey string) *Endpoint {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	// 优先按 endpointKey（channel::name）查找
	for _, endpoint := range m.endpoints {
		if endpointKeyFromConfig(endpoint.Config) == endpointKey {
			return endpoint
		}
	}

	// 兼容：旧调用方仅传 name（当且仅当全局唯一时允许回退）
	if endpointKey != "" && !strings.Contains(endpointKey, endpointKeySeparator) {
		var found *Endpoint
		for _, endpoint := range m.endpoints {
			if endpoint.Config.Name != endpointKey {
				continue
			}
			if found != nil {
				// 多渠道同名：回退会产生歧义，直接返回 nil
				return nil
			}
			found = endpoint
		}
		return found
	}
	return nil
}

// GetAllEndpoints returns all endpoints (deprecated: use GetEndpoints instead)
func (m *Manager) GetAllEndpoints() []*Endpoint {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	result := make([]*Endpoint, len(m.endpoints))
	copy(result, m.endpoints)
	return result
}

// GetEndpoints returns all endpoints for Web interface
func (m *Manager) GetEndpoints() []*Endpoint {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	result := make([]*Endpoint, len(m.endpoints))
	copy(result, m.endpoints)
	return result
}

// GetEndpointStatus returns the status of an endpoint by name
func (m *Manager) GetEndpointStatus(endpointKey string) EndpointStatus {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	for _, ep := range m.endpoints {
		if endpointKeyFromConfig(ep.Config) == endpointKey {
			ep.mutex.RLock()
			status := ep.Status
			ep.mutex.RUnlock()
			return status
		}
	}
	if endpointKey != "" && !strings.Contains(endpointKey, endpointKeySeparator) {
		var found *Endpoint
		for _, ep := range m.endpoints {
			if ep.Config.Name != endpointKey {
				continue
			}
			if found != nil {
				return EndpointStatus{}
			}
			found = ep
		}
		if found != nil {
			found.mutex.RLock()
			status := found.Status
			found.mutex.RUnlock()
			return status
		}
	}
	return EndpointStatus{}
}

// GetEndpointCount 返回当前端点数量（v5.0+ 新增）
func (m *Manager) GetEndpointCount() int {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()
	return len(m.endpoints)
}
