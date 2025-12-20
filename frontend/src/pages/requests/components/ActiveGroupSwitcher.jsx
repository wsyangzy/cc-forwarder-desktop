// ============================================
// ActiveGroupSwitcher - 渠道快捷切换器
// 2025-12-09 17:29:26 v5.0: 显示渠道名称，按优先级排序
// ============================================

import { useState, useEffect, useRef, useMemo } from 'react';
import { ArrowLeftRight, Server, Check, AlertCircle } from 'lucide-react';

/**
 * ActiveGroupSwitcher - 渠道快捷切换器
 * @param {Object} props
 * @param {Array} props.groups - 所有渠道列表（一个渠道=一个组）
 * @param {string} props.activeGroup - 当前活跃渠道名称
 * @param {Function} props.onSwitch - 切换回调 (channelName) => Promise<void>
 * @param {boolean} props.loading - 是否正在切换中
 */
const ActiveGroupSwitcher = ({
  groups = [],
  activeGroup = '',
  onSwitch,
  loading = false
}) => {
  const [isOpen, setIsOpen] = useState(false);
  const [switching, setSwitching] = useState(false);
  const containerRef = useRef(null);

  // 按优先级排序（数值越小优先级越高）- Hook 必须在条件返回之前
  const sortedGroups = useMemo(() => {
    return [...groups].sort((a, b) => (a.priority ?? 999) - (b.priority ?? 999));
  }, [groups]);

  // 点击外部关闭
  useEffect(() => {
    const handleClickOutside = (event) => {
      if (containerRef.current && !containerRef.current.contains(event.target)) {
        setIsOpen(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  // ESC 键关闭
  useEffect(() => {
    const handleKeyDown = (e) => {
      if (e.key === 'Escape') {
        setIsOpen(false);
      }
    };
    if (isOpen) {
      window.addEventListener('keydown', handleKeyDown);
    }
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [isOpen]);

  // 处理渠道选择
  const handleChannelSelect = async (channel) => {
    if (switching) return;

    // 如果选择的是当前活跃渠道，直接关闭
    if (channel.name === activeGroup) {
      setIsOpen(false);
      return;
    }

    setSwitching(true);
    try {
      // 只传递渠道名（组名）
      await onSwitch?.(channel.name, null);
      setIsOpen(false);
    } catch (error) {
      console.error('切换失败:', error);
      alert(`切换失败: ${error.message || '未知错误'}`);
    } finally {
      setSwitching(false);
    }
  };

  // 获取健康状态样式
  const getHealthStyle = (endpoint) => {
    const healthyCount = endpoint.healthy_endpoints ?? 0;
    const totalCount = endpoint.total_endpoints ?? 1;
    const isHealthy = healthyCount > 0;

    if (isHealthy) {
      return {
        dot: 'bg-emerald-400',
        text: '健康',
        color: 'text-emerald-600'
      };
    }
    return {
      dot: 'bg-rose-400',
      text: '异常',
      color: 'text-rose-600'
    };
  };

  // 无端点数据时的占位显示
  if (!groups.length) {
    return (
      <div className="flex items-center gap-2 px-3 py-1.5 rounded-lg text-sm text-gray-400 bg-gray-50 border border-gray-200">
        <AlertCircle className="w-3.5 h-3.5" />
        <span>无可用渠道</span>
      </div>
    );
  }

  // 查找当前活跃渠道
  const activeChannel = sortedGroups.find(g => g.name === activeGroup) || sortedGroups[0];
  const activeHealth = getHealthStyle(activeChannel);

  return (
    <div className="relative" ref={containerRef}>
      {/* 触发按钮 */}
      <button
        onClick={() => setIsOpen(!isOpen)}
        disabled={switching || loading}
        className={`group flex items-center gap-2 px-3 py-1.5 bg-white border rounded-lg text-sm font-medium transition-all shadow-sm ${
          isOpen
            ? 'border-indigo-300 ring-2 ring-indigo-100 text-indigo-700'
            : 'border-gray-200 text-gray-700 hover:border-indigo-300 hover:text-indigo-600'
        } ${(switching || loading) ? 'opacity-60 cursor-wait' : ''}`}
      >
        {/* 活跃状态指示器 */}
        <span className="relative flex h-2 w-2">
          <span className={`animate-ping absolute inline-flex h-full w-full rounded-full ${activeHealth.dot} opacity-75`}></span>
          <span className={`relative inline-flex rounded-full h-2 w-2 ${activeHealth.dot}`}></span>
        </span>

        <div className="flex items-center gap-1.5">
          <Server className="w-3.5 h-3.5 text-gray-400" />
          <span className="font-semibold text-xs text-gray-500">渠道:</span>
          <span className="font-bold">{activeGroup || '未选择'}</span>
        </div>

        <ArrowLeftRight className={`w-3.5 h-3.5 ml-1 text-gray-400 transition-transform ${isOpen ? 'rotate-180' : ''}`} />
      </button>

      {/* 下拉面板 */}
      {isOpen && (
        <div className="absolute top-full left-0 mt-2 w-[280px] bg-white rounded-xl shadow-xl border border-gray-100 ring-1 ring-black/5 z-50 overflow-hidden animate-in fade-in slide-in-from-top-2 duration-200">
          {/* 标题 */}
          <div className="px-4 py-3 bg-gray-50 border-b border-gray-100">
            <div className="text-xs font-semibold text-gray-500 uppercase tracking-wider">
              选择渠道
            </div>
            <div className="text-[10px] text-gray-400 mt-0.5">
              共 {sortedGroups.length} 个渠道
            </div>
          </div>

          {/* 渠道列表 */}
          <div className="p-2 max-h-[320px] overflow-y-auto">
            {sortedGroups.map((channel) => {
              const isActive = channel.name === activeGroup;
              const health = getHealthStyle(channel);

              return (
                <button
                  key={channel.name}
                  onClick={() => handleChannelSelect(channel)}
                  disabled={switching}
                  className={`w-full flex items-center justify-between px-3 py-2.5 rounded-lg text-sm text-left transition-colors ${
                    isActive
                      ? 'bg-indigo-50 text-indigo-700'
                      : 'hover:bg-gray-50 text-gray-700'
                  } ${switching ? 'opacity-50 cursor-wait' : ''}`}
                >
                  <div className="flex items-center gap-3">
                    {/* 健康状态指示点 */}
                    <span className={`w-2 h-2 rounded-full ${health.dot}`} />

                    <div className="flex flex-col">
                      {/* 渠道名称 */}
                      <div className="flex items-center gap-2">
                        <span className={`font-medium ${isActive ? 'text-indigo-700' : 'text-gray-800'}`}>
                          {channel.name}
                        </span>
                      </div>
                      {/* 健康状态 */}
                      <span className={`text-[10px] ${health.color}`}>
                        {health.text}
                        {channel.in_cooldown && ' · 冷却中'}
                      </span>
                    </div>
                  </div>

                  {/* 选中状态 */}
                  <div className="flex items-center gap-2">
                    {channel.paused && (
                      <span className="text-[10px] px-1.5 py-0.5 rounded bg-amber-100 text-amber-600">
                        已暂停
                      </span>
                    )}
                    {isActive && <Check className="w-4 h-4 text-indigo-600" />}
                  </div>
                </button>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
};

export default ActiveGroupSwitcher;
