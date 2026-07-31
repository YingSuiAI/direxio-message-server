package process

import (
	"sync/atomic"
	"testing"
)

func TestShutdownCallbacksPanicIsolationAndExactlyOnce(t *testing.T) {
	ctx := NewProcessContext()
	ctx.ComponentStarted()
	var first, second atomic.Int32
	ctx.RegisterShutdownCallback(func() {
		first.Add(1)
		panic("test panic")
	})
	ctx.RegisterShutdownCallback(func() { second.Add(1) })
	ctx.ShutdownDendrite()
	ctx.ComponentFinished()
	ctx.WaitForComponentsToFinish()
	ctx.WaitForComponentsToFinish()
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("callbacks ran %d/%d times", first.Load(), second.Load())
	}
	var late atomic.Int32
	ctx.RegisterShutdownCallback(func() { late.Add(1) })
	if late.Load() != 1 {
		t.Fatalf("late callback count = %d, want 1", late.Load())
	}
}

func TestShutdownCallbackRegistrationRacesFinalization(t *testing.T) {
	ctx := NewProcessContext()
	ctx.ShutdownDendrite()
	const count = 64
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		for i := 0; i < count; i++ {
			ctx.RegisterShutdownCallback(func() { calls.Add(1) })
		}
		close(done)
	}()
	ctx.WaitForComponentsToFinish()
	<-done
	if calls.Load() != count {
		t.Fatalf("racing callbacks = %d, want %d", calls.Load(), count)
	}
}
