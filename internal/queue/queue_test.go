package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSubmitAndHandle(t *testing.T) {
	var handled atomic.Int32
	q := New(4, 1, func(ctx context.Context, j Job) error {
		handled.Add(1)
		j.Reply("done: " + j.VideoID)
		return nil
	}, testLogger())

	var got string
	var mu sync.Mutex
	err := q.Submit(Job{
		VideoID: "abc123",
		Reply:   func(m string) { mu.Lock(); got = m; mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	q.Close(2 * time.Second)
	if handled.Load() != 1 {
		t.Errorf("handled = %d, want 1", handled.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "done: abc123" {
		t.Errorf("reply = %q, want %q", got, "done: abc123")
	}
}

func TestSubmitRefusesWhenFull(t *testing.T) {
	release := make(chan struct{})
	var started sync.Once
	block := make(chan struct{})

	q := New(2, 1, func(ctx context.Context, j Job) error {
		started.Do(func() { close(block) })
		<-release
		return nil
	}, testLogger())
	defer func() { close(release); q.Close(2 * time.Second) }()

	send := func(id string) error {
		return q.Submit(Job{VideoID: id, Reply: func(string) {}})
	}

	// Occupy the single worker.
	if err := send("running"); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	<-block

	// Depth is 2 and the worker is busy, so exactly two more fit before the channel is full.
	if err := send("queued-1"); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if err := send("queued-2"); err != nil {
		t.Fatalf("third Submit: %v", err)
	}
	if err := send("queued-3"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("fourth Submit err = %v, want ErrQueueFull", err)
	}
	if err := send("queued-4"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("fifth Submit err = %v, want ErrQueueFull", err)
	}
}

func TestSubmitDoesNotBlock(t *testing.T) {
	// The Discord interaction handler must return inside 3s, so Submit can never block on a
	// full queue. This is the test for that contract.
	release := make(chan struct{})
	block := make(chan struct{})
	var once sync.Once

	q := New(1, 1, func(ctx context.Context, j Job) error {
		once.Do(func() { close(block) })
		<-release
		return nil
	}, testLogger())
	defer func() { close(release); q.Close(2 * time.Second) }()

	q.Submit(Job{VideoID: "a", Reply: func(string) {}})
	<-block

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			q.Submit(Job{VideoID: "b", Reply: func(string) {}})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked on a full queue")
	}
}

func TestFailedJobReportsError(t *testing.T) {
	q := New(2, 1, func(ctx context.Context, j Job) error {
		return errors.New("extraction failed")
	}, testLogger())

	var got string
	var mu sync.Mutex
	q.Submit(Job{
		VideoID: "abc",
		Reply:   func(m string) { mu.Lock(); got = m; mu.Unlock() },
	})
	q.Close(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if got == "" {
		t.Fatal("no failure message sent to the channel")
	}
	if want := "extraction failed"; !contains(got, want) {
		t.Errorf("reply %q does not mention %q", got, want)
	}
}

func TestSuccessIsReportedByHandler(t *testing.T) {
	// The queue must not double-report: a successful job's message comes from the handler,
	// which is the only thing that knows the summary.
	q := New(2, 1, func(ctx context.Context, j Job) error {
		j.Reply("summary text")
		return nil
	}, testLogger())

	var count int
	var mu sync.Mutex
	q.Submit(Job{VideoID: "abc", Reply: func(string) {
		mu.Lock()
		count++
		mu.Unlock()
	}})
	q.Close(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Errorf("reply called %d times, want exactly 1", count)
	}
}

func TestNilReplyIsNotPanicked(t *testing.T) {
	// A job with no reply func is a wiring bug, not a crash.
	q := New(2, 1, func(ctx context.Context, j Job) error {
		j.Reply("ok")
		return nil
	}, testLogger())
	defer q.Close(time.Second)

	// Will return ErrQueueFull or succeed; either way, no panic.
	q.Submit(Job{VideoID: "abc"})
}

func TestMultipleJobsRunConcurrently(t *testing.T) {
	var concurrent atomic.Int32
	var peak atomic.Int32
	var done atomic.Int32

	q := New(10, 2, func(ctx context.Context, j Job) error {
		n := concurrent.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		concurrent.Add(-1)
		done.Add(1)
		return nil
	}, testLogger())

	for i := 0; i < 5; i++ {
		if err := q.Submit(Job{VideoID: string(rune('a' + i)), Reply: func(string) {}}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	q.Close(5 * time.Second)

	if done.Load() != 5 {
		t.Errorf("completed = %d, want 5", done.Load())
	}
	if peak.Load() > 2 {
		t.Errorf("peak concurrency = %d, want <= 2 (worker count)", peak.Load())
	}
}

func TestPendingAndStartedAt(t *testing.T) {
	release := make(chan struct{})
	block := make(chan struct{})
	var once sync.Once

	before := time.Now()
	q := New(5, 1, func(ctx context.Context, j Job) error {
		once.Do(func() { close(block) })
		<-release
		return nil
	}, testLogger())

	if q.StartedAt().Before(before.Add(-time.Second)) {
		t.Error("StartedAt is implausibly old")
	}
	q.Submit(Job{VideoID: "a", Reply: func(string) {}})
	<-block
	q.Submit(Job{VideoID: "b", Reply: func(string) {}})

	if got := q.Pending(); got != 1 {
		t.Errorf("Pending = %d, want 1", got)
	}
	close(release)
	q.Close(2 * time.Second)
	if got := q.Pending(); got != 0 {
		t.Errorf("Pending after drain = %d, want 0", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	q := New(2, 1, func(ctx context.Context, j Job) error { return nil }, testLogger())
	q.Close(time.Second)
	q.Close(time.Second) // must not panic on a double close
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
