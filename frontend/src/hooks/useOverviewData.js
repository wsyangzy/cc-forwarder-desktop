// ============================================
// Overview 页面数据 Hook
// 2025-11-28
// ============================================

import { useState, useCallback, useEffect, useRef } from 'react';
import { fetchStatus, fetchConnections, fetchEndpoints, fetchGroups, formatUptime } from '@utils/api.js';
import useSSE from './useSSE.js';

const useOverviewData = () => {
  const [data, setData] = useState({
    status: { status: 'running', uptime: '加载中...' },
    endpoints: { total: 0, healthy: 0, endpoints: [] },
    connections: {
      total_requests: 0,
      active_connections: 0,
      successful_requests: 0,
      failed_requests: 0,
      average_response_time: '0s',
      total_tokens: 0,
      today_cost: 0,              // 今日成本
      all_time_total_cost: 0,     // 全部历史成本
      today_tokens: 0,            // 今日 tokens
      all_time_total_tokens: 0,   // 全部历史 tokens
      today_requests: 0           // 今日请求数
    },
    groups: {
      active_group: null,
      groups: [],
      total_suspended_requests: 0
    },
    lastUpdate: null,
    loading: true,
    error: null
  });

  const [isInitialized, setIsInitialized] = useState(false);
  const [startTimestamp, setStartTimestamp] = useState(null);

  // 用于触发重新加载的标志
  const reloadTriggerRef = useRef(0);
  const [reloadTrigger, setReloadTrigger] = useState(0);

  // 实时计算运行时间
  const calculateCurrentUptime = useCallback(() => {
    if (!startTimestamp) return '加载中...';
    const currentTime = Math.floor(Date.now() / 1000);
    const uptimeSeconds = currentTime - startTimestamp;
    return formatUptime(uptimeSeconds);
  }, [startTimestamp]);

  // SSE 数据更新处理
  const handleSSEUpdate = useCallback((sseData, eventType) => {
    const { data: actualData = sseData } = sseData;
    const { change_type: changeType } = actualData;

    console.log(`📡 [SSE] 收到${eventType || 'generic'}事件, 变更类型: ${changeType || 'none'}`, sseData);

    // 保存启动时间戳
    if (sseData.start_timestamp) {
      setStartTimestamp(sseData.start_timestamp);
    }

    // 检查是否是组切换事件 - 需要重新加载数据
    const groupEvent = actualData?.event || sseData?.data?.event;
    if (eventType === 'group' && (
      groupEvent === 'group_manually_activated' ||
      groupEvent === 'group_activated' ||
      groupEvent === 'group_paused' ||
      groupEvent === 'group_switched'
    )) {
      console.log('🔄 [SSE] 检测到组切换事件，触发重新加载');
      reloadTriggerRef.current += 1;
      setReloadTrigger(reloadTriggerRef.current);
      return;
    }

    // 处理图表更新事件 - 分发给 document 供图表组件监听
    if (eventType === 'chart') {
      const chartEvent = new CustomEvent('chartUpdate', {
        detail: {
          chart_type: sseData.chart_type || actualData.chart_type,
          data: sseData.data || actualData
        }
      });
      document.dispatchEvent(chartEvent);
      console.log('📊 [SSE] 分发图表更新事件:', sseData.chart_type || actualData.chart_type);
      return;
    }

    setData(prevData => {
      const newData = { ...prevData };

      // 处理系统统计事件
      if (eventType === 'status' || changeType === 'system_stats_updated') {
        const systemUpdates = {};
        ['memory_usage', 'goroutine_count'].forEach(field => {
          if (sseData[field] !== undefined) {
            systemUpdates[field] = sseData[field];
          }
        });
        // uptime 由本地定时器计算，不从 SSE 更新（避免跳变）
        if (sseData.status) {
          const statusData = { ...sseData.status };
          // 删除 uptime，避免覆盖本地计算的值
          delete statusData.uptime;
          Object.assign(systemUpdates, statusData);
        }
        if (Object.keys(systemUpdates).length > 0) {
          newData.status = { ...newData.status, ...systemUpdates };
        }
      }

      // 处理连接统计事件
      if (eventType === 'connection' || changeType === 'connection_stats_updated') {
        const connectionFields = [
          'total_requests', 'all_time_total_requests', 'active_connections', 'successful_requests',
          'failed_requests', 'average_response_time', 'total_tokens'
        ];
        const connectionUpdates = {};
        connectionFields.forEach(field => {
          if (actualData[field] !== undefined) {
            connectionUpdates[field] = actualData[field];
          }
        });
        if (actualData.connections) {
          Object.assign(connectionUpdates, actualData.connections);
        }
        if (Object.keys(connectionUpdates).length > 0) {
          newData.connections = { ...newData.connections, ...connectionUpdates };
        }
      }

      // 处理端点事件 - 需要计算 total 和 healthy
      if (eventType === 'endpoint' || sseData.endpoints) {
        console.log('🎯 [SSE] 处理端点事件');
        const endpointData = sseData.endpoints || sseData;
        // 如果推送的是数组，需要转换为我们的格式
        if (Array.isArray(endpointData)) {
          newData.endpoints = {
            ...newData.endpoints,
            endpoints: endpointData,
            total: endpointData.length,
            healthy: endpointData.filter(e => e.status === 'healthy').length
          };
        } else if (endpointData.endpoints && Array.isArray(endpointData.endpoints)) {
          // 如果是包含 endpoints 数组的对象
          newData.endpoints = {
            ...newData.endpoints,
            ...endpointData,
            total: endpointData.total ?? endpointData.endpoints.length,
            healthy: endpointData.healthy ?? endpointData.endpoints.filter(e => e.status === 'healthy').length
          };
        } else {
          // 如果是对象格式，直接合并
          newData.endpoints = { ...newData.endpoints, ...endpointData };
        }
      }

      // 处理组事件 - 需要计算 active_group
      if (eventType === 'group' || sseData.groups) {
        console.log('👥 [SSE] 处理组事件');
        const groupData = sseData.groups || sseData;
        // 如果推送的是数组，需要转换为我们的格式
        if (Array.isArray(groupData)) {
          const activeGroup = groupData.find(g => g.is_active);
          newData.groups = {
            ...newData.groups,
            groups: groupData,
            active_group: activeGroup?.name || null
          };
        } else if (groupData.groups && Array.isArray(groupData.groups)) {
          // 如果是包含 groups 数组的对象
          const activeGroup = groupData.groups.find(g => g.is_active);
          newData.groups = {
            ...newData.groups,
            ...groupData,
            active_group: activeGroup?.name || groupData.active_group || null
          };
        } else {
          // 直接合并
          newData.groups = { ...newData.groups, ...groupData };
        }
      }

      newData.lastUpdate = new Date().toLocaleTimeString();
      return newData;
    });
  }, []);

  // 初始化 SSE 连接
  const { connectionStatus, isConnected } = useSSE(handleSSEUpdate);

  // 加载初始数据
  const loadData = useCallback(async () => {
    try {
      if (!isInitialized) {
        setData(prev => ({ ...prev, loading: true, error: null }));
      }

      const [status, endpoints, connections, groups] = await Promise.all([
        fetchStatus(),
        fetchEndpoints(),
        fetchConnections(),
        fetchGroups()
      ]);

      // 格式化 uptime - 只在还没有 startTimestamp 时使用 API 返回的值
      const formattedStatus = { ...status };

      // 解析启动时间戳（只在首次加载时）
      if (!startTimestamp && status.start_time) {
        try {
          const startDate = new Date(status.start_time);
          const parsedTimestamp = Math.floor(startDate.getTime() / 1000);
          setStartTimestamp(parsedTimestamp);
          console.log('⏰ [数据加载] 解析启动时间戳:', parsedTimestamp);
        } catch (e) {
          console.warn('解析启动时间失败:', e);
        }
      }

      // 如果已经有 startTimestamp，不要用 API 的 uptime 覆盖本地计算的值
      // 这样可以避免组切换时运行时间跳变
      if (startTimestamp) {
        delete formattedStatus.uptime;
      } else if (formattedStatus.uptime !== undefined) {
        formattedStatus.uptime = formatUptime(formattedStatus.uptime);
      }

      // 数据已经由 API 层处理好格式，直接使用
      setData(prevData => ({
        status: { ...prevData.status, ...formattedStatus },
        endpoints: { ...prevData.endpoints, ...endpoints },
        connections: { ...prevData.connections, ...connections },
        groups: { ...prevData.groups, ...groups },
        lastUpdate: new Date().toLocaleTimeString(),
        loading: false,
        error: null
      }));

      setIsInitialized(true);
    } catch (error) {
      console.error('数据加载失败:', error);
      setData(prev => ({
        ...prev,
        loading: false,
        error: error.message || '数据加载失败'
      }));
    }
  }, [isInitialized, startTimestamp]);

  // 初始加载
  useEffect(() => {
    loadData();
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // 组切换事件触发重新加载
  useEffect(() => {
    if (reloadTrigger > 0) {
      console.log('🔄 [SSE] 执行组切换后的数据重新加载');
      loadData();
    }
  }, [reloadTrigger, loadData]);

  // SSE 失败时启用定时刷新
  useEffect(() => {
    let interval = null;
    if (connectionStatus === 'failed' || connectionStatus === 'error') {
      interval = setInterval(loadData, 10000);
    }
    return () => {
      if (interval) clearInterval(interval);
    };
  }, [connectionStatus, loadData]);

  // 实时更新运行时间
  useEffect(() => {
    if (!startTimestamp) return;

    const timer = setInterval(() => {
      setData(prevData => ({
        ...prevData,
        status: {
          ...prevData.status,
          uptime: calculateCurrentUptime()
        }
      }));
    }, 1000);

    return () => clearInterval(timer);
  }, [startTimestamp, calculateCurrentUptime]);

  return {
    data,
    loadData,
    refresh: loadData,
    isInitialized,
    connectionStatus,
    isConnected
  };
};

export default useOverviewData;
