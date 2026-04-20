// agent_tools_respond_test.go — Coverage for the respond tool's per-session
// fan-out contract.
//
// The respond tool must emit one publish per unique session_id observed on
// the cycle's pending queue (carried on ctx via WithSessionIDs). Without the
// fan-out, a cycle drained from multiple tabs would only reply on whichever
// session_id was first — the BLOCKER Codex flagged during sub-PR #3 review.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// respondSink captures respond-tool publish calls without a live bus.
// Goroutine-safe because the respond tool may run concurrently in real
// deployments, and tests should tolerate the same.
type respondSink struct {
	mu    sync.Mutex
	calls []capturedReply
	err   error // if non-nil, publish returns this instead of capturing
}

func (r *respondSink) publish(text, reasoning, sessionID string) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, capturedReply{text, reasoning, sessionID})
	return len(text), nil
}

func (r *respondSink) snapshot() []capturedReply {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedReply, len(r.calls))
	copy(out, r.calls)
	return out
}

// withRespondSink swaps respondPublish for the duration of fn, restoring the
// production publisher after.
func withRespondSink(sink *respondSink, fn func()) {
	orig := respondPublish
	respondPublish = sink.publish
	defer func() { respondPublish = orig }()
	fn()
}

// callRespond drives the respond tool func synchronously with the given ctx
// and args payload. Returns the JSON result (as a decoded map) and any error.
func callRespond(t *testing.T, ctx context.Context, text string) map[string]interface{} {
	t.Helper()
	args, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := newRespondFunc()(ctx, args)
	if err != nil {
		t.Fatalf("respond returned error: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, string(out))
	}
	return decoded
}

func TestRespondTool_FanOutAcrossSessions(t *testing.T) {
	sink := &respondSink{}
	withRespondSink(sink, func() {
		preSnap := respondInvokeSnapshot()
		ctx := WithSessionIDs(context.Background(), []string{"sess-A", "sess-B"})

		result := callRespond(t, ctx, "shared answer")
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("respond returned ok=false: %v", result)
		}
		if recips, _ := result["recipients"].(float64); int(recips) != 2 {
			t.Errorf("recipients = %v, want 2", result["recipients"])
		}

		calls := sink.snapshot()
		if !sameStringSet(sessionIDsOf(calls), []string{"sess-A", "sess-B"}) {
			t.Errorf("recipients = %v, want {sess-A, sess-B}", sessionIDsOf(calls))
		}
		for _, c := range calls {
			if c.Text != "shared answer" {
				t.Errorf("text = %q, want %q (payload must be identical per recipient)", c.Text, "shared answer")
			}
		}

		// Counter must bump exactly once per respond-tool invocation, even
		// though we published twice — the counter gates the auto-fallback,
		// and multi-session fan-out is still one tool call.
		after := atomic.LoadUint64(&respondInvokeCount)
		if after-preSnap != 1 {
			t.Errorf("respondInvokeCount delta = %d, want 1", after-preSnap)
		}
	})
}

func TestRespondTool_LegacySingleSessionContext(t *testing.T) {
	sink := &respondSink{}
	withRespondSink(sink, func() {
		preSnap := respondInvokeSnapshot()
		// Legacy path: ctx has only WithSessionID, not WithSessionIDs.
		ctx := WithSessionID(context.Background(), "sess-legacy")

		result := callRespond(t, ctx, "hi")
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("respond returned ok=false: %v", result)
		}
		calls := sink.snapshot()
		if len(calls) != 1 {
			t.Fatalf("got %d publishes, want 1", len(calls))
		}
		if calls[0].SessionID != "sess-legacy" {
			t.Errorf("session_id = %q, want sess-legacy", calls[0].SessionID)
		}
		if atomic.LoadUint64(&respondInvokeCount)-preSnap != 1 {
			t.Errorf("counter should bump exactly once on legacy single-session path")
		}
	})
}

func TestRespondTool_NoContextSessionBroadcast(t *testing.T) {
	// No session_id on ctx → legacy broadcast (publish with empty sid).
	sink := &respondSink{}
	withRespondSink(sink, func() {
		preSnap := respondInvokeSnapshot()
		result := callRespond(t, context.Background(), "hi")
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("respond returned ok=false: %v", result)
		}
		calls := sink.snapshot()
		if len(calls) != 1 {
			t.Fatalf("got %d publishes, want 1 (broadcast)", len(calls))
		}
		if calls[0].SessionID != "" {
			t.Errorf("session_id = %q, want empty (broadcast)", calls[0].SessionID)
		}
		if atomic.LoadUint64(&respondInvokeCount)-preSnap != 1 {
			t.Errorf("counter must still bump for broadcast path")
		}
	})
}

func TestRespondTool_AllPublishesFailDoesNotBumpCounter(t *testing.T) {
	// All fan-out publishes fail → counter must NOT bump; auto-fallback
	// depends on the invariant "counter bump == dashboard heard us".
	sink := &respondSink{err: errors.New("bus write failure")}
	withRespondSink(sink, func() {
		preSnap := respondInvokeSnapshot()
		ctx := WithSessionIDs(context.Background(), []string{"sess-A", "sess-B"})
		result := callRespond(t, ctx, "will fail")
		if ok, _ := result["ok"].(bool); ok {
			t.Fatalf("respond should have returned ok=false on total failure: %v", result)
		}
		if atomic.LoadUint64(&respondInvokeCount) != preSnap {
			t.Errorf("counter must not bump when every publish failed")
		}
	})
}

