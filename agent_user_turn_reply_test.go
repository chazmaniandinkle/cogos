// agent_user_turn_reply_test.go — Coverage for ensureUserTurnReply.
//
// The reply guarantee is the contract that a consumed user turn always
// produces *something* on bus_dashboard_response — even when the cycle
// errored, slept, or returned an empty execute result. This test isolates
// the helper from the bus by swapping publishUserTurnReply for a capture.

package main

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureSink intercepts publish calls and records them. Embeds a mutex so
// the goroutine ensureUserTurnReply spawns can write back safely.
type captureSink struct {
	mu    sync.Mutex
	calls []capturedReply
	done  chan struct{}
}

type capturedReply struct {
	Text      string
	Reasoning string
	SessionID string
}

func newCaptureSink() *captureSink {
	return &captureSink{done: make(chan struct{}, 4)}
}

func (c *captureSink) publish(text, reasoning, sessionID string) (int, error) {
	c.mu.Lock()
	c.calls = append(c.calls, capturedReply{text, reasoning, sessionID})
	c.mu.Unlock()
	c.done <- struct{}{}
	return len(text), nil
}

func (c *captureSink) waitOne(t *testing.T) capturedReply {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for publish; captured %d so far", len(c.calls))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[len(c.calls)-1]
}

func (c *captureSink) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// withSink installs the capture in publishUserTurnReply for the duration of
// fn, restoring the production publisher after.
func withSink(sink *captureSink, fn func()) {
	orig := publishUserTurnReply
	publishUserTurnReply = sink.publish
	defer func() { publishUserTurnReply = orig }()
	fn()
}

func userTurn(text, sessionID string) []pendingUserMsg {
	return []pendingUserMsg{{Text: text, SessionID: sessionID, Ts: time.Now()}}
}

func TestEnsureUserTurnReply_PublishesOnCycleError(t *testing.T) {
	sink := newCaptureSink()
	withSink(sink, func() {
		ensureUserTurnReply(
			userTurn("hello", "sess-A"),
			0, // respondSnap — respond was never called
			errors.New("ollama: context deadline exceeded"),
			nil, // assessment is nil when assess errored
			"",
		)
		got := sink.waitOne(t)
		if got.SessionID != "sess-A" {
			t.Errorf("session_id = %q, want sess-A", got.SessionID)
		}
		if got.Text == "" {
			t.Error("expected non-empty reply on cycle error")
		}
		// Must surface the error so the user knows why they got a non-answer.
		if !strings.Contains(got.Text, "cycle failed") {
			t.Errorf("reply text should mention cycle failure; got %q", got.Text)
		}
	})
}

func TestEnsureUserTurnReply_PublishesExecuteResult(t *testing.T) {
	sink := newCaptureSink()
	withSink(sink, func() {
		ensureUserTurnReply(
			userTurn("hi", "sess-B"),
			0,
			nil,
			&Assessment{Action: "execute", Reason: "process inbox"},
			"   I processed 3 items.   ",
		)
		got := sink.waitOne(t)
		if got.Text != "I processed 3 items." {
			t.Errorf("text = %q, want trimmed execute result", got.Text)
		}
		if got.SessionID != "sess-B" {
			t.Errorf("session_id = %q, want sess-B", got.SessionID)
		}
	})
}

func TestEnsureUserTurnReply_PublishesEmptyExecuteWithSleep(t *testing.T) {
	sink := newCaptureSink()
	withSink(sink, func() {
		ensureUserTurnReply(
			userTurn("hey", "sess-C"),
			0,
			nil,
			&Assessment{Action: "sleep", Reason: "nothing to do"},
			"", // sleep skips Execute → empty result
		)
		got := sink.waitOne(t)
		if got.Text == "" {
			t.Fatal("expected synthesized reply for sleep + empty result")
		}
		if !strings.Contains(got.Text, "sleep") {
			t.Errorf("synthesized reply should mention the action; got %q", got.Text)
		}
	})
}

func TestEnsureUserTurnReply_NoUserTurn_NoPublish(t *testing.T) {
	sink := newCaptureSink()
	withSink(sink, func() {
		ensureUserTurnReply(nil, 0, nil, &Assessment{Action: "sleep"}, "")
		// Tiny grace period in case a goroutine got scheduled.
		select {
		case <-sink.done:
			t.Fatal("publish must not happen when there is no user turn")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

func TestEnsureUserTurnReply_RespondAlreadyInvoked_NoPublish(t *testing.T) {
	// Snapshot before the increment so respondInvokedSince(snap) == true.
	// respondInvokeCount is a package-global counter; we only ever bump it
	// (counters are monotonic), which is safe for parallel tests since the
	// snapshot taken here captures the pre-bump value for THIS test only.
	snap := respondInvokeSnapshot()
	atomic.AddUint64(&respondInvokeCount, 1)

	sink := newCaptureSink()
	withSink(sink, func() {
		ensureUserTurnReply(
			userTurn("hi", "sess-D"),
			snap,
			nil,
			&Assessment{Action: "execute", Reason: "ok"},
			"some result",
		)
		select {
		case <-sink.done:
			t.Fatal("publish must be suppressed when respond tool already landed this turn")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

