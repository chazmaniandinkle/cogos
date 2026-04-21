// eventbus_test.go — unit tests for the in-process event broker and ring.
//
// These tests are broker-local (no disk, no HTTP). Wiring tests that go
// through AppendEvent live alongside to exercise the ledger → broker path.
package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── Broker unit tests ──────────────────────────────────────────────────────

func newBrokerForTest(t *testing.T, ring, chanBuf int) *EventBroker {
	t.Helper()
	b := NewEventBroker(EventBrokerOptions{
		RingSize:       ring,
		MaxSubscribers: 100,
		ChanBuffer:     chanBuf,
	})
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func mkEnv(sid, typ, src string, t time.Time) *EventEnvelope {
	return &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      typ,
			Timestamp: t.UTC().Format(time.RFC3339),
			SessionID: sid,
			Data:      map[string]interface{}{"i": typ},
		},
		Metadata: EventMetadata{
			Source: src,
			Hash:   fmt.Sprintf("%s-%s-%d", sid, typ, t.UnixNano()),
			Seq:    1,
		},
	}
}

func TestBrokerPublishFanOut(t *testing.T) {
	t.Parallel()
	b := newBrokerForTest(t, 1024, 16)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var subs [3]*Subscription
	for i := range subs {
		s, err := b.Subscribe(ctx, EventFilter{})
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		subs[i] = s
	}

	now := time.Now()
	for i := 0; i < 5; i++ {
		b.Publish(mkEnv("s", fmt.Sprintf("evt.%d", i), "kernel-v3", now.Add(time.Duration(i)*time.Millisecond)))
	}

	for i, s := range subs {
		for j := 0; j < 5; j++ {
			select {
			case env := <-s.Events:
				want := fmt.Sprintf("evt.%d", j)
				if env.HashedPayload.Type != want {
					t.Errorf("sub[%d] evt[%d]: got %s want %s", i, j, env.HashedPayload.Type, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("sub[%d] evt[%d]: timed out", i, j)
			}
		}
	}
}

func TestBrokerFilterByType(t *testing.T) {
	t.Parallel()
	b := newBrokerForTest(t, 1024, 16)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s, err := b.Subscribe(ctx, EventFilter{EventTypePattern: "attention.*"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	now := time.Now()
	b.Publish(mkEnv("s", "attention.boost", "kernel-v3", now))
	b.Publish(mkEnv("s", "cycle.tick", "kernel-v3", now))
	b.Publish(mkEnv("s", "attention.decay", "kernel-v3", now))

	got := []string{}
	deadline := time.After(500 * time.Millisecond)
collect:
	for len(got) < 2 {
		select {
		case env := <-s.Events:
			got = append(got, env.HashedPayload.Type)
		case <-deadline:
			break collect
		}
	}

	if len(got) != 2 || got[0] != "attention.boost" || got[1] != "attention.decay" {
		t.Errorf("got %v; want [attention.boost attention.decay]", got)
	}
}

func TestBrokerFilterBySession(t *testing.T) {
	t.Parallel()
	b := newBrokerForTest(t, 1024, 16)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s, err := b.Subscribe(ctx, EventFilter{SessionID: "A"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	now := time.Now()
	b.Publish(mkEnv("A", "x", "kernel-v3", now))
	b.Publish(mkEnv("B", "x", "kernel-v3", now))
	b.Publish(mkEnv("A", "y", "kernel-v3", now))

	// Expect 2 events (A/x, A/y), then timeout before any B.
	got := 0
	for i := 0; i < 2; i++ {
		select {
		case env := <-s.Events:
			if env.HashedPayload.SessionID != "A" {
				t.Errorf("sub got session %s; want A", env.HashedPayload.SessionID)
			}
			got++
		case <-time.After(500 * time.Millisecond):
			t.Fatal("timed out waiting for session=A event")
		}
	}
	// Drain any stragglers — expect none.
	select {
	case env := <-s.Events:
		t.Errorf("unexpected extra event: session=%s type=%s",
			env.HashedPayload.SessionID, env.HashedPayload.Type)
	case <-time.After(100 * time.Millisecond):
	}
	if got != 2 {
		t.Errorf("delivered %d; want 2", got)
	}
}

func TestBrokerFilterSince(t *testing.T) {
	t.Parallel()
	b := newBrokerForTest(t, 1024, 16)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// since=now+1s — nothing published at `now` should be delivered.
	future := time.Now().Add(1 * time.Second)
	s, err := b.Subscribe(ctx, EventFilter{Since: future})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	now := time.Now()
	b.Publish(mkEnv("s", "early", "kernel-v3", now))
	b.Publish(mkEnv("s", "late", "kernel-v3", future.Add(2*time.Second)))

	select {
	case env := <-s.Events:
		if env.HashedPayload.Type != "late" {
			t.Errorf("first event type=%s; want late", env.HashedPayload.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected 'late' event")
	}

	select {
	case env := <-s.Events:
		t.Errorf("unexpected 'early' delivery: %s", env.HashedPayload.Type)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBrokerReplayFromRing(t *testing.T) {
	t.Parallel()
	b := newBrokerForTest(t, 5, 16)

	// Publish 10 events with size-5 ring.
	now := time.Now()
	for i := 0; i < 10; i++ {
		b.Publish(mkEnv("s", fmt.Sprintf("evt.%d", i), "kernel-v3", now.Add(time.Duration(i)*time.Millisecond)))
	}

	if b.ring.Len() != 5 {
		t.Errorf("ring len = %d; want 5", b.ring.Len())
	}

	// Replay from zero time should return all ring contents.
	replay := b.RingReplay(EventFilter{}, time.Time{})
	if len(replay) != 5 {
		t.Fatalf("replay len=%d; want 5", len(replay))
	}
	// Should be the last 5 events (5-9).
	for i, env := range replay {
		want := fmt.Sprintf("evt.%d", i+5)
		if env.HashedPayload.Type != want {
			t.Errorf("replay[%d]=%s; want %s", i, env.HashedPayload.Type, want)
		}
	}
}

func TestBrokerReplayFilterApplied(t *testing.T) {
	t.Parallel()
	b := newBrokerForTest(t, 1024, 16)
	now := time.Now()
	b.Publish(mkEnv("A", "foo", "kernel-v3", now))
	b.Publish(mkEnv("B", "foo", "kernel-v3", now))
	b.Publish(mkEnv("A", "bar", "mcp-client", now))

	replay := b.RingReplay(EventFilter{SessionID: "A", EventTypePattern: "foo"}, time.Time{})
	if len(replay) != 1 || replay[0].HashedPayload.SessionID != "A" || replay[0].HashedPayload.Type != "foo" {
		t.Errorf("unexpected replay: %+v", replay)
	}
}

func TestBrokerBackpressureDrop(t *testing.T) {
	t.Parallel()
	// Core assertion: a slow subscriber with a tiny channel buffer must not
	// block the broker; dropped events increment the slow sub's counter
	// while healthy subs continue receiving. Fast sub buffer is sized
	// generously so scheduling jitter doesn't starve it.
	b := NewEventBroker(EventBrokerOptions{
		RingSize:       1024,
		MaxSubscribers: 10,
		ChanBuffer:     4, // default for slow
	})
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	slow, err := b.Subscribe(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe slow: %v", err)
	}

	// Publish 20 events WITHOUT reading slow at all. Slow's channel fills
	// at 4, the remaining 16 must be dropped via the non-blocking send.
	now := time.Now()
	const pubN = 20
	for i := 0; i < pubN; i++ {
		b.Publish(mkEnv("s", fmt.Sprintf("e.%d", i), "kernel-v3", now.Add(time.Duration(i)*time.Millisecond)))
	}

	// Slow's dropped counter should be positive and at least pubN - chanBuf.
	drop := slow.Dropped()
	if drop == 0 {
		t.Errorf("slow sub dropped counter=0; expected >0 with chan buf=4 and %d events", pubN)
	}
	if drop < uint64(pubN-4) {
		t.Errorf("slow sub dropped=%d; expected >= %d", drop, pubN-4)
	}

	// Verify slow still has buffered events we can read (nothing is lost
	// from the buffered fraction).
	var received int
drain:
	for {
		select {
		case _, ok := <-slow.Events:
			if !ok {
				break drain
			}
			received++
		case <-time.After(100 * time.Millisecond):
			break drain
		}
	}
	if received == 0 {
		t.Errorf("slow sub received=0; expected >0 buffered events")
	}

	// Healthy sub: reads actively, should get all events if we trickle.
	fast, err := b.Subscribe(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe fast: %v", err)
	}
	var fastGot int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range fast.Events {
			atomic.AddInt32(&fastGot, 1)
		}
	}()

	// Publish with a tiny yield between sends so fast drains.
	for i := 0; i < pubN; i++ {
		b.Publish(mkEnv("s", fmt.Sprintf("e2.%d", i), "kernel-v3", now.Add(time.Duration(i)*time.Millisecond)))
		time.Sleep(time.Millisecond) // allow goroutine to run
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&fastGot) < int32(pubN) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&fastGot); got != int32(pubN) {
		t.Errorf("fast sub received=%d; want %d (trickled publishing)", got, pubN)
	}

	slow.Cancel()
	fast.Cancel()
	wg.Wait()
}

func TestBrokerContextCancelUnsubscribes(t *testing.T) {
	t.Parallel()
	b := newBrokerForTest(t, 1024, 16)

	ctx, cancel := context.WithCancel(context.Background())
	sub, err := b.Subscribe(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if n := b.SubscriberCount(); n != 1 {
		t.Fatalf("SubscriberCount=%d; want 1", n)
	}
	cancel()
	// The removal happens in a goroutine; poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && b.SubscriberCount() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := b.SubscriberCount(); n != 0 {
		t.Errorf("SubscriberCount after cancel=%d; want 0", n)
	}
	// Ensure the channel is eventually closed.
	select {
	case _, ok := <-sub.Events:
		if ok {
			t.Errorf("expected closed channel after cancel")
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("channel not closed after cancel")
	}
}

func TestBrokerCloseUnblocksSubs(t *testing.T) {
	t.Parallel()
	b := NewEventBroker(EventBrokerOptions{})

	ctx := context.Background()
	sub, err := b.Subscribe(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for range sub.Events {
		}
		close(done)
	}()

	if err := b.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine still blocked after Close")
	}

	// Second Close should be a no-op.
	if err := b.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Subscribe after Close should return an error.
	if _, err := b.Subscribe(ctx, EventFilter{}); err == nil {
		t.Errorf("Subscribe after Close returned nil error")
	}
}

func TestBrokerConcurrentPubSub(t *testing.T) {
	t.Parallel()
	const (
		nSubs    = 5
		nPubs    = 5
		perPub   = 200
		totalPub = nPubs * perPub
	)
	b := newBrokerForTest(t, totalPub+10, totalPub+10) // ample buffer so no drops

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	subs := make([]*Subscription, nSubs)
	for i := range subs {
		s, err := b.Subscribe(ctx, EventFilter{})
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		subs[i] = s
	}

	var wg sync.WaitGroup
	wg.Add(nPubs)
	start := time.Now()
	for p := 0; p < nPubs; p++ {
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPub; i++ {
				env := mkEnv(fmt.Sprintf("s-%d", p), fmt.Sprintf("e.%d.%d", p, i), "kernel-v3",
					start.Add(time.Duration(p*perPub+i)*time.Microsecond))
				b.Publish(env)
			}
		}(p)
	}
	wg.Wait()

	// Each sub should drain exactly totalPub events. Use a deadline guard.
	for i, s := range subs {
		got := 0
		deadline := time.After(3 * time.Second)
	drain:
		for got < totalPub {
			select {
			case <-s.Events:
				got++
			case <-deadline:
				break drain
			}
		}
		if got != totalPub {
			t.Errorf("sub[%d] received %d; want %d", i, got, totalPub)
		}
	}
}

func TestBrokerMaxSubscribersEnforced(t *testing.T) {
	t.Parallel()
	b := NewEventBroker(EventBrokerOptions{MaxSubscribers: 2})
	t.Cleanup(func() { _ = b.Close() })

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := b.Subscribe(ctx, EventFilter{}); err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
	}
	if _, err := b.Subscribe(ctx, EventFilter{}); err == nil {
		t.Errorf("third Subscribe succeeded; want ErrTooManySubscribers")
	}
}

// ── AppendEvent ↔ broker wiring tests ──────────────────────────────────────

func TestAppendEventPublishesToBroker(t *testing.T) {
	t.Parallel()
	// Additive registration — other tests' brokers stay registered, but
	// our session filter ensures we only see our own event.
	b := NewEventBroker(EventBrokerOptions{})
	RegisterBroker(b)
	t.Cleanup(func() { _ = b.Close() }) // Close unregisters.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sid := "wiring-test-session-" + nowISO()
	sub, err := b.Subscribe(ctx, EventFilter{SessionID: sid})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	root := t.TempDir()
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "observer.surprise",
			Timestamp: nowISO(),
			SessionID: sid,
		},
		Metadata: EventMetadata{Source: "kernel-v3"},
	}
	if err := AppendEvent(root, sid, env); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	select {
	case got := <-sub.Events:
		if got.HashedPayload.Type != "observer.surprise" {
			t.Errorf("broker received type=%s; want observer.surprise", got.HashedPayload.Type)
		}
		if got.Metadata.Hash == "" {
			t.Errorf("broker envelope missing hash — AppendEvent should set it before publishing")
		}
	case <-time.After(time.Second):
		t.Fatal("broker did not receive event")
	}
}

func TestAppendEventNilBrokerSafe(t *testing.T) {
	t.Parallel()
	// No broker installed for this test's session — the additive registry
	// means other brokers may still receive it (and drop on session
	// filter), but AppendEvent must not crash when the local slot is unset.
	root := t.TempDir()
	sid := "nil-broker-session-" + nowISO()
	env := &EventEnvelope{
		HashedPayload: EventPayload{
			Type:      "test.event",
			Timestamp: nowISO(),
			SessionID: sid,
		},
		Metadata: EventMetadata{Source: "kernel-v3"},
	}
	if err := AppendEvent(root, sid, env); err != nil {
		t.Fatalf("AppendEvent with no local broker: %v", err)
	}
}

// ── Helper tests ───────────────────────────────────────────────────────────

func TestParseSinceDuration(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"2026-04-21T11:00:00Z", time.Date(2026, 4, 21, 11, 0, 0, 0, time.UTC)},
		{"5m", now.Add(-5 * time.Minute)},
		{"1h", now.Add(-time.Hour)},
	}
	for _, c := range cases {
		got, err := ParseSinceDuration(c.in, now)
		if err != nil {
			t.Errorf("ParseSinceDuration(%q): %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("ParseSinceDuration(%q) = %v; want %v", c.in, got, c.want)
		}
	}

	if _, err := ParseSinceDuration("not-a-duration", now); err == nil {
		t.Errorf("ParseSinceDuration accepted garbage")
	}
}
