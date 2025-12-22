package endpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// GetAuthIdentityForEndpoint returns a non-sensitive auth identity for billing/analytics.
//
// Important:
// - Never returns raw token/api-key values.
// - "fastest/priority" route selection uses endpoint health/priority, not this method.
//
// Returned values:
// - authType: "token" | "api_key" | ""
// - authKey:  a stable, non-reversible identifier like "token2@ab12cd34ef56..."
func (m *Manager) GetAuthIdentityForEndpoint(ep *Endpoint) (authType string, authKey string) {
	if m == nil || ep == nil {
		return "", ""
	}

	// Prefer Token over ApiKey (mirrors Forwarder behavior: Authorization takes precedence conceptually).
	if label, value, ok := m.getActiveTokenLabelAndValue(ep); ok && value != "" {
		return "token", formatAuthKey(label, value)
	}
	if label, value, ok := m.getActiveApiKeyLabelAndValue(ep); ok && value != "" {
		return "api_key", formatAuthKey(label, value)
	}
	return "", ""
}

func (m *Manager) getActiveTokenLabelAndValue(ep *Endpoint) (label string, value string, ok bool) {
	// 1) Multi tokens (endpoint-local)
	if len(ep.Config.Tokens) > 0 {
		idx := m.keyManager.GetActiveTokenIndex(ep.Config.Name)
		if idx < 0 || idx >= len(ep.Config.Tokens) {
			idx = 0
		}
		t := ep.Config.Tokens[idx]
		label = t.Name
		if label == "" {
			label = fmt.Sprintf("token%d", idx+1)
		}
		return label, t.Value, true
	}

	// 2) Single token
	if ep.Config.Token != "" {
		return "default", ep.Config.Token, true
	}

	// 3) Group inheritance (legacy: single token only)
	groupName := ep.Config.Group
	if groupName == "" {
		groupName = "Default"
	}

	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()
	for _, e := range m.endpoints {
		endpointGroup := e.Config.Group
		if endpointGroup == "" {
			endpointGroup = "Default"
		}
		if endpointGroup == groupName && e.Config.Token != "" {
			return "default", e.Config.Token, true
		}
	}

	return "", "", false
}

func (m *Manager) getActiveApiKeyLabelAndValue(ep *Endpoint) (label string, value string, ok bool) {
	// 1) Multi api-keys (endpoint-local)
	if len(ep.Config.ApiKeys) > 0 {
		idx := m.keyManager.GetActiveApiKeyIndex(ep.Config.Name)
		if idx < 0 || idx >= len(ep.Config.ApiKeys) {
			idx = 0
		}
		k := ep.Config.ApiKeys[idx]
		label = k.Name
		if label == "" {
			label = fmt.Sprintf("apiKey%d", idx+1)
		}
		return label, k.Value, true
	}

	// 2) Single api-key
	if ep.Config.ApiKey != "" {
		return "default", ep.Config.ApiKey, true
	}

	// 3) Group inheritance (legacy: single api-key only)
	groupName := ep.Config.Group
	if groupName == "" {
		groupName = "Default"
	}

	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()
	for _, e := range m.endpoints {
		endpointGroup := e.Config.Group
		if endpointGroup == "" {
			endpointGroup = "Default"
		}
		if endpointGroup == groupName && e.Config.ApiKey != "" {
			return "default", e.Config.ApiKey, true
		}
	}

	return "", "", false
}

func formatAuthKey(label, raw string) string {
	fp := fingerprintSecret(raw)
	if label == "" {
		return fp
	}
	return fmt.Sprintf("%s@%s", label, fp)
}

func fingerprintSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	// 16 hex chars = 64 bits, stable enough and short for UI.
	return hex.EncodeToString(sum[:])[:16]
}

