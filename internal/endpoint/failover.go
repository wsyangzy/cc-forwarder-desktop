// failover.go - 故障转移相关功能
// 包含请求级故障转移、冷却机制、端点切换等

package endpoint

import (
	"fmt"
	"log/slog"
	"time"
)

// SetOnFailoverTriggered 设置故障转移回调
// 当请求失败触发“跨渠道”故障转移时调用，用于同步数据库
func (m *Manager) SetOnFailoverTriggered(fn func(failedChannel, newChannel string)) {
	m.onFailoverTriggered = fn
}

// TriggerRequestFailover 触发请求级故障转移
// 当请求在某端点上失败达到重试上限时调用
// 返回: 新激活的渠道名，如果没有可用渠道则返回空字符串
func (m *Manager) TriggerRequestFailover(failedEndpointName string, reason string) (string, error) {
	slog.Warn(fmt.Sprintf("🔄 [故障转移] 触发请求级故障转移: %s, 原因: %s", failedEndpointName, reason))

	// 未启用故障转移时不进行跨渠道切换（保持配置语义）
	if m.config == nil {
		return "", fmt.Errorf("配置未初始化")
	}
	autoSwitchEnabled := m.config.Failover.Enabled || m.config.Group.AutoSwitchBetweenGroups
	if !autoSwitchEnabled {
		return "", fmt.Errorf("故障转移未启用")
	}

	// 1. 找到失败的端点并设置冷却
	failedEndpoint := m.GetEndpointByNameAny(failedEndpointName)
	if failedEndpoint == nil {
		return "", fmt.Errorf("端点 %s 不存在", failedEndpointName)
	}

	failedChannel := ChannelKey(failedEndpoint)

	until, err := m.SetEndpointCooldown(failedEndpointName, reason)
	if err != nil {
		return "", err
	}
	slog.Info(fmt.Sprintf("⏱️ [故障转移] 端点 %s 进入冷却，恢复时间: %s", failedEndpointName, until.Format("15:04:05")))

	// 2. 将失败端点所属渠道置为冷却，触发跨渠道切换
	m.groupManager.SetGroupCooldown(failedChannel)

	// 3. 选择并激活下一个可用渠道（按优先级从高到低）
	var newChannel string
	now := time.Now()
	for _, g := range m.groupManager.GetAllGroups() {
		if g.Name == "" || g.Name == failedChannel {
			continue
		}
		if g.ManuallyPaused {
			continue
		}
		if !g.CooldownUntil.IsZero() && now.Before(g.CooldownUntil) {
			continue
		}

		// 组内至少有一个可用端点才视为可切换
		hasAvailableEndpoint := false
		for _, ep := range g.Endpoints {
			failoverEnabled := true
			if ep.Config.FailoverEnabled != nil {
				failoverEnabled = *ep.Config.FailoverEnabled
			}
			if !failoverEnabled {
				continue
			}

			ep.mutex.RLock()
			inEndpointCooldown := !ep.Status.CooldownUntil.IsZero() && now.Before(ep.Status.CooldownUntil)
			isHealthy := ep.Status.Healthy
			neverChecked := ep.Status.NeverChecked
			ep.mutex.RUnlock()

			if (isHealthy || neverChecked) && !inEndpointCooldown {
				hasAvailableEndpoint = true
				break
			}
		}

		if hasAvailableEndpoint {
			newChannel = g.Name
			break
		}
	}

	if newChannel == "" {
		slog.Error("❌ [故障转移] 没有可用的故障转移渠道")
		return "", fmt.Errorf("没有可用的故障转移渠道")
	}

	if err := m.groupManager.ManualActivateGroup(newChannel); err != nil {
		slog.Error(fmt.Sprintf("❌ [故障转移] 激活新渠道失败: %v", err))
		return "", fmt.Errorf("激活新渠道失败: %w", err)
	}

	slog.Info(fmt.Sprintf("✅ [故障转移] 已切换到渠道: %s", newChannel))

	// 5. 调用回调通知 App 层同步数据库
	if m.onFailoverTriggered != nil {
		go m.onFailoverTriggered(failedChannel, newChannel)
	}

	// 6. 触发前端刷新
	if m.onHealthCheckComplete != nil {
		go m.onHealthCheckComplete()
	}

	return newChannel, nil
}

// IsEndpointInCooldown 检查端点是否在冷却中
func (m *Manager) IsEndpointInCooldown(name string) bool {
	ep := m.GetEndpointByNameAny(name)
	if ep == nil {
		return false
	}

	ep.mutex.RLock()
	defer ep.mutex.RUnlock()

	return !ep.Status.CooldownUntil.IsZero() && time.Now().Before(ep.Status.CooldownUntil)
}

// ClearEndpointCooldown 清除端点冷却状态（用于手动激活时）
func (m *Manager) ClearEndpointCooldown(name string) {
	ep := m.GetEndpointByNameAny(name)
	if ep == nil {
		return
	}

	ep.mutex.Lock()
	defer ep.mutex.Unlock()

	if !ep.Status.CooldownUntil.IsZero() {
		slog.Info(fmt.Sprintf("🔓 [冷却] 清除端点冷却: %s (原因: %s)", name, ep.Status.CooldownReason))
		ep.Status.CooldownUntil = time.Time{}
		ep.Status.CooldownReason = ""
	}
}

// GetEndpointCooldownInfo 获取端点冷却信息
func (m *Manager) GetEndpointCooldownInfo(name string) (inCooldown bool, until time.Time, reason string) {
	ep := m.GetEndpointByNameAny(name)
	if ep == nil {
		return false, time.Time{}, ""
	}

	ep.mutex.RLock()
	defer ep.mutex.RUnlock()

	now := time.Now()
	inCooldown = !ep.Status.CooldownUntil.IsZero() && now.Before(ep.Status.CooldownUntil)
	return inCooldown, ep.Status.CooldownUntil, ep.Status.CooldownReason
}
