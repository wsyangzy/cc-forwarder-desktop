// ============================================
// useFilters Hook - 筛选器状态管理
// 2025-11-28 17:02:47
// ============================================

import { useState, useCallback, useMemo } from 'react';
import { DEFAULT_FILTERS } from '../utils/constants.js';

/**
 * 获取当天时间范围
 * @returns {{ startDate: string, endDate: string }}
 */
export const getTodayTimeRange = () => {
  const now = new Date();

  // 当天开始时间 (00:00)
  const startOfDay = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 0, 0, 0);

  // 当天结束时间 (23:59)
  const endOfDay = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 59, 59);

  // 转换为 datetime-local 格式 (YYYY-MM-DDTHH:mm)
  const formatDateTime = (date) => {
    const year = date.getFullYear();
    const month = String(date.getMonth() + 1).padStart(2, '0');
    const day = String(date.getDate()).padStart(2, '0');
    const hours = String(date.getHours()).padStart(2, '0');
    const minutes = String(date.getMinutes()).padStart(2, '0');
    return `${year}-${month}-${day}T${hours}:${minutes}`;
  };

  return {
    startDate: formatDateTime(startOfDay),
    endDate: formatDateTime(endOfDay)
  };
};

/**
 * 创建初始筛选器状态
 * @returns {Object}
 */
const createInitialFilters = () => {
  const todayRange = getTodayTimeRange();
  return {
    ...DEFAULT_FILTERS,
    startDate: todayRange.startDate,
    endDate: todayRange.endDate
  };
};

/**
 * useFilters Hook - 管理筛选器状态和查询参数构建
 * @param {Object} initialFilters - 初始筛选器状态
 * @returns {Object}
 */
export const useFilters = (initialFilters = {}) => {
  // 筛选器状态
  const [filters, setFilters] = useState(() => ({
    ...createInitialFilters(),
    ...initialFilters
  }));

  // 更新单个筛选器
  const updateFilter = useCallback((key, value) => {
    setFilters(prev => ({ ...prev, [key]: value }));
  }, []);

  // 批量更新筛选器
  const updateFilters = useCallback((newFilters) => {
    setFilters(prev => ({ ...prev, ...newFilters }));
  }, []);

  // 重置筛选器
  const resetFilters = useCallback(() => {
    setFilters(createInitialFilters());
  }, []);

  // 构建 API 查询参数
  const buildQueryParams = useCallback(() => {
    const queryParams = {};

    // 处理时间筛选
    // 后端 parseTimeWithLocation 支持不带时区的时间字符串，并会使用后端配置的时区进行解析。
    const ensureSeconds = (value) => {
      if (!value) return '';
      // datetime-local 通常为 YYYY-MM-DDTHH:mm
      // 若包含秒则直接返回
      const lastColon = value.lastIndexOf(':');
      if (lastColon > 0 && value.length - lastColon - 1 === 2 && value.includes('T')) {
        // 形如 ...:mm，补齐秒
        if (value.length === 16) return value + ':00';
      }
      return value;
    };

    if (filters.startDate) {
      queryParams.start_date = ensureSeconds(filters.startDate);
    }
    if (filters.endDate) {
      queryParams.end_date = ensureSeconds(filters.endDate);
    }

    // 处理状态筛选 - 'all' 表示不筛选
    if (filters.status && filters.status !== 'all') {
      queryParams.status = filters.status;
    }

    // 处理模型筛选 - 空字符串表示不筛选
    if (filters.model && filters.model !== '') {
      queryParams.model = filters.model;
    }

    // 处理渠道筛选
    if (filters.channel && filters.channel !== 'all') {
      queryParams.channel = filters.channel;
    }

    // 处理其他筛选条件
    if (filters.endpoint && filters.endpoint !== 'all') {
      queryParams.endpoint = filters.endpoint;
    }
    if (filters.group && filters.group !== 'all') {
      queryParams.group = filters.group;
    }

    return queryParams;
  }, [filters]);

  // 检查是否有活动筛选器
  const hasActiveFilters = useMemo(() => {
    const defaultFilters = createInitialFilters();
    return Object.entries(filters).some(([key, value]) => {
      if (key === 'startDate' || key === 'endDate') {
        return value && value !== '';
      }
      return value && value !== defaultFilters[key] && value !== 'all' && value !== '';
    });
  }, [filters]);

  // 活动筛选器数量
  const activeFiltersCount = useMemo(() => {
    const defaultFilters = createInitialFilters();
    return Object.entries(filters).filter(([key, value]) => {
      if (key === 'startDate' || key === 'endDate') {
        return value && value !== '';
      }
      return value && value !== defaultFilters[key] && value !== 'all' && value !== '';
    }).length;
  }, [filters]);

  return {
    filters,
    updateFilter,
    updateFilters,
    resetFilters,
    buildQueryParams,
    hasActiveFilters,
    activeFiltersCount
  };
};

export default useFilters;
