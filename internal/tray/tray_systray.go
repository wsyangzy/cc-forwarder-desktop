// +build !stub

package tray

import (
	"context"
	"sync"

	"github.com/getlantern/systray"
)

type systrayController struct {
	opts      Options
	ctx       context.Context
	quitCh    chan struct{}
	once      sync.Once
	running   bool
	runningMu sync.Mutex
}

func (c *systrayController) Stop() {
	c.once.Do(func() {
		c.runningMu.Lock()
		if c.running {
			systray.Quit()
			c.running = false
		}
		c.runningMu.Unlock()
		close(c.quitCh)
	})
}

func start(ctx context.Context, opts Options) (Controller, error) {
	ctrl := &systrayController{
		opts:   opts,
		ctx:    ctx,
		quitCh: make(chan struct{}),
	}

	// systray.Run 会阻塞，在单独的 goroutine 中运行
	go func() {
		ctrl.runningMu.Lock()
		ctrl.running = true
		ctrl.runningMu.Unlock()

		systray.Run(
			func() { ctrl.onReady() },
			func() { ctrl.onExit() },
		)
	}()

	return ctrl, nil
}

func (c *systrayController) onReady() {
	// 设置图标
	if len(c.opts.Icon) > 0 {
		systray.SetIcon(c.opts.Icon)
	}

	// 设置 tooltip
	if c.opts.Tooltip != "" {
		systray.SetTooltip(c.opts.Tooltip)
	} else {
		systray.SetTooltip("CC-Forwarder")
	}

	// 创建菜单（只保留显示和退出，隐藏交给窗口 X 按钮）
	mShow := systray.AddMenuItem("显示主窗口", "显示应用主窗口")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出应用")

	// 监听菜单点击
	go func() {
		for {
			select {
			case <-c.quitCh:
				return
			case <-mShow.ClickedCh:
				if c.opts.OnShow != nil {
					c.opts.OnShow()
				}
			case <-mQuit.ClickedCh:
				if c.opts.OnQuit != nil {
					c.opts.OnQuit()
				}
			}
		}
	}()
}

func (c *systrayController) onExit() {
	// 清理
}
