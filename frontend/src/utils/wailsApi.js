// ============================================
// Wails API 适配层
// 2025-12-04
// 将 Wails Bindings 包装为与 HTTP API 兼容的接口
// ============================================

// 检测是否在 Wails 环境中运行
export const isWailsEnvironment = () => {
  return typeof window !== 'undefined' && window.go !== undefined;
};

// 动态导入 Wails 绑定（避免在非 Wails 环境中报错）
let WailsApp = null;
let WailsRuntime = null;

const loadWailsBindings = async () => {
  if (!isWailsEnvironment()) {
    return false;
  }

  try {
    WailsApp = await import('@wailsjs/go/main/App');
    WailsRuntime = await import('@wailsjs/runtime/runtime');
    return true;
  } catch (error) {
    console.warn('Failed to load Wails bindings:', error);
    return false;
  }
};

// 初始化 Wails 绑定
let wailsInitialized = false;
let wailsInitPromise = null;

export const initWails = async () => {
  if (wailsInitialized) return true;
  if (wailsInitPromise) return wailsInitPromise;

  wailsInitPromise = loadWailsBindings().then(result => {
    wailsInitialized = result;
    return result;
  });

  return wailsInitPromise;
};

// ============================================
// Wails Events 适配
// ============================================

// 事件监听器映射
const eventListeners = new Map();

/**
 * 订阅 Wails 事件
 * @param {string} eventName - 事件名称
 * @param {Function} callback - 回调函数
 * @returns {Function} - 取消订阅函数
 */
export const subscribeToEvent = (eventName, callback) => {
  // 异步初始化后订阅
  let unsubscribeFunc = null;
  let isUnsubscribed = false;

  console.log(`📡 [subscribeToEvent] 开始订阅事件: ${eventName}`);

  initWails().then(() => {
    if (isUnsubscribed) {
      console.log(`📡 [subscribeToEvent] 事件 ${eventName} 已取消订阅，跳过注册`);
      return;
    }

    if (!WailsRuntime) {
      console.warn(`📡 [subscribeToEvent] Wails Runtime not loaded, 无法订阅 ${eventName}`);
      return;
    }

    console.log(`📡 [subscribeToEvent] 注册事件监听: ${eventName}`);
    unsubscribeFunc = WailsRuntime.EventsOn(eventName, (data) => {
      console.log(`📡 [subscribeToEvent] 收到事件 ${eventName}:`, data);
      callback(data);
    });

    // 存储监听器以便清理
    if (!eventListeners.has(eventName)) {
      eventListeners.set(eventName, []);
    }
    eventListeners.get(eventName).push({ callback, unsubscribe: unsubscribeFunc });
  });

  // 返回取消订阅函数
  return () => {
    isUnsubscribed = true;
    if (unsubscribeFunc && typeof unsubscribeFunc === 'function') {
      unsubscribeFunc();
    }
  };
};

/**
 * 取消订阅所有事件
 */
export const unsubscribeAll = () => {
  if (!WailsRuntime) return;

  eventListeners.forEach((listeners, eventName) => {
    listeners.forEach(({ unsubscribe }) => {
      if (typeof unsubscribe === 'function') {
        unsubscribe();
      }
    });
  });
  eventListeners.clear();
};

// ============================================
// 系统状态 API
// ============================================

export const getSystemStatus = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const status = await WailsApp.GetSystemStatus();

  // 转换为前端期望的格式
  return {
    status: status.proxy_running ? 'running' : 'stopped',
    version: status.version,
    uptime: status.uptime_seconds,
    start_time: status.start_time, // ISO8601 格式的启动时间
    proxy_running: status.proxy_running,
    proxy_port: status.proxy_port,
    proxy_host: status.proxy_host,
    active_group: status.active_group,
    config_path: status.config_path,
    auth_enabled: status.auth_enabled
  };
};

export const getConfig = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  return await WailsApp.GetConfig();
};

// ============================================
// 端点管理 API
// ============================================

export const getEndpoints = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const endpoints = await WailsApp.GetEndpoints();

  // 转换为前端期望的格式
  const formattedEndpoints = endpoints.map(ep => ({
    name: ep.name,
    url: ep.url,
    channel: ep.channel || '', // v5.0: 渠道标签
    group: ep.group,
    priority: ep.priority,
    group_priority: ep.group_priority,
    group_is_active: ep.group_is_active,
    healthy: ep.healthy,
    status: ep.healthy ? 'healthy' : 'unhealthy',
    last_check: ep.last_check,
    response_time: ep.response_time_ms,
    consecutive_fail: ep.consecutive_fail,
    never_checked: !ep.last_check
  }));

  const healthy = formattedEndpoints.filter(e => e.healthy).length;

  return {
    endpoints: formattedEndpoints,
    total: formattedEndpoints.length,
    healthy
  };
};

export const setEndpointPriority = async (endpointName, priority) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.SetEndpointPriority(endpointName, priority);
  return { success: true };
};

export const triggerHealthCheck = async (endpointName) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.TriggerHealthCheck(endpointName);
  return { success: true, healthy: true };
};

export const batchHealthCheckAll = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const result = await WailsApp.BatchHealthCheckAll();
  return {
    success: result.success,
    message: result.message,
    total: result.total,
    healthy_count: result.healthy_count,
    unhealthy_count: result.unhealthy_count
  };
};

// ============================================
// Key 管理 API
// ============================================

export const getKeysOverview = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  console.log('🔑 [Wails] 调用 GetKeysOverview...');
  const result = await WailsApp.GetKeysOverview();
  console.log('🔑 [Wails] GetKeysOverview 原始返回:', result);

  // 转换为前端期望的格式
  const endpoints = (result.endpoints || []).map(ep => ({
    endpoint: ep.endpoint,
    tokens: (ep.tokens || []).map(t => ({
      index: t.index,
      name: t.name || `Token ${t.index + 1}`,
      masked: t.value,  // 后端返回的是 value 字段（已脱敏）
      is_active: t.is_active
    })),
    api_keys: (ep.api_keys || []).map(k => ({
      index: k.index,
      name: k.name || `API Key ${k.index + 1}`,
      masked: k.value,  // 后端返回的是 value 字段（已脱敏）
      is_active: k.is_active
    })),
    current_token_index: ep.current_token_index,
    current_api_key_index: ep.current_api_key_index
  }));

  const formatted = {
    endpoints,
    total: endpoints.length,
    timestamp: result.timestamp
  };
  console.log('🔑 [Wails] GetKeysOverview 格式化后:', formatted);
  return formatted;
};

export const switchKey = async (endpointName, keyType, index) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const result = await WailsApp.SwitchKey(endpointName, keyType, index);
  return {
    success: result.success,
    message: result.message,
    endpoint: result.endpoint,
    key_type: result.key_type,
    new_index: result.new_index
  };
};

// ============================================
// 组管理 API
// ============================================

export const getGroups = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 获取组、端点和 Keys 信息
  const [groups, endpointsData, keysData] = await Promise.all([
    WailsApp.GetGroups(),
    WailsApp.GetEndpoints(),
    WailsApp.GetKeysOverview().catch(() => ({ endpoints: [] })) // 容错：获取 keys 失败不影响主流程
  ]);

  // v6.0: 组名 = 渠道(channel)（后端已在 GetEndpoints 中映射到 ep.group）
  // 计算每个组的健康端点统计
  const groupHealthMap = new Map();
  endpointsData.forEach(ep => {
    const groupName = ep.group || ep.channel || ep.name;
    if (!groupHealthMap.has(groupName)) {
      groupHealthMap.set(groupName, { total: 0, healthy: 0 });
    }
    const stats = groupHealthMap.get(groupName);
    stats.total++;
    if (ep.healthy) {
      stats.healthy++;
    }
  });

  // 构建端点名到 tokens 的映射
  const endpointTokensMap = new Map();
  (keysData.endpoints || []).forEach(ep => {
    const tokens = (ep.tokens || []).map(t => ({
      index: t.index,
      name: t.name || `Token ${t.index + 1}`,
      key: t.value, // 脱敏的 key 值
      is_active: t.is_active,
      endpoint: ep.endpoint, // 关联的端点名
      type: inferTokenType(t.name) // 推断 Token 类型
    }));
    endpointTokensMap.set(ep.endpoint, tokens);
  });

  // 将端点 tokens 聚合到组（渠道）
  const groupTokensMap = new Map();
  endpointsData.forEach(ep => {
    const groupName = ep.group || ep.channel || ep.name;
    const endpointTokens = endpointTokensMap.get(ep.name) || [];
    if (!groupTokensMap.has(groupName)) {
      groupTokensMap.set(groupName, []);
    }
    const existing = groupTokensMap.get(groupName);
    existing.push(...endpointTokens);
  });

  // 推断 Token 类型的辅助函数
  function inferTokenType(name) {
    if (!name) return 'Std';
    const lowerName = name.toLowerCase();
    if (lowerName.includes('pro') || lowerName.includes('特价')) return 'Pro';
    if (lowerName.includes('ent') || lowerName.includes('主号')) return 'Ent';
    if (lowerName.includes('free') || lowerName.includes('测试')) return 'Free';
    return 'Std';
  }

  // 转换为前端期望的格式
  const formattedGroups = groups.map(g => {
    const healthStats = groupHealthMap.get(g.name) || { total: 0, healthy: 0 };
    // v6.0: 组名 = 渠道(channel)，聚合组内所有端点 tokens
    const tokens = groupTokensMap.get(g.name) || [];

    return {
      name: g.name,
      channel: g.channel,  // v5.0: 渠道名称
      is_active: g.active,
      paused: g.paused,
      priority: g.priority,
      endpoint_count: g.endpoint_count,
      total_endpoints: healthStats.total,
      healthy_endpoints: healthStats.healthy,
      unhealthy_endpoints: healthStats.total - healthStats.healthy,
      in_cooldown: g.in_cooldown,
      cooldown_remain_ms: g.cooldown_remain_ms,
      tokens: tokens // 添加 tokens 数组
    };
  });

  const activeGroup = formattedGroups.find(g => g.is_active);

  return {
    groups: formattedGroups,
    active_group: activeGroup?.name || null,
    total_suspended_requests: 0
  };
};

// 获取后端原始组信息（轻量：仅 GetGroups，不做额外聚合）
export const getGroupsRaw = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const groups = await WailsApp.GetGroups();
  return Array.isArray(groups) ? groups : [];
};

export const activateGroup = async (groupName) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.ActivateGroup(groupName);
  return { success: true };
};

export const pauseGroup = async (groupName) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.PauseGroup(groupName);
  return { success: true };
};

export const resumeGroup = async (groupName) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.ResumeGroup(groupName);
  return { success: true };
};

// ============================================
// 使用统计 API
// ============================================

/**
 * 获取使用统计（与 HTTP API 格式一致）
 * @param {Object} params - 查询参数
 * @param {string} params.period - 时间周期: "1h", "1d", "7d", "30d", "90d"
 * @param {string} params.start_date - 开始时间（优先于 period）
 * @param {string} params.end_date - 结束时间（优先于 period）
 * @param {string} params.status - 状态筛选
 * @param {string} params.model - 模型筛选
 * @param {string} params.endpoint - 端点筛选
 * @param {string} params.group - 组筛选
 * @returns {Promise<Object>} - 统计数据
 */
export const getUsageStats = async (params = {}) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 构建查询参数对象
  // 注意：前端 buildQueryParams() 返回 start_date/end_date（带下划线）
  const queryParams = {
    period: params.period || '30d',
    start_date: params.start_date || '',
    end_date: params.end_date || '',
    status: params.status || '',
    model: params.model || '',
    channel: params.channel || '',
    endpoint: params.endpoint || '',
    group: params.group || ''
  };

  console.log('📊 [Wails] GetUsageStats 参数:', queryParams);
  const data = await WailsApp.GetUsageStats(queryParams);

  // 返回与 HTTP API 一致的格式
  return {
    period: data.period || queryParams.period,
    total_requests: data.total_requests || 0,
    success_rate: data.success_rate || 0,
    avg_duration_ms: data.avg_duration_ms || 0,
    total_cost_usd: data.total_cost_usd || 0,
    total_tokens: data.total_tokens || 0,
    failed_requests: data.failed_requests || 0
  };
};

export const getUsageSummary = async (startTime = '', endTime = '') => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const summary = await WailsApp.GetUsageSummary(startTime, endTime);

  return {
    total_requests: summary.total_requests || 0,
    all_time_total_requests: summary.all_time_total_requests || 0, // 全部历史请求数
    today_requests: summary.today_requests || 0,            // 今日请求数
    successful_requests: summary.success_requests || 0,
    failed_requests: summary.failed_requests || 0,
    total_input_tokens: summary.total_input_tokens || 0,
    total_output_tokens: summary.total_output_tokens || 0,
    total_cost: summary.total_cost || 0,
    today_cost: summary.today_cost || 0,                    // 今日成本
    all_time_total_cost: summary.all_time_total_cost || 0,  // 全部历史成本
    today_tokens: summary.today_tokens || 0,                // 今日 tokens
    all_time_total_tokens: summary.all_time_total_tokens || 0,  // 全部历史 tokens
    total_tokens: (summary.total_input_tokens || 0) + (summary.total_output_tokens || 0)
  };
};

export const getRequests = async (params = {}) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 构建 Wails 绑定参数对象
  // 注意：前端 buildQueryParams() 返回 start_date/end_date（带下划线）
  const queryParams = {
    page: parseInt(params.page || 1),
    page_size: parseInt(params.limit || params.pageSize || 50),
    start_date: params.start_date || params.start_time || params.startDate || '',
    end_date: params.end_date || params.end_time || params.endDate || '',
    status: params.status || '',
    model: params.model || '',
    channel: params.channel || '',
    endpoint: params.endpoint || '',
    group: params.group || ''
  };

  console.log('🔍 [Wails] GetRequests 参数:', queryParams);
  const result = await WailsApp.GetRequests(queryParams);
  console.log('🔍 [Wails] GetRequests 返回:', result);

  // 转换请求记录格式
  const requests = (result.requests || []).map(r => ({
    request_id: r.request_id,
    id: r.request_id,
    timestamp: r.timestamp,
    start_time: r.timestamp,
    channel: r.channel || '',
    endpoint_name: r.endpoint,
    endpoint: r.endpoint,
    group_name: r.group,
    group: r.group,
    model_name: r.model,
    model: r.model,
    status: r.status,
    status_code: r.http_status,
    http_status_code: r.http_status,  // 添加 http_status_code 映射
    retry_count: r.retry_count || 0,  // 添加重试次数
    input_tokens: r.input_tokens,
    output_tokens: r.output_tokens,
    cache_creation_tokens: r.cache_creation_tokens,
    cache_creation_5m_tokens: r.cache_creation_5m_tokens,  // v5.0.1+
    cache_creation_1h_tokens: r.cache_creation_1h_tokens,  // v5.0.1+
    cache_read_tokens: r.cache_read_tokens,
    duration_ms: r.response_time,
    duration: r.response_time,
    is_streaming: r.is_streaming,
    total_cost_usd: r.cost,
    cost: r.cost,
    // 错误信息字段
    failure_reason: r.failure_reason || '',
    cancel_reason: r.cancel_reason || ''
  }));

  return {
    requests,
    total: result.total,
    page: result.page,
    pageSize: result.page_size,
    totalPages: Math.ceil(result.total / result.page_size)
  };
};

// ============================================
// 代理信息
// ============================================

export const getProxyURL = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  return await WailsApp.GetProxyURL();
};

export const isProxyRunning = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  return await WailsApp.IsProxyRunning();
};

// ============================================
// 图表数据 API
// ============================================

/**
 * 获取请求趋势图表数据
 * @param {number} minutes - 时间范围（分钟）
 * @returns {Promise<Array>} - 图表数据点数组 [{time, total, success, fail}]
 */
export const getRequestTrendChart = async (minutes = 30) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  console.log('📊 [Wails] 调用 GetRequestTrendChart, minutes:', minutes);
  const data = await WailsApp.GetRequestTrendChart(minutes);
  console.log('📊 [Wails] GetRequestTrendChart 返回:', data ? `${data.length} 个数据点` : '无数据', data);
  return data || [];
};

/**
 * 获取响应时间图表数据
 * @param {number} minutes - 时间范围（分钟）
 * @returns {Promise<Array>} - 图表数据点数组 [{time, avg, min, max}]
 */
export const getResponseTimeChart = async (minutes = 30) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const data = await WailsApp.GetResponseTimeChart(minutes);
  return data || [];
};

/**
 * 获取连接活动图表数据
 * @param {number} minutes - 时间范围（分钟）
 * @returns {Promise<Array>} - 图表数据点数组 [{time, value}]
 */
export const getConnectionActivityChart = async (minutes = 60) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const data = await WailsApp.GetConnectionActivityChart(minutes);
  // 转换为前端期望的格式 (connections 字段)
  return (data || []).map(point => ({
    time: point.time,
    connections: point.value
  }));
};

// ============================================
// Token 使用统计 API
// ============================================

/**
 * 获取 Token 使用统计（运行时内存数据）
 * @returns {Promise<Object>} - Token 使用数据 {input, output, cacheCreation, cacheRead}
 */
export const getTokenUsage = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const data = await WailsApp.GetTokenUsage();
  return {
    input: data.input_tokens || 0,
    output: data.output_tokens || 0,
    cacheCreation: data.cache_creation_tokens || 0,
    cacheRead: data.cache_read_tokens || 0,
    total: data.total_tokens || 0
  };
};

// ============================================
// 端点健康状态图表 API
// ============================================

/**
 * 获取端点健康状态数据
 * @returns {Promise<Object>} - 健康状态数据 {healthy, unhealthy, total}
 */
export const getEndpointHealthChart = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const data = await WailsApp.GetEndpointHealthChart();
  return {
    healthy: data.healthy || 0,
    unhealthy: data.unhealthy || 0,
    total: data.total || 0
  };
};

// ============================================
// 端点成本图表 API
// ============================================

/**
 * 获取当日端点成本数据
 * @returns {Promise<Array>} - 端点成本数据 [{name, tokens, cost}]
 */
export const getEndpointCosts = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const data = await WailsApp.GetEndpointCosts();
  // 数据已经是 [{name, tokens, cost}] 格式
  return data || [];
};

// ============================================
// v5.0+ 端点存储管理 API (SQLite)
// ============================================

/**
 * 获取端点存储状态
 * @returns {Promise<Object>} - {enabled, storage_type, total_count, enabled_count}
 */
export const getEndpointStorageStatus = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const status = await WailsApp.GetEndpointStorageStatus();
  return {
    enabled: status.enabled || false,
    storageType: status.storage_type || 'yaml',
    totalCount: status.total_count || 0,
    enabledCount: status.enabled_count || 0
  };
};

/**
 * 获取所有端点记录（SQLite 存储）
 * @returns {Promise<Array>} - 端点记录数组
 */
export const getEndpointRecords = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const records = await WailsApp.GetEndpointRecords();
  return (records || []).map(r => ({
    id: r.id,
    channel: r.channel,
    name: r.name,
    url: r.url,
    token: r.token,       // v5.0: 本地桌面应用，直接返回原始 Token
    apiKey: r.api_key,    // v5.0: 本地桌面应用，直接返回原始 ApiKey
    tokenMasked: r.token_masked,
    apiKeyMasked: r.api_key_masked,
    headers: r.headers || {},
    priority: r.priority,
    failoverEnabled: r.failover_enabled,
    cooldownSeconds: r.cooldown_seconds,
    timeoutSeconds: r.timeout_seconds,
    supportsCountTokens: r.supports_count_tokens,
    costMultiplier: r.cost_multiplier,
    inputCostMultiplier: r.input_cost_multiplier,
    outputCostMultiplier: r.output_cost_multiplier,
    cacheCreationCostMultiplier: r.cache_creation_cost_multiplier,
    cacheCreationCostMultiplier1h: r.cache_creation_cost_multiplier_1h,
    cacheReadCostMultiplier: r.cache_read_cost_multiplier,
    enabled: r.enabled,
    createdAt: r.created_at,
    updatedAt: r.updated_at,
    healthy: r.healthy,
    lastCheck: r.last_check,
    responseTimeMs: r.response_time_ms,
    // 冷却状态（请求级故障转移）
    in_cooldown: r.in_cooldown,
    inCooldown: r.in_cooldown,
    cooldown_until: r.cooldown_until,
    cooldownUntil: r.cooldown_until,
    cooldown_reason: r.cooldown_reason,
    cooldownReason: r.cooldown_reason
  }));
};

/**
 * 获取单个端点记录
 * @param {string} name - 端点名称
 * @returns {Promise<Object>} - 端点记录
 */
export const getEndpointRecord = async (name) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const r = await WailsApp.GetEndpointRecord(name);
  return {
    id: r.id,
    channel: r.channel,
    name: r.name,
    url: r.url,
    tokenMasked: r.token_masked,
    apiKeyMasked: r.api_key_masked,
    headers: r.headers || {},
    priority: r.priority,
    failoverEnabled: r.failover_enabled,
    cooldownSeconds: r.cooldown_seconds,
    timeoutSeconds: r.timeout_seconds,
    supportsCountTokens: r.supports_count_tokens,
    costMultiplier: r.cost_multiplier,
    enabled: r.enabled,
    createdAt: r.created_at,
    updatedAt: r.updated_at,
    healthy: r.healthy,
    responseTimeMs: r.response_time_ms
  };
};

/**
 * 创建新端点
 * @param {Object} input - 端点配置
 * @returns {Promise<void>}
 */
export const createEndpointRecord = async (input) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 转换为后端期望的格式
  const record = {
    channel: input.channel || '',
    name: input.name || '',
    url: input.url || '',
    token: input.token || '',
    api_key: input.apiKey || '',
    headers: input.headers || {},
    priority: parseInt(input.priority) || 1,
    failover_enabled: input.failoverEnabled !== false,
    cooldown_seconds: input.cooldownSeconds ? parseInt(input.cooldownSeconds) : null,
    timeout_seconds: parseInt(input.timeoutSeconds) || 300,
    supports_count_tokens: input.supportsCountTokens || false,
    cost_multiplier: parseFloat(input.costMultiplier) || 1.0,
    input_cost_multiplier: parseFloat(input.inputCostMultiplier) || 1.0,
    output_cost_multiplier: parseFloat(input.outputCostMultiplier) || 1.0,
    cache_creation_cost_multiplier: parseFloat(input.cacheCreationCostMultiplier) || 1.0,
    cache_creation_cost_multiplier_1h: parseFloat(input.cacheCreationCostMultiplier1h) || 1.0,
    cache_read_cost_multiplier: parseFloat(input.cacheReadCostMultiplier) || 1.0
  };

  await WailsApp.CreateEndpointRecord(record);
  return { success: true };
};

/**
 * 更新端点配置
 * @param {string} name - 端点名称
 * @param {Object} input - 更新的配置
 * @returns {Promise<void>}
 */
export const updateEndpointRecord = async (name, input) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 转换为后端期望的格式
  const record = {
    channel: input.channel || '',
    name: input.name || name,
    url: input.url || '',
    token: input.token || '',
    api_key: input.apiKey || '',
    headers: input.headers || {},
    priority: parseInt(input.priority) || 1,
    failover_enabled: input.failoverEnabled !== false,
    cooldown_seconds: input.cooldownSeconds ? parseInt(input.cooldownSeconds) : null,
    timeout_seconds: parseInt(input.timeoutSeconds) || 300,
    supports_count_tokens: input.supportsCountTokens || false,
    cost_multiplier: parseFloat(input.costMultiplier) || 1.0,
    input_cost_multiplier: parseFloat(input.inputCostMultiplier) || 1.0,
    output_cost_multiplier: parseFloat(input.outputCostMultiplier) || 1.0,
    cache_creation_cost_multiplier: parseFloat(input.cacheCreationCostMultiplier) || 1.0,
    cache_creation_cost_multiplier_1h: parseFloat(input.cacheCreationCostMultiplier1h) || 1.0,
    cache_read_cost_multiplier: parseFloat(input.cacheReadCostMultiplier) || 1.0
  };

  await WailsApp.UpdateEndpointRecord(name, record);
  return { success: true };
};

export const setEndpointFailoverEnabled = async (name, enabled) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.SetEndpointFailoverEnabled(name, !!enabled);
  return { success: true };
};

/**
 * 删除端点
 * @param {string} name - 端点名称
 * @returns {Promise<void>}
 */
export const deleteEndpointRecord = async (name) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.DeleteEndpointRecord(name);
  return { success: true };
};

/**
 * 切换端点启用状态
 * @param {string} name - 端点名称
 * @param {boolean} enabled - 是否启用
 * @returns {Promise<void>}
 */
export const toggleEndpointRecord = async (name, enabled) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.ToggleEndpointRecord(name, enabled);
  return { success: true };
};

/**
 * 获取所有渠道
 * @returns {Promise<Array>} - 渠道列表 [{name, endpointCount}]
 */
export const getChannels = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const channels = await WailsApp.GetChannels();
  return (channels || []).map(c => ({
    name: c.name,
    website: c.website || '',
    endpointCount: c.endpoint_count
  }));
};

export const createChannel = async (input) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const payload = {
    name: (input?.name || '').trim(),
    website: (input?.website || '').trim()
  };

  await WailsApp.CreateChannel(payload);
  return { success: true };
};

export const deleteChannel = async (name, deleteEndpoints = false) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const channelName = (name || '').trim();
  if (!channelName) throw new Error('渠道名称不能为空');

  await WailsApp.DeleteChannel(channelName, !!deleteEndpoints);
  return { success: true };
};

/**
 * 按渠道获取端点
 * @param {string} channel - 渠道名称
 * @returns {Promise<Array>} - 端点记录数组
 */
export const getEndpointsByChannel = async (channel) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const records = await WailsApp.GetEndpointsByChannel(channel);
  return (records || []).map(r => ({
    id: r.id,
    channel: r.channel,
    name: r.name,
    url: r.url,
    tokenMasked: r.token_masked,
    priority: r.priority,
    failoverEnabled: r.failover_enabled,
    enabled: r.enabled,
    healthy: r.healthy,
    responseTimeMs: r.response_time_ms
  }));
};

// ============================================
// v5.0+ 模型定价管理 API (SQLite)
// ============================================

/**
 * 获取模型定价存储状态
 * @returns {Promise<Object>} - {enabled, totalCount, hasDefault}
 */
export const getModelPricingStorageStatus = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const status = await WailsApp.GetModelPricingStorageStatus();
  return {
    enabled: status.enabled || false,
    totalCount: status.total_count || 0,
    hasDefault: status.has_default || false
  };
};

/**
 * 获取所有模型定价
 * @returns {Promise<Array>} - 模型定价数组
 */
export const getModelPricings = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const records = await WailsApp.GetModelPricings();
  return (records || []).map(r => ({
    id: r.id,
    modelName: r.model_name,
    displayName: r.display_name,
    description: r.description,
    inputPrice: r.input_price,
    outputPrice: r.output_price,
    cacheCreationPrice5m: r.cache_creation_price_5m,
    cacheCreationPrice1h: r.cache_creation_price_1h,
    cacheReadPrice: r.cache_read_price,
    isDefault: r.is_default,
    createdAt: r.created_at,
    updatedAt: r.updated_at
  }));
};

/**
 * 获取单个模型定价
 * @param {string} modelName - 模型名称
 * @returns {Promise<Object>} - 模型定价记录
 */
export const getModelPricing = async (modelName) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const r = await WailsApp.GetModelPricing(modelName);
  return {
    id: r.id,
    modelName: r.model_name,
    displayName: r.display_name,
    description: r.description,
    inputPrice: r.input_price,
    outputPrice: r.output_price,
    cacheCreationPrice5m: r.cache_creation_price_5m,
    cacheCreationPrice1h: r.cache_creation_price_1h,
    cacheReadPrice: r.cache_read_price,
    isDefault: r.is_default,
    createdAt: r.created_at,
    updatedAt: r.updated_at
  };
};

/**
 * 创建模型定价
 * @param {Object} input - 定价配置
 * @returns {Promise<Object>} - {success: true}
 */
export const createModelPricing = async (input) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 转换为后端期望的格式
  const record = {
    model_name: input.modelName || '',
    display_name: input.displayName || '',
    description: input.description || '',
    input_price: parseFloat(input.inputPrice) || 3.0,
    output_price: parseFloat(input.outputPrice) || 15.0,
    cache_creation_price_5m: parseFloat(input.cacheCreationPrice5m) || 0,
    cache_creation_price_1h: parseFloat(input.cacheCreationPrice1h) || 0,
    cache_read_price: parseFloat(input.cacheReadPrice) || 0,
    is_default: input.isDefault || false
  };

  await WailsApp.CreateModelPricing(record);
  return { success: true };
};

/**
 * 更新模型定价
 * @param {string} modelName - 模型名称
 * @param {Object} input - 更新的配置
 * @returns {Promise<Object>} - {success: true}
 */
export const updateModelPricing = async (modelName, input) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 转换为后端期望的格式
  const record = {
    model_name: input.modelName || modelName,
    display_name: input.displayName || '',
    description: input.description || '',
    input_price: parseFloat(input.inputPrice) || 3.0,
    output_price: parseFloat(input.outputPrice) || 15.0,
    cache_creation_price_5m: parseFloat(input.cacheCreationPrice5m) || 0,
    cache_creation_price_1h: parseFloat(input.cacheCreationPrice1h) || 0,
    cache_read_price: parseFloat(input.cacheReadPrice) || 0,
    is_default: input.isDefault || false
  };

  await WailsApp.UpdateModelPricing(modelName, record);
  return { success: true };
};

/**
 * 删除模型定价
 * @param {string} modelName - 模型名称
 * @returns {Promise<Object>} - {success: true}
 */
export const deleteModelPricing = async (modelName) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.DeleteModelPricing(modelName);
  return { success: true };
};

/**
 * 设置默认模型定价
 * @param {string} modelName - 模型名称
 * @returns {Promise<Object>} - {success: true}
 */
export const setDefaultModelPricing = async (modelName) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.SetDefaultModelPricing(modelName);
  return { success: true };
};

// ============================================
// v5.1+ 系统设置管理 API (SQLite)
// ============================================

/**
 * 获取设置存储状态
 * @returns {Promise<Object>} - {enabled, totalCount, categoryCount, isInitialized}
 */
export const getSettingsStorageStatus = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const status = await WailsApp.GetSettingsStorageStatus();
  return {
    enabled: status.enabled || false,
    totalCount: status.total_count || 0,
    categoryCount: status.category_count || 0,
    isInitialized: status.is_initialized || false
  };
};

/**
 * 获取所有设置分类
 * @returns {Promise<Array>} - 分类信息数组
 */
export const getSettingCategories = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const categories = await WailsApp.GetSettingCategories();
  return (categories || []).map(c => ({
    name: c.name,
    label: c.label,
    description: c.description,
    icon: c.icon,
    order: c.order
  }));
};

/**
 * 获取所有设置
 * @returns {Promise<Array>} - 设置数组
 */
export const getAllSettings = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const settings = await WailsApp.GetAllSettings();
  return (settings || []).map(s => ({
    id: s.id,
    category: s.category,
    key: s.key,
    value: s.value,
    value_type: s.value_type,
    label: s.label,
    description: s.description,
    display_order: s.display_order,
    requires_restart: s.requires_restart,
    created_at: s.created_at,
    updated_at: s.updated_at
  }));
};

/**
 * 获取指定分类的设置
 * @param {string} category - 分类名称
 * @returns {Promise<Array>} - 设置数组
 */
export const getSettingsByCategory = async (category) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const settings = await WailsApp.GetSettingsByCategory(category);
  return (settings || []).map(s => ({
    id: s.id,
    category: s.category,
    key: s.key,
    value: s.value,
    value_type: s.value_type,
    label: s.label,
    description: s.description,
    display_order: s.display_order,
    requires_restart: s.requires_restart,
    created_at: s.created_at,
    updated_at: s.updated_at
  }));
};

/**
 * 获取单个设置
 * @param {string} category - 分类名称
 * @param {string} key - 设置键
 * @returns {Promise<Object>} - 设置记录
 */
export const getSetting = async (category, key) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const s = await WailsApp.GetSetting(category, key);
  return {
    id: s.id,
    category: s.category,
    key: s.key,
    value: s.value,
    value_type: s.value_type,
    label: s.label,
    description: s.description,
    display_order: s.display_order,
    requires_restart: s.requires_restart,
    created_at: s.created_at,
    updated_at: s.updated_at
  };
};

/**
 * 更新单个设置
 * @param {string} category - 分类名称
 * @param {string} key - 设置键
 * @param {string} value - 新值
 * @returns {Promise<Object>} - {success: true}
 */
export const updateSetting = async (category, key, value) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.UpdateSetting({
    category,
    key,
    value
  });
  return { success: true };
};

/**
 * 批量更新设置
 * @param {Object|Array} input - 设置对象 {settings: [...]} 或设置数组 [{category, key, value}]
 * @returns {Promise<Object>} - {success: true}
 */
export const batchUpdateSettings = async (input) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  // 兼容两种调用方式：对象 {settings: [...]} 或数组 [...]
  const settingsArray = Array.isArray(input) ? input : (input.settings || []);

  await WailsApp.BatchUpdateSettings({
    settings: settingsArray.map(s => ({
      category: s.category,
      key: s.key,
      value: s.value
    }))
  });
  return { success: true };
};

/**
 * 重置分类设置为默认值
 * @param {string} category - 分类名称
 * @returns {Promise<Object>} - {success: true}
 */
export const resetCategorySettings = async (category) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.ResetCategorySettings(category);
  return { success: true };
};

/**
 * 获取端口信息
 * @returns {Promise<Object>} - {preferred_port, actual_port, is_default, was_occupied}
 */
export const getPortInfo = async () => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  const info = await WailsApp.GetPortInfo();
  return {
    preferred_port: info.preferred_port,
    actual_port: info.actual_port,
    is_default: info.is_default,
    was_occupied: info.was_occupied
  };
};

/**
 * 更新首选端口（需要重启生效）
 * @param {number} port - 新端口号
 * @returns {Promise<Object>} - {success: true}
 */
export const updatePreferredPort = async (port) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  await WailsApp.UpdatePreferredPort(port);
  return { success: true };
};

/**
 * 检查端口是否可用
 * @param {number} port - 端口号
 * @returns {Promise<boolean>} - 是否可用
 */
export const checkPortAvailable = async (port) => {
  await initWails();
  if (!WailsApp) throw new Error('Wails not available');

  return await WailsApp.CheckPortAvailable(port);
};
