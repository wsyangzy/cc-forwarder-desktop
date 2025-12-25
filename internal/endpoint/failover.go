// failover.go - 故障转移相关功能
// 包含请求级故障转移、冷却机制、端点切换等

package endpoint

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"
)

type groupCandidate struct {
	name      string
	priority  int
	bestRT    time.Duration
	hasBestRT bool
}

func (m *Manager) collectFailoverCandidates(excludeChannel string) []groupCandidate {
	// 说明：
	// - 可用性判定不依赖“响应时间快慢”，而依赖：未暂停、未处于渠道冷却、渠道内至少存在一个可用端点
	// - “fastest” 仅用于在候选渠道之间做排序（按渠道内可用端点的最小响应时间），不会因为慢就判定不可用
	now := time.Now()
	candidates := make([]groupCandidate, 0, 8)

	for _, g := range m.groupManager.GetAllGroups() {
		if g == nil || g.Name == "" || g.Name == excludeChannel {
			continue
		}
		if g.ManuallyPaused {
			continue
		}
		if !g.CooldownUntil.IsZero() && now.Before(g.CooldownUntil) {
			continue
		}

		// 渠道内至少有一个“可用端点”才视为可切换（与渠道内路由候选保持一致）
		hasAvailableEndpoint := false

		bestRT := time.Duration(math.MaxInt64)
		hasBestRT := false
		for _, ep := range g.Endpoints {
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

			ep.mutex.RLock()
			inEndpointCooldown := !ep.Status.CooldownUntil.IsZero() && now.Before(ep.Status.CooldownUntil)
			isHealthy := ep.Status.Healthy
			neverChecked := ep.Status.NeverChecked
			rt := ep.Status.ResponseTime
			ep.mutex.RUnlock()

			if inEndpointCooldown {
				continue
			}
			if !(isHealthy || neverChecked) {
				continue
			}

			hasAvailableEndpoint = true

			// fastest 策略的排序依据：优先使用“已测得”的响应时间；未测得则不参与 bestRT
			if rt > 0 {
				hasBestRT = true
				if rt < bestRT {
					bestRT = rt
				}
			}
		}

		if !hasAvailableEndpoint {
			continue
		}

		candidates = append(candidates, groupCandidate{
			name:      g.Name,
			priority:  g.Priority,
			bestRT:    bestRT,
			hasBestRT: hasBestRT,
		})
	}

	return candidates
}

func sortFailoverCandidates(strategy string, candidates []groupCandidate) {
	switch strategy {
	case "fastest":
		sort.Slice(candidates, func(i, j int) bool {
			// 有响应时间的优先于“未测得”的（避免完全随机）
			if candidates[i].hasBestRT != candidates[j].hasBestRT {
				return candidates[i].hasBestRT
			}
			if candidates[i].hasBestRT && candidates[j].hasBestRT && candidates[i].bestRT != candidates[j].bestRT {
				return candidates[i].bestRT < candidates[j].bestRT
			}
			// 回退：优先级越小越优先，其次名字稳定排序
			if candidates[i].priority != candidates[j].priority {
				return candidates[i].priority < candidates[j].priority
			}
			return candidates[i].name < candidates[j].name
		})
	default:
		// priority（或未知策略）：
		// 以渠道优先级（GroupInfo.Priority，v6.1+ 为 channel.priority）为主，保持稳定
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].priority != candidates[j].priority {
				return candidates[i].priority < candidates[j].priority
			}
			return candidates[i].name < candidates[j].name
		})
	}
}

// SelectNextAvailableChannel 选择下一个可用渠道（用于手动停用活跃渠道或请求级故障转移）。
// excludeChannel: 要排除的渠道（例如当前活跃/失败渠道）。
func (m *Manager) SelectNextAvailableChannel(excludeChannel string) (string, error) {
	if m == nil || m.groupManager == nil || m.config == nil {
		return "", fmt.Errorf("配置未初始化")
	}

	candidates := m.collectFailoverCandidates(excludeChannel)
	if len(candidates) == 0 {
		return "", fmt.Errorf("没有可用的故障转移渠道")
	}

	strategy := "priority"
	if m.config.Strategy.Type != "" {
		strategy = m.config.Strategy.Type
	}
	sortFailoverCandidates(strategy, candidates)

	return candidates[0].name, nil
}

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

	// 4) 选择并激活下一个可用渠道（由 strategy + 可用性共同决定）
	newChannel, err := m.SelectNextAvailableChannel(failedChannel)
	if err != nil {
		slog.Error("❌ [故障转移] 没有可用的故障转移渠道")
		return "", err
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
