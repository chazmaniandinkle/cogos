// tools_classify_test.go exercises the tool-ownership classifier and
// partitioner introduced for the BrowserOS bridge. These tests lock down
// the invariant that BrowserOS-style `browser_*` tools survive the
// pipeline as external while CogOS built-ins (bash/read/write/edit/...)
// stay internal.
package harness

import (
	"encoding/json"
	"testing"
)

func TestClassifyTool_InternalBuiltins(t *testing.T) {
	cases := []string{
		"bash", "Bash", "BASH",
		"exec", "shell",
		"read", "FILE_read",
		"write", "file_write",
		"edit", "apply-patch", "apply_patch",
		"search", "grep", "glob", "find",
	}
	for _, name := range cases {
		if got := ClassifyTool(name); got != ToolInternal {
			t.Errorf("ClassifyTool(%q) = %s; want internal", name, got)
		}
	}
}

func TestClassifyTool_ExternalBrowserOS(t *testing.T) {
	cases := []string{
		"browser_navigate",
		"browser_click",
		"browser_take_snapshot",
		"custom_magic_tool",
		"",
		"unknown",
	}
	for _, name := range cases {
		if got := ClassifyTool(name); got != ToolExternal {
			t.Errorf("ClassifyTool(%q) = %s; want external", name, got)
		}
	}
}

func TestExtractToolName(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"type":"function","function":{"name":"bash"}}`, "bash"},
		{`{"function":{"name":"browser_navigate"}}`, "browser_navigate"},
		{`{"function":{}}`, ""},
		{`{}`, ""},
		{`not-json`, ""},
	}
	for _, c := range cases {
		got := ExtractToolName(json.RawMessage(c.raw))
		if got != c.want {
			t.Errorf("ExtractToolName(%q) = %q; want %q", c.raw, got, c.want)
		}
	}
}

func TestPartitionTools_MixedSet(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"bash"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"browser_navigate"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"read"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"browser_click"}}`),
		json.RawMessage(`{"type":"function","function":{"name":""}}`),           // dropped
		json.RawMessage(`not-json`),                                             // dropped
	}
	internal, external := PartitionTools(tools)
	if len(internal) != 2 {
		t.Errorf("internal count = %d; want 2", len(internal))
	}
	if len(external) != 2 {
		t.Errorf("external count = %d; want 2", len(external))
	}
	if ExtractToolName(internal[0]) != "bash" || ExtractToolName(internal[1]) != "read" {
		t.Errorf("internal order wrong: %v", []string{ExtractToolName(internal[0]), ExtractToolName(internal[1])})
	}
	if ExtractToolName(external[0]) != "browser_navigate" || ExtractToolName(external[1]) != "browser_click" {
		t.Errorf("external order wrong: %v", []string{ExtractToolName(external[0]), ExtractToolName(external[1])})
	}
}

func TestPartitionTools_Empty(t *testing.T) {
	internal, external := PartitionTools(nil)
	if internal != nil || external != nil {
		t.Errorf("empty input produced non-nil output: internal=%v external=%v", internal, external)
	}
	internal, external = PartitionTools([]json.RawMessage{})
	if internal != nil || external != nil {
		t.Errorf("zero-length input produced non-nil output: internal=%v external=%v", internal, external)
	}
}

func TestMapToolsToCLINames_ExternalToolsDropped(t *testing.T) {
	// Regression: the audit flagged the hardcoded 6-tool list as the reason
	// external tools were lost. After the refactor, MapToolsToCLINames still
	// maps only internal tools (intentional — external tools are registered
	// via MCP, not --allowed-tools) but the classifier is shared so
	// PartitionTools and MapToolsToCLINames agree on what "internal" means.
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"bash"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"browser_navigate"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"grep"}}`),
	}
	names := MapToolsToCLINames(tools)
	if len(names) != 2 {
		t.Fatalf("mapped %d names; want 2 (browser_navigate must be dropped)", len(names))
	}
	for _, n := range names {
		if n == "browser_navigate" {
			t.Error("external tool browser_navigate leaked into --allowed-tools list")
		}
	}
}
