package app

import (
	"context"
	"fmt"
	"sync"
)

// lifecycleTasks closes admission before canceling its asynchronous work.
// StopContext can return while work that ignores cancellation is still running.
type lifecycleTasks struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	active int
	done   chan struct{}
}

func newLifecycleTasks(parent context.Context) *lifecycleTasks {
	ctx, cancel := context.WithCancel(parent)
	return &lifecycleTasks{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (g *lifecycleTasks) Start(work func(context.Context)) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return false
	}
	g.active++
	ctx := g.ctx
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			g.active--
			if g.closed && g.active == 0 {
				close(g.done)
			}
			g.mu.Unlock()
		}()
		work(ctx)
	}()
	return true
}

func (g *lifecycleTasks) Stop() {
	_ = g.StopContext(context.Background())
}

func (g *lifecycleTasks) cancelWork() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.cancel()
		if g.active == 0 {
			close(g.done)
		}
	}
	g.mu.Unlock()
}

func (g *lifecycleTasks) StopContext(ctx context.Context) error {
	g.cancelWork()
	select {
	case <-g.done:
		return nil
	default:
	}
	select {
	case <-g.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("unfinished lifecycle work: %w", ctx.Err())
	}
}
