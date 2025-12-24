// health_check.go - 健康检查相关功能
// 包含定时健康检查、手动检查、批量检查等

package endpoint

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"cc-forwarder/internal/utils"
)

// SetOnHealthCheckComplete 设置健康检查完成回调
// 用于 Wails 桌面应用在定时健康检查完成后推送事件到前端
func (m *Manager) SetOnHealthCheckComplete(fn func()) {
	m.onHealthCheckComplete = fn
}

// refreshGroupActivation 刷新组激活状态
// 当端点健康状态变化时调用，用于重新评估哪些组应该被激活
// v5.0+: 解决新增端点后不会自动激活的问题
func (m *Manager) refreshGroupActivation() {
	// 防御性检查：确保 groupManager 已初始化
	if m.groupManager == nil {
		return
	}

	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	m.groupManager.UpdateGroups(snapshot)
	slog.Debug("🔄 [组管理] 端点健康状态变化，已刷新组激活状态")

	// 触发健康检查完成回调（通知前端更新）
	if m.onHealthCheckComplete != nil {
		go m.onHealthCheckComplete()
	}
}

// healthCheckLoop runs the health check routine
func (m *Manager) healthCheckLoop() {
	defer m.wg.Done()

	// 获取当前检查间隔
	getCheckInterval := func() time.Duration {
		interval := m.config.Health.CheckInterval
		if interval <= 0 {
			interval = 30 * time.Second // 默认30秒检查一次
		}
		return interval
	}

	currentInterval := getCheckInterval()
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	// Initial health check
	m.performHealthChecks()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.performHealthChecks()

			// 检查配置是否变化，动态调整间隔
			newInterval := getCheckInterval()
			if newInterval != currentInterval {
				slog.Info("🔄 [健康检查] 间隔已更新", "old", currentInterval, "new", newInterval)
				currentInterval = newInterval
				ticker.Reset(currentInterval)
			}
		}
	}
}

// performHealthChecks performs health checks on all endpoints
func (m *Manager) performHealthChecks() {
	// v5.0+: 使用快照机制，不阻塞动态增删操作
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	var endpointsToCheck []*Endpoint

	if len(snapshot) == 0 {
		slog.Debug("🩺 [健康检查] 没有配置的端点，跳过健康检查")
		return
	}

	// 健康检查覆盖范围（与 TODO.md 的资源优化要求对齐）：
	// - 启用“渠道间故障转移”时：检查所有“参与故障转移”的渠道，且仅检查其中参与故障转移的端点
	// - 未启用时：只检查当前活跃渠道中参与故障转移的端点
	if m.config.Failover.Enabled {
		for _, ep := range snapshot {
			if ep == nil {
				continue
			}
			// 渠道级开关：暂停渠道不参与跨渠道故障转移，也不做健康检查
			if m.groupManager != nil && !m.groupManager.IsChannelFailoverEnabled(ChannelKey(ep)) {
				continue
			}
			// 端点级开关：不参与故障转移的端点不做健康检查
			failoverEnabled := true
			if ep.Config.FailoverEnabled != nil {
				failoverEnabled = *ep.Config.FailoverEnabled
			}
			if !failoverEnabled {
				continue
			}
			endpointsToCheck = append(endpointsToCheck, ep)
		}
		slog.Debug(fmt.Sprintf("🩺 [健康检查] 渠道间故障转移模式：检查 %d 个参与端点 (总端点 %d)",
			len(endpointsToCheck), len(snapshot)))
	} else {
		active := m.groupManager.FilterEndpointsByActiveGroups(snapshot)
		for _, ep := range active {
			if ep == nil {
				continue
			}
			failoverEnabled := true
			if ep.Config.FailoverEnabled != nil {
				failoverEnabled = *ep.Config.FailoverEnabled
			}
			if !failoverEnabled {
				continue
			}
			endpointsToCheck = append(endpointsToCheck, ep)
		}
		slog.Debug(fmt.Sprintf("🩺 [健康检查] 非故障转移模式：检查活跃渠道 %d 个参与端点 (总端点 %d)",
			len(endpointsToCheck), len(snapshot)))
	}

	if len(endpointsToCheck) == 0 {
		slog.Debug("🩺 [健康检查] 没有需要检查的端点，跳过健康检查")
		return
	}

	var wg sync.WaitGroup

	// Check the determined endpoints based on mode
	for _, endpoint := range endpointsToCheck {
		wg.Add(1)
		go func(ep *Endpoint) {
			defer wg.Done()
			m.checkEndpointHealth(ep)
		}(endpoint)
	}

	wg.Wait()

	// Count healthy endpoints after checks
	healthyCount := 0
	for _, ep := range endpointsToCheck {
		if ep.IsHealthy() {
			healthyCount++
		}
	}

	// v5.0+ Wails 桌面应用：定时健康检查完成后触发回调推送事件
	if m.onHealthCheckComplete != nil {
		go m.onHealthCheckComplete()
	}
}

// checkEndpointHealth checks the health of a single endpoint
func (m *Manager) checkEndpointHealth(endpoint *Endpoint) {
	start := time.Now()

	baseURL := strings.TrimSpace(endpoint.Config.URL)
	healthURL := baseURL + m.config.Health.HealthPath
	req, err := http.NewRequestWithContext(m.ctx, "GET", healthURL, nil)
	if err != nil {
		m.updateEndpointStatus(endpoint, false, 0)
		return
	}

	// Add authorization header with dynamically resolved token
	token := m.GetTokenForEndpoint(endpoint)
	if token == "" {
		token = m.GetApiKeyForEndpoint(endpoint)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := m.client.Do(req)
	responseTime := time.Since(start)

	if err != nil {
		m.updateEndpointStatus(endpoint, false, responseTime)
		return
	}

	resp.Body.Close()

	// Only consider 2xx as healthy for API endpoints
	// 2xx: Success responses only
	// All other status codes (including 4xx, 5xx) are considered unhealthy
	healthy := (resp.StatusCode >= 200 && resp.StatusCode < 300)

	m.updateEndpointStatus(endpoint, healthy, responseTime)
}

// updateEndpointStatus updates the health status of an endpoint
func (m *Manager) updateEndpointStatus(endpoint *Endpoint, healthy bool, responseTime time.Duration) {
	endpoint.mutex.Lock()

	endpoint.Status.LastCheck = time.Now()
	endpoint.Status.ResponseTime = responseTime
	endpoint.Status.NeverChecked = false // 标记为已检测

	// 记录状态变化前的健康状态
	wasUnhealthy := !endpoint.Status.Healthy

	if healthy {
		// Endpoint is healthy
		endpoint.Status.Healthy = true
		endpoint.Status.ConsecutiveFails = 0

		// Log recovery if endpoint was previously unhealthy
		if wasUnhealthy {
			slog.Info(fmt.Sprintf("✅ [健康检查] 端点恢复正常: %s - 响应时间: %dms",
				endpoint.Config.Name, responseTime.Milliseconds()))
		}
	} else {
		// Endpoint failed health check
		endpoint.Status.ConsecutiveFails++
		wasHealthy := endpoint.Status.Healthy

		// Mark as unhealthy immediately on any failure
		endpoint.Status.Healthy = false

		// Log the failure
		if wasHealthy {
			slog.Warn(fmt.Sprintf("❌ [健康检查] 端点标记为不可用: %s - 连续失败: %d次, 响应时间: %dms",
				endpoint.Config.Name, endpoint.Status.ConsecutiveFails, responseTime.Milliseconds()))
		}
	}

	endpoint.mutex.Unlock()

	// 通知Web界面端点状态变化
	go m.notifyWebInterface(endpoint)

	// v5.0+: 当端点从不健康变为健康时，重新评估组的激活状态
	// 这对新增端点后立即激活特别重要
	if healthy && wasUnhealthy {
		go m.refreshGroupActivation()
	}
}

// IsHealthy returns the health status of an endpoint
func (e *Endpoint) IsHealthy() bool {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.Status.Healthy
}

// GetResponseTime returns the last response time of an endpoint
func (e *Endpoint) GetResponseTime() time.Duration {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.Status.ResponseTime
}

// GetStatus returns a copy of the endpoint status
func (e *Endpoint) GetStatus() EndpointStatus {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.Status
}

// ManualHealthCheck performs a manual health check on a specific endpoint by name
func (m *Manager) ManualHealthCheck(endpointName string) error {
	var targetEndpoint *Endpoint

	// v5.0+: 使用读锁查找端点
	m.endpointsMu.RLock()
	for _, endpoint := range m.endpoints {
		if endpoint.Config.Name == endpointName {
			targetEndpoint = endpoint
			break
		}
	}
	m.endpointsMu.RUnlock()

	if targetEndpoint == nil {
		return fmt.Errorf("端点 '%s' 未找到", endpointName)
	}

	// Perform health check on the endpoint
	slog.Info(fmt.Sprintf("🔍 [手动检查] 开始检查端点: %s", endpointName))
	m.checkEndpointHealth(targetEndpoint)

	// Get status and log completion with response time
	status := targetEndpoint.Status
	healthStatus := "健康"
	if !status.Healthy {
		if status.NeverChecked {
			healthStatus = "未检测"
		} else {
			healthStatus = "不健康"
		}
	}

	slog.Info(fmt.Sprintf("🔍 [手动检查] 检查完成: %s - 状态: %s, 响应时间: %s",
		endpointName, healthStatus, utils.FormatResponseTime(status.ResponseTime)))

	return nil
}

// BatchHealthCheckAll 批量检测所有端点的健康状态
// 并发执行所有端点的健康检查，提高效率
// 使用信号量限制并发数量，避免资源耗尽
func (m *Manager) BatchHealthCheckAll() (int, int, error) {
	slog.Info("🔍 [批量健康检测] 开始检测所有端点")

	// v5.0+: 使用快照机制获取端点列表
	m.endpointsMu.RLock()
	endpoints := make([]*Endpoint, len(m.endpoints))
	copy(endpoints, m.endpoints)
	m.endpointsMu.RUnlock()

	if len(endpoints) == 0 {
		return 0, 0, fmt.Errorf("没有配置任何端点")
	}

	slog.Info(fmt.Sprintf("   共有 %d 个端点需要检测", len(endpoints)))

	// 使用信号量限制并发数量（最多 20 个并发）
	const maxConcurrentChecks = 20
	semaphore := make(chan struct{}, maxConcurrentChecks)

	// 使用 WaitGroup 并发检测所有端点
	var wg sync.WaitGroup
	var healthyCount, unhealthyCount int
	var countMu sync.Mutex

	for _, endpoint := range endpoints {
		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(ep *Endpoint) {
			defer wg.Done()
			defer func() { <-semaphore }() // 释放信号量

			// 执行健康检测（复用现有方法）
			m.checkEndpointHealth(ep)

			// 获取检测结果（需要加锁读取）
			ep.mutex.RLock()
			healthy := ep.Status.Healthy
			responseTime := ep.Status.ResponseTime
			ep.mutex.RUnlock()

			// 统计检测结果
			countMu.Lock()
			if healthy {
				healthyCount++
			} else {
				unhealthyCount++
			}
			countMu.Unlock()

			// 记录检测结果
			healthStatus := "❌ 不健康"
			if healthy {
				healthStatus = "✅ 健康"
			}
			slog.Debug(fmt.Sprintf("   检测端点: %s - 状态: %s, 响应时间: %s",
				ep.Config.Name,
				healthStatus,
				utils.FormatResponseTime(responseTime),
			))
		}(endpoint)
	}

	// 等待所有检测完成
	wg.Wait()

	slog.Info(fmt.Sprintf("✅ [批量健康检测] 完成，共检测 %d 个端点 (健康: %d, 不健康: %d)",
		len(endpoints), healthyCount, unhealthyCount))

	return healthyCount, unhealthyCount, nil
}
