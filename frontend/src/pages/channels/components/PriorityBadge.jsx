// ============================================
// PriorityBadge - 优先级徽章
// ============================================

const getPriorityTone = (priority) => {
  if (priority <= 1) return 'bg-emerald-50 text-emerald-700 border-emerald-200';
  if (priority <= 3) return 'bg-indigo-50 text-indigo-700 border-indigo-200';
  if (priority <= 10) return 'bg-slate-50 text-slate-700 border-slate-200';
  return 'bg-slate-50 text-slate-500 border-slate-200';
};

const PriorityBadge = ({ priority = 1, size = 'sm', className = '' }) => {
  const numeric = Number(priority);
  const value = Number.isFinite(numeric) ? Math.max(1, Math.floor(numeric)) : 1;

  const sizeClass = size === 'md'
    ? 'min-w-10 h-7 px-2.5 text-[11px]'
    : 'min-w-9 h-6 px-2 text-[11px]';

  return (
    <span
      className={`inline-flex items-center justify-center rounded-full border font-semibold whitespace-nowrap leading-none flex-shrink-0 ${sizeClass} ${getPriorityTone(value)} ${className}`.trim()}
      title={`优先级 P${value}（越小越优先）`}
    >
      P{value}
    </span>
  );
};

export default PriorityBadge;
