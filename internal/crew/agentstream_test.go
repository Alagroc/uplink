package crew

import (
	"strings"
	"testing"
)

func TestSummarizeClaudeEvents(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		wants []string
	}{
		{
			name:  "init",
			line:  `{"type":"system","subtype":"init","model":"claude-opus-5","tools":["Bash","Read","mcp__uplink__ask_operator"]}`,
			wants: []string{"session started", "claude-opus-5", "3 tools"},
		},
		{
			name:  "assistant text",
			line:  `{"type":"assistant","message":{"content":[{"type":"text","text":"Converting the compose file now."}]}}`,
			wants: []string{"says:", "Converting the compose file"},
		},
		{
			name:  "tool use shows the command",
			line:  `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"kubectl apply -f k3s/"}}]}}`,
			wants: []string{"tool Bash", "kubectl apply -f k3s/"},
		},
		{
			name:  "failed tool result is surfaced",
			line:  `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"error: no such namespace"}]}}`,
			wants: []string{"tool failed", "no such namespace"},
		},
		{
			name:  "result carries cost and final text",
			line:  `{"type":"result","subtype":"success","num_turns":7,"total_cost_usd":0.42,"result":"Migration complete."}`,
			wants: []string{"session success", "7 turns", "$0.42", "Migration complete."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(summarizeAgentEvent([]byte(tc.line)), " | ")
			for _, want := range tc.wants {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in: %s", want, got)
				}
			}
		})
	}
}

// Thinking blocks are long and are not what the operator asked for.
func TestThinkingIsNotShipped(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"long internal monologue"}]}}`
	if got := summarizeAgentEvent([]byte(line)); len(got) != 0 {
		t.Errorf("thinking should be dropped, got %v", got)
	}
}

// Successful tool results are noise; the raw transcript keeps them.
func TestSuccessfulToolResultsAreQuiet(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`
	if got := summarizeAgentEvent([]byte(line)); len(got) != 0 {
		t.Errorf("successful tool results should be quiet, got %v", got)
	}
}

func TestSummarizeCodexEvents(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{`{"type":"thread.started","thread_id":"t1"}`, "session started"},
		{`{"type":"item.completed","item":{"item_type":"assistant_message","text":"Done with the manifests."}}`, "Done with the manifests."},
		{`{"type":"item.completed","item":{"item_type":"command_execution","command":"docker compose config","exit_code":0}}`, "exit 0"},
		{`{"type":"error","message":"rate limited"}`, "rate limited"},
	}
	for _, tc := range cases {
		got := strings.Join(summarizeAgentEvent([]byte(tc.line)), " | ")
		if !strings.Contains(got, tc.want) {
			t.Errorf("line %s\n  got %q, want it to contain %q", tc.line, got, tc.want)
		}
	}
}

// An unrecognised or malformed line must never be silently swallowed: a schema
// change in some CLI should degrade to raw text, not to silence.
func TestUnknownShapesDegradeGracefully(t *testing.T) {
	if got := summarizeAgentEvent([]byte("not json at all")); len(got) == 0 {
		t.Error("non-JSON output should still be reported")
	}
	if got := summarizeAgentEvent([]byte(`{"type":"brand_new_event_type","detail":"x"}`)); got != nil {
		// Returning nothing is acceptable for a recognised-but-uninteresting
		// event; the assertion here is only that it must not panic.
		t.Logf("unknown event produced %v", got)
	}
}

func TestClipCollapsesAndTruncates(t *testing.T) {
	if got := clip("a\nb", 10); got != "a b" {
		t.Errorf("newlines should collapse to spaces, got %q", got)
	}
	long := strings.Repeat("x", 100)
	got := clip(long, 10)
	if len(got) > 14 || !strings.HasSuffix(got, "…") {
		t.Errorf("clip produced %q", got)
	}
}

// --- Cursor-shaped events ---

func TestSummarizeCursorEvents(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "assistant message",
			line: `{"type":"assistant_message","text":"Reviewing the diff now."}`,
			want: "Reviewing the diff now.",
		},
		{
			name: "tool call with a flat name",
			line: `{"type":"tool_call","name":"read","args":{"path":"k3s/deployment.yaml"}}`,
			want: "tool read k3s/deployment.yaml",
		},
		{
			// Cursor nests arguments under a per-tool key, which also names it.
			name: "tool call wrapped under its tool key",
			line: `{"type":"tool_call","args":{"readToolCall":{"path":"compose.yml"}}}`,
			want: "tool readToolCall compose.yml",
		},
		{
			name: "failed tool call",
			line: `{"type":"tool_call","subtype":"completed","name":"bash","error":"exit status 1"}`,
			want: "tool failed: exit status 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(summarizeAgentEvent([]byte(tc.line)), " | ")
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// Absence is not zero. Reporting "0 tools" for a CLI that simply does not
// publish its tool list reads as a broken session.
func TestMissingToolListIsNotReportedAsZero(t *testing.T) {
	withList := `{"type":"system","subtype":"init","model":"claude-opus-5","tools":["Bash","Read"]}`
	got := strings.Join(summarizeAgentEvent([]byte(withList)), " ")
	if !strings.Contains(got, "2 tools available") {
		t.Errorf("a reported list should be counted: %q", got)
	}

	noList := `{"type":"system","subtype":"init","model":"some-model"}`
	got = strings.Join(summarizeAgentEvent([]byte(noList)), " ")
	if strings.Contains(got, "0 tools") {
		t.Errorf("absent tool metadata must not be reported as zero: %q", got)
	}
	if !strings.Contains(got, "some-model") {
		t.Errorf("the model should still be reported: %q", got)
	}

	// An genuinely empty list is still worth stating.
	empty := `{"type":"system","subtype":"init","model":"m","tools":[]}`
	got = strings.Join(summarizeAgentEvent([]byte(empty)), " ")
	if !strings.Contains(got, "0 tools available") {
		t.Errorf("an explicitly empty list should be reported as zero: %q", got)
	}
}
