package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalHarnessControllerTriggerAndList(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.LocalModel = "gemma4:e4b"
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)

	var call int
	model := "gemma4:e4b"
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": model}},
			})
		case "/api/chat":
			call++
			w.Header().Set("Content-Type", "application/json")
			if call == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"action":"observe","reason":"field changed","urgency":0.4,"target":"memory","task":"summarize current state"}`,
					},
					"done":              true,
					"prompt_eval_count": 1,
					"eval_count":        1,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": "local harness executed",
				},
				"done":              true,
				"prompt_eval_count": 1,
				"eval_count":        1,
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer llm.Close()
	t.Setenv(localLLMEndpointEnv, llm.URL)

	ctrl, err := NewLocalHarnessController(cfg, makeNucleus("Cog", "tester"), proc, srv.mcpServer)
	if err != nil {
		t.Fatalf("NewLocalHarnessController: %v", err)
	}

	res, err := ctrl.TriggerAgent(context.Background(), DefaultAgentID, "test", true)
	if err != nil {
		t.Fatalf("TriggerAgent: %v", err)
	}
	if !res.Triggered {
		t.Fatalf("expected triggered=true, got %+v", res)
	}
	if res.Action != "observe" {
		t.Fatalf("Action = %q; want observe", res.Action)
	}

	list, err := ctrl.ListAgents(context.Background(), false)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListAgents len = %d; want 1", len(list))
	}
	if list[0].CycleCount != 1 {
		t.Fatalf("CycleCount = %d; want 1", list[0].CycleCount)
	}

	snap, err := ctrl.GetAgent(context.Background(), DefaultAgentID, true, 5)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if len(snap.Traces) != 1 {
		t.Fatalf("Traces len = %d; want 1", len(snap.Traces))
	}
	if snap.Traces[0].Result != "local harness executed" {
		t.Fatalf("trace result = %q; want local harness executed", snap.Traces[0].Result)
	}
}

func TestServerLegacyAgentStatusRoute(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	proc := NewProcess(cfg, makeNucleus("Cog", "tester"))
	srv := NewServer(cfg, makeNucleus("Cog", "tester"), proc)
	srv.SetAgentController(&fakeAgentController{
		GetResult: &AgentSnapshot{
			Summary: AgentSummary{
				AgentID:     DefaultAgentID,
				Alive:       true,
				CycleCount:  3,
				LastAction:  "sleep",
				LastCycle:   "2026-04-21T12:00:00Z",
				LastUrgency: 0.2,
				LastReason:  "idle",
				LastDurMs:   42,
				Model:       "gemma4:e4b",
				Interval:    "1m0s",
				UptimeSec:   60,
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["cycle_count"].(float64) != 3 {
		t.Fatalf("cycle_count = %v; want 3", body["cycle_count"])
	}
	if body["uptime"].(string) != "1m0s" {
		t.Fatalf("uptime = %v; want 1m0s", body["uptime"])
	}
}
