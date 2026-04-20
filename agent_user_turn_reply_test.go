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
			respondInvokeSnapshot(), // fresh snapshot — respond must not appear to have landed
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
			respondInvokeSnapshot(),
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
			respondInvokeSnapshot(),
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
		ensureUserTurnReply(nil, respondInvokeSnapshot(), nil, &Assessment{Action: "sleep"}, "")
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

// waitN blocks until n publish calls have landed or the timeout expires.
// Returns the captured calls (stable order not guaranteed since they race).
func (c *captureSink) waitN(t *testing.T, n int) []capturedReply {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for c.callCount() < n {
		select {
		case <-c.done:
		case <-deadline:
			t.Fatalf("timed out waiting for %d publishes; captured %d", n, c.callCount())
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedReply, len(c.calls))
	copy(out, c.calls)
	return out
}

// sessionIDsOf extracts the session_id field from a list of captured replies.
// Used for set-comparison assertions that don't depend on goroutine order.
func sessionIDsOf(calls []capturedReply) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.SessionID
	}
	return out
}

// sameStringSet returns true if got and want contain the same strings,
// regardless of ordering. Used to assert fan-out recipients without being
// sensitive to which goroutine happened to land first.
func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	gotMap := make(map[string]int, len(got))
	for _, s := range got {
		gotMap[s]++
	}
	for _, s := range want {
		if gotMap[s] == 0 {
			return false
		}
		gotMap[s]--
	}
	return true
}

// TestEnsureUserTurnReply_FanOutTwoSessions: cycle drained two messages from
// two different sessions and the agent emitted ONE response (executeResult).
// Contract: one publish per unique session_id, same payload.
func TestEnsureUserTurnReply_FanOutTwoSessions(t *testing.T) {
	sink := newCaptureSink()
	withSink(sink, func() {
		pending := []pendingUserMsg{
			{Text: "from A", SessionID: "sess-A", Ts: time.Now()},
			{Text: "from B", SessionID: "sess-B", Ts: time.Now()},
		}
		ensureUserTurnReply(
			pending,
			respondInvokeSnapshot(),
			nil,
			&Assessment{Action: "execute", Reason: "ok"},
			"shared answer",
		)
		calls := sink.waitN(t, 2)
		if !sameStringSet(sessionIDsOf(calls), []string{"sess-A", "sess-B"}) {
			t.Errorf("recipients = %v, want {sess-A, sess-B}", sessionIDsOf(calls))
		}
		for _, c := range calls {
			if c.Text != "shared answer" {
				t.Errorf("text = %q, want %q (payload should be identical across sessions)", c.Text, "shared answer")
			}
		}
	})
}

// TestEnsureUserTurnReply_FanOutSameSessionCollapses: two messages from the
// same session collapse to one publish. Guards against N-messages-N-replies
// regression when a single tab sends multiple messages in one cycle.
func TestEnsureUserTurnReply_FanOutSameSessionCollapses(t *testing.T) {
	sink := newCaptureSink()
	withSink(sink, func() {
		pending := []pendingUserMsg{
			{Text: "msg 1", SessionID: "sess-X", Ts: time.Now()},
			{Text: "msg 2", SessionID: "sess-X", Ts: time.Now()},
		}
		ensureUserTurnReply(
			pending,
			respondInvokeSnapshot(),
			nil,
			&Assessment{Action: "execute", Reason: "ok"},
			"ack",
		)
		// Wait for the one expected call, then confirm no second arrives.
		got := sink.waitOne(t)
		if got.SessionID != "sess-X" {
			t.Errorf("session_id = %q, want sess-X", got.SessionID)
		}
		select {
		case <-sink.done:
			t.Fatalf("unexpected second publish; calls=%d", sink.callCount())
		case <-time.After(50 * time.Millisecond):
		}
	})
}

// TestEnsureUserTurnReply_FanOutErrorMultiSession: cycle errored after draining
// messages from two sessions. Every session must still receive the failure
// notice — silence on one tab would be the original BLOCKER regression.
func TestEnsureUserTurnReply_FanOutErrorMultiSession(t *testing.T) {
	sink := newCaptureSink()
	withSink(sink, func() {
		pending := []pendingUserMsg{
			{Text: "A", SessionID: "sess-A", Ts: time.Now()},
			{Text: "B", SessionID: "sess-B", Ts: time.Now()},
		}
		ensureUserTurnReply(
			pending,
			respondInvokeSnapshot(),
			errors.New("ollama: deadline"),
			nil,
			"",
		)
		calls := sink.waitN(t, 2)
		if !sameStringSet(sessionIDsOf(calls), []string{"sess-A", "sess-B"}) {
			t.Errorf("recipients = %v, want {sess-A, sess-B}", sessionIDsOf(calls))
		}
		for _, c := range calls {
			if !strings.Contains(c.Text, "cycle failed") {
				t.Errorf("reply text should mention cycle failure; got %q", c.Text)
			}
		}
	})
}

// TestUniqueUserMessageSessionIDs covers the ordering + dedup + empty-id
// contract that the fan-out loop depends on.
func TestUniqueUserMessageSessionIDs(t *testing.T) {
	cases := []struct {
		name string
		in   []pendingUserMsg
		want []string
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "single session preserved",
			in:   []pendingUserMsg{{SessionID: "a"}},
			want: []string{"a"},
		},
		{
			name: "duplicates collapse, first-seen order preserved",
			in: []pendingUserMsg{
				{SessionID: "a"},
				{SessionID: "b"},
				{SessionID: "a"},
				{SessionID: "c"},
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "empty id collapses to one broadcast entry",
			in:   []pendingUserMsg{{SessionID: ""}, {SessionID: ""}},
			want: []string{""},
		},
		{
			name: "mixed empty + named",
			in: []pendingUserMsg{
				{SessionID: ""},
				{SessionID: "a"},
				{SessionID: ""},
			},
			want: []string{"", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uniqueUserMessageSessionIDs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

