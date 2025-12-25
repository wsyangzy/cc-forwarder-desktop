package tray

import "context"

// Controller 表示托盘控制器（用于停止托盘）。
type Controller interface {
	Stop()
}

// Options 托盘启动参数。
type Options struct {
	// Icon 托盘图标内容（Windows 推荐 .ico 字节；其它平台可忽略）。
	Icon []byte

	// Tooltip 托盘悬浮提示文本。
	Tooltip string

	// OnShow 用户希望显示主窗口时触发（如双击托盘图标/点击“显示”菜单）。
	OnShow func()

	// OnHide 用户希望隐藏主窗口时触发（如点击“隐藏”菜单）。
	OnHide func()

	// OnQuit 用户选择“退出”时触发。
	OnQuit func()
}

// Start 启动系统托盘（平台相关实现）。
func Start(ctx context.Context, opts Options) (Controller, error) {
	return start(ctx, opts)
}
