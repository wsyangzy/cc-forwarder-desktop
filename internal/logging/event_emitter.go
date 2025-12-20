package logging

import (
	"context"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// EventEmitter Wails 事件发射器
// 负责将日志通过 Wails Runtime Events 推送到前端
type EventEmitter struct {
	mu sync.Mutex

	ctx     context.Context
	enabled bool

	batchSize     int
	flushInterval time.Duration

	queue    chan LogEntry
	stopChan chan struct{}
	doneChan chan struct{}
}

// NewEventEmitter 创建事件发射器
func NewEventEmitter() *EventEmitter {
	return &EventEmitter{
		batchSize:     10,                     // 每批最多10条
		flushInterval: 100 * time.Millisecond, // 100ms刷新一次
	}
}

// Start 启动事件发射器（前端订阅后调用）
func (e *EventEmitter) Start(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.enabled {
		return // 已启动
	}

	e.ctx = ctx
	e.enabled = true
	e.stopChan = make(chan struct{})
	e.doneChan = make(chan struct{})

	// 有界队列：避免前端消费慢时拖住日志主路径（满了就丢弃）
	queueCap := e.batchSize * 200 // 默认约2000条
	if queueCap < 100 {
		queueCap = 100
	}
	e.queue = make(chan LogEntry, queueCap)

	// 启动批量发送 goroutine（EventsEmit 在锁外调用）
	go e.batchSendLoop(e.ctx, e.queue, e.stopChan, e.doneChan, e.batchSize, e.flushInterval)
}

// Stop 停止事件发射器
func (e *EventEmitter) Stop() {
	e.mu.Lock()
	if !e.enabled {
		e.mu.Unlock()
		return
	}
	e.enabled = false
	stopChan := e.stopChan
	doneChan := e.doneChan
	e.stopChan = nil
	e.doneChan = nil
	e.queue = nil
	e.mu.Unlock()

	if stopChan != nil {
		close(stopChan)
	}
	if doneChan != nil {
		<-doneChan
	}
}

// Emit 发射一条日志事件
func (e *EventEmitter) Emit(entry LogEntry) {
	e.mu.Lock()
	if !e.enabled || e.queue == nil {
		e.mu.Unlock()
		return
	}
	queue := e.queue
	e.mu.Unlock()

	// 不阻塞调用方：队列满时丢弃（避免日志广播反向拖慢核心业务）
	select {
	case queue <- entry:
	default:
		// 尽量保留高优先级日志
		if entry.Level == "ERROR" || entry.Level == "WARN" {
			select {
			case <-queue:
			default:
			}
			select {
			case queue <- entry:
			default:
			}
		}
	}
}

func (e *EventEmitter) batchSendLoop(
	ctx context.Context,
	queue <-chan LogEntry,
	stop <-chan struct{},
	done chan<- struct{},
	batchSize int,
	flushInterval time.Duration,
) {
	defer close(done)

	if batchSize <= 0 {
		batchSize = 10
	}
	if flushInterval <= 0 {
		flushInterval = 100 * time.Millisecond
	}

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	buffer := make([]LogEntry, 0, batchSize)
	flush := func() {
		if len(buffer) == 0 {
			return
		}
		if ctx != nil {
			runtime.EventsEmit(ctx, "log:batch", buffer)
		}
		buffer = buffer[:0]
	}

	for {
		select {
		case <-stop:
			// 尽量把剩余消息刷掉（不阻塞）
			for {
				select {
				case entry := <-queue:
					buffer = append(buffer, entry)
					if len(buffer) >= batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case entry := <-queue:
			buffer = append(buffer, entry)
			if len(buffer) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// IsEnabled 返回是否已启用
func (e *EventEmitter) IsEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enabled
}
