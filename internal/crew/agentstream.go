package crew

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// summarizeAgentEvent turns one agent transcript event into zero or more short
// operator-facing lines.
//
// The CLIs do not agree on a schema and each changes theirs between versions,
// so this reads defensively: recognised shapes are summarised, anything else
// falls back to a compact rendering rather than being dropped. The full raw
// stream is always kept on the crew host.
func summarizeAgentEvent(line []byte) []string {
	var ev map[string]any
	if err := json.Unmarshal(line, &ev); err != nil {
		return []string{clip(string(line), 300)}
	}

	switch str(ev["type"]) {
	case "system":
		if str(ev["subtype"]) == "init" {
			model := str(ev["model"])
			tools := countOf(ev["tools"])
			return []string{fmt.Sprintf("session started (model=%s, %d tools available)", orDash(model), tools)}
		}
		return nil

	case "assistant":
		return summarizeMessage(ev["message"], "")

	case "user":
		// Tool results: report only failures. Successes are usually noise, and
		// the operator can read the raw transcript when they want detail.
		return summarizeToolResults(ev["message"])

	case "result":
		status := str(ev["subtype"])
		if status == "" {
			status = "finished"
		}
		cost := ev["total_cost_usd"]
		turns := ev["num_turns"]
		parts := []string{"session " + status}
		if turns != nil {
			parts = append(parts, fmt.Sprintf("%v turns", turns))
		}
		if cost != nil {
			parts = append(parts, fmt.Sprintf("$%v", cost))
		}
		out := []string{strings.Join(parts, ", ")}
		if text := str(ev["result"]); text != "" {
			out = append(out, "final: "+clip(text, 2000))
		}
		return out

	// Codex-style events.
	case "item.completed", "item.started":
		return summarizeCodexItem(ev)
	case "thread.started":
		return []string{"session started"}
	case "turn.completed", "turn.started":
		return nil
	case "error":
		return []string{"error: " + clip(firstNonEmpty(str(ev["message"]), compact(ev)), 500)}
	}

	// Unknown shape: keep something rather than nothing.
	if msg := ev["message"]; msg != nil {
		if out := summarizeMessage(msg, ""); len(out) > 0 {
			return out
		}
	}
	return nil
}

func summarizeCodexItem(ev map[string]any) []string {
	item, ok := ev["item"].(map[string]any)
	if !ok {
		return nil
	}
	switch str(item["item_type"]) {
	case "assistant_message", "agent_message":
		if text := str(item["text"]); text != "" {
			return []string{"says: " + clip(text, 1200)}
		}
	case "command_execution":
		cmd := clip(str(item["command"]), 300)
		if code, ok := item["exit_code"]; ok && str(ev["type"]) == "item.completed" {
			return []string{fmt.Sprintf("ran: %s (exit %v)", cmd, code)}
		}
		return []string{"ran: " + cmd}
	case "file_change", "patch_apply":
		return []string{"edited files: " + clip(compact(item["changes"]), 300)}
	case "mcp_tool_call":
		return []string{fmt.Sprintf("tool %s.%s", str(item["server"]), str(item["tool"]))}
	case "reasoning":
		return nil
	}
	return nil
}

// summarizeMessage renders an Anthropic-style message content array.
func summarizeMessage(raw any, prefix string) []string {
	msg, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	content, ok := msg["content"].([]any)
	if !ok {
		if text := str(msg["content"]); text != "" {
			return []string{prefix + "says: " + clip(text, 1200)}
		}
		return nil
	}

	var out []string
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch str(block["type"]) {
		case "text":
			if text := strings.TrimSpace(str(block["text"])); text != "" {
				out = append(out, prefix+"says: "+clip(text, 1200))
			}
		case "tool_use":
			out = append(out, prefix+"tool "+str(block["name"])+" "+describeToolInput(block["input"]))
		case "thinking":
			// Deliberately not shipped: it is long, and the operator asked for
			// status, not the agent's inner monologue.
		}
	}
	return out
}

// summarizeToolResults reports failed tool results only.
func summarizeToolResults(raw any) []string {
	msg, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok || str(block["type"]) != "tool_result" {
			continue
		}
		isErr, _ := block["is_error"].(bool)
		if !isErr {
			continue
		}
		out = append(out, "tool failed: "+clip(textOf(block["content"]), 600))
	}
	return out
}

// describeToolInput picks the one field that tells the operator what happened.
func describeToolInput(raw any) string {
	input, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "url", "question", "text", "summary"} {
		if v := str(input[key]); v != "" {
			return clip(v, 300)
		}
	}
	return clip(compact(input), 200)
}

func textOf(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if text := str(block["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return compact(raw)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func countOf(v any) int {
	if list, ok := v.([]any); ok {
		return len(list)
	}
	return 0
}

func compact(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

// clip collapses a value onto one line and truncates it. Cutting on a rune
// boundary matters: a byte-offset cut can split a multi-byte character and emit
// invalid UTF-8, which then becomes U+FFFD in the operator's log.
func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " …"
}
