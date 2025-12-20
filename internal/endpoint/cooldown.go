package endpoint

import (
	"fmt"
	"time"
)

// SetEndpointCooldown sets request-fail cooldown on a specific endpoint.
// 端点进入冷却后，在选择候选端点时会被跳过（直到冷却结束）。
func (m *Manager) SetEndpointCooldown(endpointName string, reason string) (time.Time, error) {
	ep := m.GetEndpointByNameAny(endpointName)
	if ep == nil {
		return time.Time{}, fmt.Errorf("端点 %s 不存在", endpointName)
	}

	cooldownDuration := m.config.Failover.DefaultCooldown
	if cooldownDuration == 0 {
		cooldownDuration = 10 * time.Minute
	}
	if ep.Config.Cooldown != nil && *ep.Config.Cooldown > 0 {
		cooldownDuration = *ep.Config.Cooldown
	}

	until := time.Now().Add(cooldownDuration)

	ep.mutex.Lock()
	ep.Status.CooldownUntil = until
	ep.Status.CooldownReason = reason
	ep.mutex.Unlock()

	return until, nil
}

