package process

import (
	"context"
	"sync"

	"github.com/getsentry/sentry-go"
	"github.com/sirupsen/logrus"
)

type ProcessContext struct {
	mu                    sync.RWMutex
	wg                    sync.WaitGroup      // used to wait for components to shutdown
	ctx                   context.Context     // cancelled when Stop is called
	shutdown              context.CancelFunc  // shut down Dendrite
	degraded              map[string]struct{} // reasons why the process is degraded
	shutdownCallbacksMu   sync.Mutex
	shutdownCallbacks     []func()
	shutdownCallbacksOnce sync.Once
}

func NewProcessContext() *ProcessContext {
	ctx, shutdown := context.WithCancel(context.Background())
	return &ProcessContext{
		ctx:      ctx,
		shutdown: shutdown,
		wg:       sync.WaitGroup{},
		degraded: make(map[string]struct{}),
	}
}

func (b *ProcessContext) Context() context.Context {
	return context.WithValue(b.ctx, "scope", "process") // nolint:staticcheck
}

func (b *ProcessContext) ComponentStarted() {
	b.wg.Add(1)
}

func (b *ProcessContext) ComponentFinished() {
	b.wg.Done()
}

func (b *ProcessContext) ShutdownDendrite() {
	b.shutdown()
}

func (b *ProcessContext) WaitForShutdown() <-chan struct{} {
	return b.ctx.Done()
}

func (b *ProcessContext) WaitForComponentsToFinish() {
	b.wg.Wait()
	if b.ctx.Err() != nil {
		b.shutdownCallbacksOnce.Do(func() {
			b.shutdownCallbacksMu.Lock()
			callbacks := append([]func(){}, b.shutdownCallbacks...)
			b.shutdownCallbacks = nil
			b.shutdownCallbacksMu.Unlock()
			for _, callback := range callbacks {
				callback()
			}
		})
	}
}

// RegisterShutdownCallback runs once after process cancellation and all
// registered components have stopped. It is the final shutdown phase for
// resources that must remain fenced while HTTP and workers drain.
func (b *ProcessContext) RegisterShutdownCallback(callback func()) {
	if b == nil || callback == nil {
		return
	}
	b.shutdownCallbacksMu.Lock()
	b.shutdownCallbacks = append(b.shutdownCallbacks, callback)
	b.shutdownCallbacksMu.Unlock()
}

func (b *ProcessContext) Degraded(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.degraded[err.Error()]; !ok {
		logrus.WithError(err).Warn("Dendrite has entered a degraded state")
		sentry.CaptureException(err)
		b.degraded[err.Error()] = struct{}{}
	}
}

func (b *ProcessContext) IsDegraded() (bool, []string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.degraded) == 0 {
		return false, nil
	}
	reasons := make([]string, 0, len(b.degraded))
	for reason := range b.degraded {
		reasons = append(reasons, reason)
	}
	return true, reasons
}
