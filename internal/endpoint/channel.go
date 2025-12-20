package endpoint

import "cc-forwarder/config"

// ChannelKeyFromConfig returns the routing channel key for an endpoint.
//
// 规则：
// - 如果配置了 channel，则按 channel 分组路由与故障转移
// - 否则回退到 endpoint name（保持旧版本“一端点一组”兼容）
func ChannelKeyFromConfig(cfg config.EndpointConfig) string {
	if cfg.Channel != "" {
		return cfg.Channel
	}
	return cfg.Name
}

// ChannelKey returns the routing channel key for an endpoint instance.
func ChannelKey(ep *Endpoint) string {
	if ep == nil {
		return ""
	}
	return ChannelKeyFromConfig(ep.Config)
}

