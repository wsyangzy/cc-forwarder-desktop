// ============================================
// Endpoints 页面工具函数
// ============================================

/**
 * 按渠道分组端点
 * @param {Array} endpoints - 端点列表
 * @returns {Array} 分组后的端点列表 [{ channel, endpoints }, ...]
 */
export const groupEndpointsByChannel = (endpoints) => {
  const groups = {};
  endpoints.forEach(ep => {
    const channel = ep.channel || ep.group || '未分组';
    if (!groups[channel]) groups[channel] = [];
    groups[channel].push(ep);
  });

  // 先对每组内的端点按优先级排序
  Object.keys(groups).forEach(channel => {
    groups[channel].sort((a, b) => (a.priority || 99) - (b.priority || 99));
  });

  // 按渠道内最高优先级（最小数字）排序
  return Object.entries(groups)
    .sort(([, epsA], [, epsB]) => {
      const minPriorityA = Math.min(...epsA.map(e => e.priority || 99));
      const minPriorityB = Math.min(...epsB.map(e => e.priority || 99));
      return minPriorityA - minPriorityB;
    })
    .map(([channel, eps]) => ({
      channel,
      endpoints: eps
    }));
};
