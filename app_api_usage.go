// app_api_usage.go - 使用统计 API (Wails Bindings)
// 包含使用统计摘要、请求记录查询等功能

package main

import (
	"context"
	"log/slog"
	"time"

	"cc-forwarder/internal/tracking"
)

const usageDBQueryTimeout = 8 * time.Second

// ============================================================
// 使用统计 API
// ============================================================

// UsageSummary 使用统计摘要
type UsageSummary struct {
	TotalRequests        int64   `json:"total_requests"`          // 运行时请求数
	AllTimeTotalRequests int64   `json:"all_time_total_requests"` // 全部历史请求数（数据库）
	TodayRequests        int64   `json:"today_requests"`          // 今日请求数（数据库）
	SuccessRequests      int64   `json:"success_requests"`
	FailedRequests       int64   `json:"failed_requests"`
	TotalInputTokens     int64   `json:"total_input_tokens"`
	TotalOutputTokens    int64   `json:"total_output_tokens"`
	TotalCost            float64 `json:"total_cost"`            // 运行时成本
	TodayCost            float64 `json:"today_cost"`            // 今日成本（数据库）
	AllTimeTotalCost     float64 `json:"all_time_total_cost"`   // 全部历史成本（数据库）
	TodayTokens          int64   `json:"today_tokens"`          // 今日 tokens（数据库）
	AllTimeTotalTokens   int64   `json:"all_time_total_tokens"` // 全部历史 tokens（数据库）
}

// GetUsageSummary 获取使用统计摘要
// 当没有传递时间参数时，返回运行时统计（从内存）+ 全部历史请求总数（从数据库）
// 当传递时间参数时，返回历史数据（从数据库）
func (a *App) GetUsageSummary(startTimeStr, endTimeStr string) (UsageSummary, error) {
	a.mu.RLock()
	monitoringMiddleware := a.monitoringMiddleware
	usageTracker := a.usageTracker
	cfg := a.config
	logger := a.logger
	a.mu.RUnlock()

	// 如果没有传递时间参数，返回运行时统计（与 Web 版本 /api/v1/connections 一致）
	if startTimeStr == "" && endTimeStr == "" {
		if monitoringMiddleware == nil {
			return UsageSummary{}, nil
		}

		// 从内存中获取运行时统计
		metrics := monitoringMiddleware.GetMetrics()
		stats := metrics.GetMetrics()

		// 计算总 Token
		totalInputTokens := stats.TotalTokenUsage.InputTokens
		totalOutputTokens := stats.TotalTokenUsage.OutputTokens

		// 查询数据库获取全部历史和今日统计
		var allTimeTotalCost float64
		var allTimeTotalTokens int64
		var allTimeTotal int64
		var todayCost float64
		var todayTokens int64
		var todayRequests int64

		if usageTracker != nil {
			// 获取配置的时区
			loc := time.Local
			if cfg != nil && cfg.Timezone != "" {
				if parsedLoc, err := time.LoadLocation(cfg.Timezone); err == nil {
					loc = parsedLoc
				}
			}

			// 直接从 request_logs 表查询全部历史统计
			ctx, cancel := context.WithTimeout(context.Background(), usageDBQueryTimeout)
			allTimeTotalCost, allTimeTotalTokens, allTimeTotal = queryStatsFromDB(ctx, logger, usageTracker, time.Time{}, time.Now())
			cancel()

			// 查询今日统计（使用配置的时区）
			now := time.Now().In(loc)
			todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
			todayEnd := todayStart.Add(24 * time.Hour)
			ctx, cancel = context.WithTimeout(context.Background(), usageDBQueryTimeout)
			todayCost, todayTokens, todayRequests = queryStatsFromDB(ctx, logger, usageTracker, todayStart, todayEnd)
			cancel()
		}

		return UsageSummary{
			TotalRequests:        stats.TotalRequests,
			AllTimeTotalRequests: allTimeTotal,
			TodayRequests:        todayRequests,
			SuccessRequests:      stats.SuccessfulRequests,
			FailedRequests:       stats.FailedRequests,
			TotalInputTokens:     totalInputTokens,
			TotalOutputTokens:    totalOutputTokens,
			TotalCost:            0, // 运行时统计不计算成本
			TodayCost:            todayCost,
			AllTimeTotalCost:     allTimeTotalCost,
			TodayTokens:          todayTokens,
			AllTimeTotalTokens:   allTimeTotalTokens,
		}, nil
	}

	// 传递了时间参数，从数据库查询历史数据
	if usageTracker == nil {
		return UsageSummary{}, nil
	}

	// 解析时间范围
	var startTime, endTime time.Time
	var err error

	if startTimeStr != "" {
		startTime, err = time.Parse(time.RFC3339, startTimeStr)
		if err != nil {
			startTime = time.Now().AddDate(0, 0, -7) // 默认最近7天
		}
	} else {
		startTime = time.Now().AddDate(0, 0, -7)
	}

	if endTimeStr != "" {
		endTime, err = time.Parse(time.RFC3339, endTimeStr)
		if err != nil {
			endTime = time.Now()
		}
	} else {
		endTime = time.Now()
	}

	ctx, cancel := context.WithTimeout(context.Background(), usageDBQueryTimeout)
	defer cancel()
	summaries, err := usageTracker.GetUsageSummary(ctx, startTime, endTime)
	if err != nil {
		return UsageSummary{}, err
	}

	// 聚合所有摘要
	result := UsageSummary{}
	for _, s := range summaries {
		result.TotalRequests += int64(s.RequestCount)
		result.SuccessRequests += int64(s.SuccessCount)
		result.FailedRequests += int64(s.ErrorCount)
		result.TotalInputTokens += s.TotalInputTokens
		result.TotalOutputTokens += s.TotalOutputTokens
		result.TotalCost += s.TotalCostUSD
	}

	return result, nil
}

// queryStatsFromDB 直接从 request_logs 表查询成本、tokens 和请求数
func queryStatsFromDB(ctx context.Context, logger *slog.Logger, usageTracker *tracking.UsageTracker, startTime, endTime time.Time) (cost float64, tokens int64, requests int64) {
	if usageTracker == nil {
		return 0, 0, 0
	}

	db := usageTracker.GetDB()
	if db == nil {
		return 0, 0, 0
	}

	var query string
	var args []interface{}

	if startTime.IsZero() {
		// 查询全部历史（包含所有 token 类型：输入、输出、缓存创建、缓存读取）
		query = "SELECT COALESCE(SUM(total_cost_usd), 0), COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0), COUNT(*) FROM request_logs"
	} else {
		// 查询指定时间范围（包含所有 token 类型）
		query = "SELECT COALESCE(SUM(total_cost_usd), 0), COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0), COUNT(*) FROM request_logs WHERE start_time >= ? AND start_time < ?"
		args = append(args, startTime, endTime)
	}

	err := db.QueryRowContext(ctx, query, args...).Scan(&cost, &tokens, &requests)
	if err != nil {
		if logger != nil {
			logger.Debug("查询统计数据失败", "error", err)
		}
		return 0, 0, 0
	}

	return cost, tokens, requests
}

// RequestRecord 请求记录
type RequestRecord struct {
	ID                    string  `json:"id"`
	RequestID             string  `json:"request_id"`
	Timestamp             string  `json:"timestamp"`
	Channel               string  `json:"channel"` // v5.0: 渠道标签
	Endpoint              string  `json:"endpoint"`
	Group                 string  `json:"group"`
	Model                 string  `json:"model"`
	AuthType              string  `json:"auth_type,omitempty"` // token / api_key / ''
	AuthKey               string  `json:"auth_key,omitempty"`  // 脱敏标识（指纹/别名@指纹），不含明文
	Status                string  `json:"status"`
	HTTPStatus            int     `json:"http_status"`
	RetryCount            int     `json:"retry_count"`              // 重试次数
	FailureReason         string  `json:"failure_reason,omitempty"` // 失败原因
	CancelReason          string  `json:"cancel_reason,omitempty"`  // 取消原因
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`    // 总缓存创建（向后兼容）
	CacheCreation5mTokens int64   `json:"cache_creation_5m_tokens"` // v5.0.1: 5分钟缓存
	CacheCreation1hTokens int64   `json:"cache_creation_1h_tokens"` // v5.0.1: 1小时缓存
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	ResponseTime          int64   `json:"response_time"`
	IsStreaming           bool    `json:"is_streaming"`
	Cost                  float64 `json:"cost"`
}

// RequestListResult 请求列表结果
type RequestListResult struct {
	Requests []RequestRecord `json:"requests"`
	Total    int             `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

// RequestQueryParams 请求查询参数
type RequestQueryParams struct {
	Page      int    `json:"page"`
	PageSize  int    `json:"page_size"`
	StartDate string `json:"start_date"` // 格式：2025-12-05T00:00 或 2025-12-05T00:00:00+08:00
	EndDate   string `json:"end_date"`   // 格式：2025-12-05T23:59 或 2025-12-05T23:59:59+08:00
	Status    string `json:"status"`     // 可选：completed, failed, pending 等
	Model     string `json:"model"`      // 可选：模型名称
	Channel   string `json:"channel"`    // 可选：渠道名称（v5.0）
	Endpoint  string `json:"endpoint"`   // 可选：端点名称
	Group     string `json:"group"`      // 可选：组名称
}

// GetRequests 获取请求记录列表（热池+数据库双源查询）
// 支持筛选参数：时间范围、状态、模型等
func (a *App) GetRequests(params RequestQueryParams) (RequestListResult, error) {
	a.mu.RLock()
	usageTracker := a.usageTracker
	cfg := a.config
	a.mu.RUnlock()

	if usageTracker == nil {
		return RequestListResult{}, nil
	}

	page := params.Page
	pageSize := params.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	offset := (page - 1) * pageSize

	ctx, cancel := context.WithTimeout(context.Background(), usageDBQueryTimeout)
	defer cancel()

	// 解析时间参数（使用配置的时区）
	loc := time.Local
	if cfg != nil && cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}

	var startTime, endTime time.Time

	// 解析开始时间
	if params.StartDate != "" {
		if t, err := parseTimeWithLocation(params.StartDate, loc); err == nil {
			startTime = t
		}
	}
	// 解析结束时间
	if params.EndDate != "" {
		if t, err := parseTimeWithLocation(params.EndDate, loc); err == nil {
			endTime = t
		}
	}

	// 如果没有传时间参数，默认查询今天
	if startTime.IsZero() {
		now := time.Now().In(loc)
		startTime = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	}
	if endTime.IsZero() {
		now := time.Now().In(loc)
		endTime = time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, loc)
	}

	// 使用热池+数据库双源查询（与 HTTP API 一致）
	opts := &tracking.QueryOptions{
		StartDate:    &startTime,
		EndDate:      &endTime,
		ModelName:    params.Model,
		Channel:      params.Channel, // v5.0: 渠道筛选
		EndpointName: params.Endpoint,
		GroupName:    params.Group,
		Status:       params.Status,
		Limit:        pageSize,
		Offset:       offset,
	}

	requests, total, err := usageTracker.QueryRequestDetailsWithHotPool(ctx, opts)
	if err != nil {
		return RequestListResult{}, err
	}

	result := RequestListResult{
		Requests: make([]RequestRecord, 0, len(requests)),
		Page:     page,
		PageSize: pageSize,
		Total:    int(total),
	}

	for _, r := range requests {
		// 使用统一的时间格式（2025-12-04 17:18:48）
		// 数据库存储的就是配置时区的时间，直接格式化，不做时区转换
		record := RequestRecord{
			RequestID:             r.RequestID,
			Timestamp:             r.StartTime.Format("2006-01-02 15:04:05"),
			Channel:               r.Channel, // v5.0: 渠道标签
			Endpoint:              r.EndpointName,
			Group:                 r.GroupName,
			Model:                 r.ModelName,
			AuthType:              r.AuthType,
			AuthKey:               r.AuthKey,
			Status:                r.Status,
			RetryCount:            r.RetryCount,
			FailureReason:         r.FailureReason,
			CancelReason:          r.CancelReason,
			InputTokens:           r.InputTokens,
			OutputTokens:          r.OutputTokens,
			CacheCreationTokens:   r.CacheCreationTokens,
			CacheCreation5mTokens: r.CacheCreation5mTokens, // v5.0.1+
			CacheCreation1hTokens: r.CacheCreation1hTokens, // v5.0.1+
			CacheReadTokens:       r.CacheReadTokens,
			IsStreaming:           r.IsStreaming,
			Cost:                  r.TotalCostUSD,
		}

		// 处理指针字段
		if r.HTTPStatusCode != nil {
			record.HTTPStatus = *r.HTTPStatusCode
		}
		if r.DurationMs != nil {
			record.ResponseTime = *r.DurationMs
		}

		result.Requests = append(result.Requests, record)
	}

	return result, nil
}

// ============================================================
// 使用统计 API (与 HTTP API 格式一致)
// ============================================================

// UsageStatsData 使用统计数据（与 HTTP API /api/v1/usage/stats 格式一致）
type UsageStatsData struct {
	Period        string  `json:"period"`
	TotalRequests int     `json:"total_requests"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	TotalTokens   int64   `json:"total_tokens"`
	FailedCount   int     `json:"failed_requests"`
}

// UsageStatsQueryParams 使用统计查询参数
type UsageStatsQueryParams struct {
	Period    string `json:"period"`     // 时间周期: "1h", "1d", "7d", "30d", "90d"
	StartDate string `json:"start_date"` // 开始时间（优先于 period）
	EndDate   string `json:"end_date"`   // 结束时间（优先于 period）
	Status    string `json:"status"`     // 可选：状态筛选
	Model     string `json:"model"`      // 可选：模型筛选
	Channel   string `json:"channel"`    // 可选：渠道筛选（v5.0）
	Endpoint  string `json:"endpoint"`   // 可选：端点筛选
	Group     string `json:"group"`      // 可选：组筛选
}

// GetUsageStats 获取使用统计（与 HTTP API 格式一致）
// 支持完整筛选参数，与前端 buildQueryParams() 配合
func (a *App) GetUsageStats(params UsageStatsQueryParams) (UsageStatsData, error) {
	a.mu.RLock()
	usageTracker := a.usageTracker
	cfg := a.config
	a.mu.RUnlock()

	period := params.Period
	if period == "" {
		period = "30d"
	}

	result := UsageStatsData{
		Period: period,
	}

	// 解析时间参数（使用配置的时区）
	loc := time.Local
	if cfg != nil && cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			loc = l
		}
	}

	var startTime, endTime time.Time
	var useCustomRange bool

	// 优先使用自定义时间范围
	if params.StartDate != "" {
		if t, err := parseTimeWithLocation(params.StartDate, loc); err == nil {
			startTime = t
			useCustomRange = true
		}
	}
	if params.EndDate != "" {
		if t, err := parseTimeWithLocation(params.EndDate, loc); err == nil {
			endTime = t
			useCustomRange = true
		}
	}

	// 如果没有自定义时间，使用 period 计算
	if !useCustomRange {
		endTime = time.Now()
		switch period {
		case "1h":
			startTime = endTime.Add(-1 * time.Hour)
		case "1d":
			startTime = endTime.AddDate(0, 0, -1)
		case "7d":
			startTime = endTime.AddDate(0, 0, -7)
		case "30d":
			startTime = endTime.AddDate(0, 0, -30)
		case "90d":
			startTime = endTime.AddDate(0, 0, -90)
		default:
			startTime = endTime.AddDate(0, 0, -30)
			result.Period = "30d"
		}
	}

	// 如果有 usageTracker，从热池+数据库组合查询（与 HTTP API 一致）
	if usageTracker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// 使用聚合查询：热池 + 数据库双源统计（避免拉取海量明细导致 UI 卡顿）
		opts := &tracking.QueryOptions{
			StartDate:    &startTime,
			EndDate:      &endTime,
			ModelName:    params.Model,
			Channel:      params.Channel, // v5.0: 渠道筛选
			EndpointName: params.Endpoint,
			GroupName:    params.Group,
			Status:       params.Status,
			Limit:        0,
			Offset:       0,
		}

		totals, err := usageTracker.QueryUsageStatsTotalsWithHotPool(ctx, opts)
		if err == nil && totals != nil {
			result.TotalRequests = int(totals.TotalRequests)
			result.FailedCount = int(totals.FailedRequests)
			result.TotalTokens = totals.TotalTokens
			result.TotalCostUSD = totals.TotalCostUSD
			result.AvgDurationMs = totals.AvgDurationMs()

			if totals.TotalRequests > 0 {
				result.SuccessRate = float64(totals.SuccessRequests) / float64(totals.TotalRequests) * 100
			}

			return result, nil
		}
		// 查询失败时，继续使用运行时统计（降级方案）
	}

	// 从运行时 Metrics 获取统计（降级方案，或无 usageTracker 时）
	return a.getUsageStatsFromRuntime(period), nil
}

// getUsageStatsFromRuntime 从运行时内存获取统计数据
func (a *App) getUsageStatsFromRuntime(period string) UsageStatsData {
	result := UsageStatsData{
		Period: period,
	}

	if a.monitoringMiddleware == nil {
		return result
	}

	metrics := a.monitoringMiddleware.GetMetrics()
	if metrics == nil {
		return result
	}

	stats := metrics.GetMetrics()

	// 从运行时统计计算
	result.TotalRequests = int(stats.TotalRequests)
	result.FailedCount = int(stats.FailedRequests)

	// 计算成功率
	if stats.TotalRequests > 0 {
		result.SuccessRate = float64(stats.SuccessfulRequests) / float64(stats.TotalRequests) * 100
	}

	// 计算总 Token
	result.TotalTokens = stats.TotalTokenUsage.InputTokens + stats.TotalTokenUsage.OutputTokens +
		stats.TotalTokenUsage.CacheCreationTokens + stats.TotalTokenUsage.CacheReadTokens

	// 计算平均耗时（使用 Metrics 提供的方法）
	avgResponseTime := metrics.GetAverageResponseTime()
	result.AvgDurationMs = float64(avgResponseTime.Milliseconds())

	// 运行时不计算成本
	result.TotalCostUSD = 0

	return result
}

// ============================================================
// Token 使用统计 API
// ============================================================

// TokenUsageData Token 使用数据结构
type TokenUsageData struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

// GetTokenUsage 获取当前 Token 使用统计（运行时内存数据）
func (a *App) GetTokenUsage() TokenUsageData {
	a.mu.RLock()
	monitoringMiddleware := a.monitoringMiddleware
	a.mu.RUnlock()

	if monitoringMiddleware == nil {
		return TokenUsageData{}
	}

	metrics := monitoringMiddleware.GetMetrics()
	if metrics == nil {
		return TokenUsageData{}
	}

	tokenStats := metrics.GetTotalTokenStats()

	return TokenUsageData{
		InputTokens:         tokenStats.InputTokens,
		OutputTokens:        tokenStats.OutputTokens,
		CacheCreationTokens: tokenStats.CacheCreationTokens,
		CacheReadTokens:     tokenStats.CacheReadTokens,
		TotalTokens:         tokenStats.InputTokens + tokenStats.OutputTokens + tokenStats.CacheCreationTokens + tokenStats.CacheReadTokens,
	}
}
