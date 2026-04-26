// scoring.go — Go port of evals/harness/scoring.py.
//
// Ports the score() function line-for-line from the Python implementation,
// including the _ci (case-insensitive) field variants added in Phase C.
//
// Per design memo Q8: direct port, exact shape. No weighted scoring,
// no judge integration. Those are Phase D+ concerns.
//
// ScoredResult is the minimal interface that carries what the scorer needs.
// This is cleaner than the Python shim pattern (_agentic_to_scorable) because
// Go lets us define a thin interface without modifying the callers.
package eval

import "strings"

// ScoredResult is the minimal interface the scorer needs from a dispatch result.
// Implemented by DispatchScoredResult (which wraps a DispatchResult) and by
// test stubs.
type ScoredResult interface {
	// Content returns the final assistant text response.
	Content() string
	// ToolCallNames returns the names of tool calls made during the trial,
	// in invocation order.
	ToolCallNames() []string
	// FinishReason returns the reason the model stopped (e.g. "stop", "tool_calls").
	FinishReason() string
}

// Score evaluates a rubric against a scored result and returns a Verdict.
//
// Direct port of evals/harness/scoring.py score() (lines 23-69).
// Includes case-insensitive variants (content_contains_ci, content_must_not_contain_ci).
func Score(rubric Rubric, result ScoredResult) Verdict {
	var failures []string
	var notes []string

	called := result.ToolCallNames()
	if len(called) == 0 {
		notes = append(notes, "tool_calls: []")
	} else {
		notes = append(notes, "tool_calls: "+joinNames(called))
	}
	notes = append(notes, "finish_reason: "+result.FinishReason())

	// expected_tools: each must appear in tool-call sequence
	for _, req := range rubric.ExpectedTools {
		if !containsStr(called, req) {
			failures = append(failures, "expected_tools: missing "+quote(req)+"; got "+formatList(called))
		}
	}

	// expected_tools_any_of: at least one must appear
	if len(rubric.ExpectedToolsAnyOf) > 0 {
		anyFound := false
		for _, t := range rubric.ExpectedToolsAnyOf {
			if containsStr(called, t) {
				anyFound = true
				break
			}
		}
		if !anyFound {
			failures = append(failures, "expected_tools_any_of: none of "+formatList(rubric.ExpectedToolsAnyOf)+" appeared; got "+formatList(called))
		}
	}

	// forbidden_tools: none must appear
	for _, forbid := range rubric.ForbiddenTools {
		if containsStr(called, forbid) {
			failures = append(failures, "forbidden_tools: "+quote(forbid)+" was called")
		}
	}

	// first_tool_one_of: first call must be one of the allowed names
	if len(rubric.FirstToolOneOf) > 0 {
		var first string
		if len(called) > 0 {
			first = called[0]
		}
		if !containsStr(rubric.FirstToolOneOf, first) {
			failures = append(failures,
				"first_tool_one_of: first call was "+quoteOrNone(first)+"; expected one of "+formatList(rubric.FirstToolOneOf))
		}
	}

	content := result.Content()

	// content_contains: each string must appear (case-sensitive)
	for _, needle := range rubric.ContentContains {
		if !strings.Contains(content, needle) {
			failures = append(failures, "content_contains: "+quote(needle)+" not in content")
		}
	}

	// content_must_not_contain: must not appear (case-sensitive)
	for _, forbid := range rubric.ContentMustNotContain {
		if strings.Contains(content, forbid) {
			failures = append(failures, "content_must_not_contain: "+quote(forbid)+" appeared in content")
		}
	}

	// content_contains_ci: case-insensitive
	contentLower := strings.ToLower(content)
	for _, needle := range rubric.ContentContainsCI {
		if !strings.Contains(contentLower, strings.ToLower(needle)) {
			failures = append(failures, "content_contains_ci: "+quote(needle)+" not in content (case-insensitive)")
		}
	}

	// content_must_not_contain_ci: case-insensitive
	for _, forbid := range rubric.ContentMustNotContainCI {
		if strings.Contains(contentLower, strings.ToLower(forbid)) {
			failures = append(failures, "content_must_not_contain_ci: "+quote(forbid)+" appeared in content (case-insensitive)")
		}
	}

	return Verdict{
		Passed:   len(failures) == 0,
		Failures: failures,
		Notes:    notes,
	}
}

// DispatchScoredResult adapts a DispatchResult for use with Score.
// It also carries tool-call names extracted from the dispatch batch result
// (tool_calls are stored separately in TrialRecord).
type DispatchScoredResult struct {
	result    DispatchResult
	toolCalls []string
}

// NewDispatchScoredResult wraps a DispatchResult for scoring.
// toolCalls is the ordered list of tool call names extracted from the result.
func NewDispatchScoredResult(r DispatchResult, toolCalls []string) *DispatchScoredResult {
	return &DispatchScoredResult{result: r, toolCalls: toolCalls}
}

func (d *DispatchScoredResult) Content() string        { return d.result.Content }
func (d *DispatchScoredResult) ToolCallNames() []string { return d.toolCalls }
func (d *DispatchScoredResult) FinishReason() string {
	if len(d.toolCalls) > 0 && d.result.Content == "" {
		return "tool_calls"
	}
	return "stop"
}

// ---------------------------------------------------------------------------
// String formatting helpers (internal to scoring)
// ---------------------------------------------------------------------------

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func quote(s string) string {
	return "'" + s + "'"
}

func quoteOrNone(s string) string {
	if s == "" {
		return "None"
	}
	return quote(s)
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	return "[" + strings.Join(names, ", ") + "]"
}

func formatList(names []string) string {
	return joinNames(names)
}
