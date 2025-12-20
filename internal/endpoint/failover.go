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

// TriggerRequestFailoverWithFailedEndpoints 触发请求级故障转移（跨渠道切换）。
//
// 语义：
// - 由上层在“当前渠道内所有端点都已尝试且重试耗尽”时调用
// - 会将本次请求中失败过的端点统一进入冷却，避免下一次请求立即重复撞同一批端点
// - 然后将失败渠道置为冷却，并切换到下一个可用渠道
//
// 返回: 新激活的渠道名，如果没有可用渠道则返回空字符串
func (m *Manager) TriggerRequestFailoverWithFailedEndpoints(failedEndpointNames []string, reason string) (string, error) {
	slog.Warn(fmt.Sprintf("🔄 [故障转移] 触发请求级故障转移，原因: %s", reason))

	// 未启用故障转移时不进行跨渠道切换（保持配置语义）
	if m.config == nil {
		return "", fmt.Errorf("配置未初始化")
	}
	if !m.config.Failover.Enabled {
		return "", fmt.Errorf("故障转移未启用")
	}

	// 1) 去重 + 找到失败渠道
	uniqueNames := make([]string, 0, len(failedEndpointNames))
	seen := make(map[string]struct{}, len(failedEndpointNames))
	for _, name := range failedEndpointNames {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		uniqueNames = append(uniqueNames, name)
	}
	if len(uniqueNames) == 0 {
		return "", fmt.Errorf("缺少失败端点信息")
	}

	var failedChannel string
	for _, name := range uniqueNames {
		ep := m.GetEndpointByNameAny(name)
		if ep == nil {
			continue
		}
		failedChannel = ChannelKey(ep)
		break
	}
	if failedChannel == "" {
		return "", fmt.Errorf("无法解析失败渠道（端点不存在或未初始化）")
	}

	// 2) 失败端点统一进入冷却（最佳努力：不阻塞跨渠道切换）
	cooldownApplied := 0
	var lastUntil time.Time
	for _, name := range uniqueNames {
		ep := m.GetEndpointByNameAny(name)
		if ep == nil {
			slog.Warn(fmt.Sprintf("⚠️ [故障转移] 设置端点冷却失败：端点不存在: %s", name))
			continue
		}
		if ChannelKey(ep) != failedChannel {
			slog.Warn(fmt.Sprintf("⚠️ [故障转移] 跳过不属于失败渠道的端点冷却: %s (channel=%s, failed_channel=%s)",
				name, ChannelKey(ep), failedChannel))
			continue
		}

		until, err := m.SetEndpointCooldown(name, reason)
		if err != nil {
			slog.Warn(fmt.Sprintf("⚠️ [故障转移] 设置端点冷却失败: %s, 错误: %v", name, err))
			continue
		}
		lastUntil = until
		cooldownApplied++
	}
	if cooldownApplied > 0 {
		slog.Info(fmt.Sprintf("⏱️ [故障转移] 端点冷却已应用: channel=%s endpoints=%d 恢复时间(示例): %s",
			failedChannel, cooldownApplied, lastUntil.Format("15:04:05")))
	}

	// 3) 将失败渠道置为冷却，触发跨渠道切换
	m.groupManager.SetGroupCooldown(failedChannel)

	// 4) 选择并激活下一个可用渠道（按优先级从高到低）
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
		// 兼容：新渠道端点可能处于 neverChecked（尚未健康检查），但请求级故障转移应允许先切过去尝试。
		// 这里回退到强制激活，避免因“尚未健康检查”而无法完成跨渠道切换。
		slog.Warn(fmt.Sprintf("⚠️ [故障转移] 常规激活新渠道失败，回退强制激活: %s, 错误: %v", newChannel, err))
		if err2 := m.groupManager.ManualActivateGroupWithForce(newChannel, true); err2 != nil {
			slog.Error(fmt.Sprintf("❌ [故障转移] 强制激活新渠道失败: %v", err2))
			return "", fmt.Errorf("激活新渠道失败: %w", err2)
		}
	}

	slog.Info(fmt.Sprintf("✅ [故障转移] 已切换到渠道: %s", newChannel))

	// 5) 调用回调通知 App 层同步数据库
	if m.onFailoverTriggered != nil {
		go m.onFailoverTriggered(failedChannel, newChannel)
	}

	// 6) 触发前端刷新
	if m.onHealthCheckComplete != nil {
		go m.onHealthCheckComplete()
	}

	return newChannel, nil
}

// TriggerRequestFailover 兼容旧签名：仅传入最后失败的端点。
func (m *Manager) TriggerRequestFailover(failedEndpointName string, reason string) (string, error) {
	return m.TriggerRequestFailoverWithFailedEndpoints([]string{failedEndpointName}, reason)
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
