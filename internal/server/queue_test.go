package server

import (
	"context"
	"testing"
	"time"
)

func TestAcquireRequestSlot_Immediate(t *testing.T) {
	// Drain any residual tokens
drainLoop:
	for {
		select {
		case <-requestSem:
		default:
			break drainLoop
		}
	}

	ctx := context.Background()
	release, ok := acquireRequestSlot(ctx, 1*time.Second)
	if !ok {
		t.Fatalf("expected immediate slot acquisition, got false")
	}
	if release == nil {
		t.Fatalf("expected non-nil release function")
	}
	release()
}

func TestAcquireRequestSlot_QueueTimeout(t *testing.T) {
	// Fill all slots
	var releases []func()
	for i := 0; i < maxConcurrentRequests; i++ {
		rel, ok := acquireRequestSlot(context.Background(), 100*time.Millisecond)
		if !ok {
			t.Fatalf("failed to fill slot %d", i)
		}
		releases = append(releases, rel)
	}
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()

	// Try to acquire with a short 200ms timeout when all slots are full
	start := time.Now()
	ctx := context.Background()
	_, ok := acquireRequestSlot(ctx, 200*time.Millisecond)
	elapsed := time.Since(start)

	if ok {
		t.Errorf("expected slot acquisition to fail/timeout when queue is full, got true")
	}
	if elapsed < 180*time.Millisecond {
		t.Errorf("expected to wait at least ~200ms in queue, only waited %v", elapsed)
	}
}

func TestAcquireRequestSlot_QueueAcquiresWhenSlotFreed(t *testing.T) {
	// Fill all slots
	var releases []func()
	for i := 0; i < maxConcurrentRequests; i++ {
		rel, ok := acquireRequestSlot(context.Background(), 100*time.Millisecond)
		if !ok {
			t.Fatalf("failed to fill slot %d", i)
		}
		releases = append(releases, rel)
	}
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()

	// In background, free one slot after 150ms
	go func() {
		time.Sleep(150 * time.Millisecond)
		if len(releases) > 0 {
			releases[0]()
			releases = releases[1:]
		}
	}()

	start := time.Now()
	ctx := context.Background()
	rel, ok := acquireRequestSlot(ctx, 1*time.Second)
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("expected slot acquisition to succeed after slot freed, got false")
	}
	defer rel()

	if elapsed < 100*time.Millisecond {
		t.Errorf("expected to wait at least 100ms for slot to free, waited %v", elapsed)
	}
}

func TestAcquireRequestSlot_ContextCancel(t *testing.T) {
	// Fill all slots
	var releases []func()
	for i := 0; i < maxConcurrentRequests; i++ {
		rel, ok := acquireRequestSlot(context.Background(), 100*time.Millisecond)
		if !ok {
			t.Fatalf("failed to fill slot %d", i)
		}
		releases = append(releases, rel)
	}
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, ok := acquireRequestSlot(ctx, 2*time.Second)
	elapsed := time.Since(start)

	if ok {
		t.Errorf("expected false on context cancel, got true")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected fast exit on context cancel, took %v", elapsed)
	}
}
