// events_query_test.go — tests for the observability-flavored query wrapper.
package engine

import (
	"testing"
	"time"
)

func TestQueryEventsEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	res, err := QueryEvents(root, EventQuery{})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if res.Count != 0 {
		t.Errorf("Count = %d; want 0", res.Count)
	}
	if len(res.Events) != 0 {
		t.Errorf("len(events) = %d; want 0", len(res.Events))
	}
}

func TestQueryEventsFilterBySource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "query-src"

	// Append two events with different sources via the shared test helper.
	// We build envelopes manually because appendTestEvent uses a fixed
	// "test" source.
	mkEvt := func(typ, src string) *EventEnvelope {
		return &EventEnvelope{
			HashedPayload: EventPayload{
				Type:      typ,
				Timestamp: nowISO(),
				SessionID: sid,
			},
			Metadata: EventMetadata{Source: src},
		}
	}
	for _, ev := range []*EventEnvelope{
		mkEvt("foo", "kernel-v3"),
		mkEvt("bar", "mcp-client"),
		mkEvt("baz", "kernel-v3"),
	} {
		if err := AppendEvent(root, sid, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	res, err := QueryEvents(root, EventQuery{Source: "kernel-v3"})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if res.Count != 2 {
		t.Errorf("Count=%d; want 2 (source filter)", res.Count)
	}
	for _, ev := range res.Events {
		if ev.Source != "kernel-v3" {
			t.Errorf("event source=%s; want kernel-v3", ev.Source)
		}
	}
}

func TestQueryEventsOrderDefaults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "query-order"

	// Append three events in known chronological order.
	for _, name := range []string{"first", "second", "third"} {
		appendTestEvent(t, root, sid, name, nil, "")
		time.Sleep(1 * time.Millisecond) // ensure distinct timestamps
	}

	// Default (desc): newest first.
	res, err := QueryEvents(root, EventQuery{SessionID: sid})
	if err != nil {
		t.Fatalf("QueryEvents desc: %v", err)
	}
	if res.Count != 3 {
		t.Fatalf("count=%d; want 3", res.Count)
	}
	if res.Events[0].Type != "third" {
		t.Errorf("desc order: first event type=%s; want third", res.Events[0].Type)
	}

	// asc: oldest first.
	res, err = QueryEvents(root, EventQuery{SessionID: sid, Order: "asc"})
	if err != nil {
		t.Fatalf("QueryEvents asc: %v", err)
	}
	if res.Events[0].Type != "first" {
		t.Errorf("asc order: first event type=%s; want first", res.Events[0].Type)
	}
}

func TestQueryEventsLimitTruncates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "query-limit"
	for i := 0; i < 5; i++ {
		appendTestEvent(t, root, sid, "e", nil, "")
	}
	res, err := QueryEvents(root, EventQuery{SessionID: sid, Limit: 2})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if res.Count != 2 {
		t.Errorf("Count=%d; want 2", res.Count)
	}
	if !res.Truncated {
		t.Errorf("Truncated=false; want true")
	}
	if res.NextBefore == "" {
		t.Errorf("NextBefore empty; want RFC3339 timestamp")
	}
}
