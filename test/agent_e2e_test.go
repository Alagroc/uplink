package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptedAgent stands in for a real AI CLI. It is a genuine MCP client: it
// spawns `uplink radio` and speaks JSON-RPC to it over stdio, exactly as
// claude/codex/cursor-agent do. That makes the round trip under test real —
// only the reasoning is scripted.
const scriptedAgent = `#!/usr/bin/env python3
import json, os, subprocess, sys

prompt = sys.argv[1] if len(sys.argv) > 1 else ""
radio = subprocess.Popen(
    [os.environ["UPLINK_BIN"], "radio", "--ground", os.environ["UPLINK_GROUND"]],
    stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)

next_id = [0]
def rpc(method, params=None):
    next_id[0] += 1
    msg = {"jsonrpc": "2.0", "id": next_id[0], "method": method}
    if params is not None:
        msg["params"] = params
    radio.stdin.write(json.dumps(msg) + "\n")
    radio.stdin.flush()
    line = radio.stdout.readline()
    if not line:
        raise SystemExit("radio closed the connection")
    return json.loads(line)

def tool(name, args):
    resp = rpc("tools/call", {"name": name, "arguments": args})
    if "error" in resp:
        raise SystemExit("rpc error: %s" % resp["error"])
    result = resp["result"]
    text = "".join(c.get("text", "") for c in result.get("content", []))
    return text, result.get("isError", False)

init = rpc("initialize", {"protocolVersion": "2025-06-18",
                          "capabilities": {},
                          "clientInfo": {"name": "scripted-agent", "version": "1"}})
print("INIT_OK instructions=%s" % bool(init["result"].get("instructions")))

listed = rpc("tools/list")
names = sorted(t["name"] for t in listed["result"]["tools"])
print("TOOLS %s" % ",".join(names))

tool("report_progress", {"text": "starting on: %s" % prompt, "phase": "start"})

answer, is_error = tool("ask_operator", {
    "question": "Should the scratch volume become a PVC or a hostPath mount?",
    "context": "docker-compose.yml bind-mounts /mnt/scratch; the k3s node has local-path installed",
    "options": ["PVC via local-path", "hostPath"],
    "urgency": "high",
    "timeout_s": 120,
})
if is_error:
    raise SystemExit("ask_operator failed: %s" % answer)
print("ANSWER_RECEIVED %s" % answer)

msgs, _ = tool("check_messages", {})
print("MESSAGES %s" % msgs.replace("\n", " ~ "))

tool("task_complete", {"summary": "applied the operator's decision: %s" % answer})
print("DONE")

radio.stdin.close()
radio.wait(timeout=10)
`

// writeScriptedRunner installs the stand-in agent as a runner named "scripted".
func writeScriptedRunner(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "scripted_agent.py")
	if err := os.WriteFile(script, []byte(scriptedAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	runners := map[string]any{
		"scripted": map[string]any{
			"command":     "python3",
			"args":        []string{script, "{{prompt}}"},
			"mcp_style":   "none",
			"stream_json": false,
			"env":         map[string]string{"UPLINK_BIN": uplinkBin},
		},
	}
	data, err := json.MarshalIndent(runners, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runners.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitForQuestion polls the inbox until a question is waiting, and returns its id.
func (h *harness) waitForQuestion(timeout time.Duration) (id, body string) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = h.call("inbox")
		if strings.Contains(last, "WAITING") {
			for _, field := range strings.Fields(last) {
				if strings.HasPrefix(field, "q_") {
					return field, last
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	h.t.Fatalf("no question arrived within %s; last inbox:\n%s", timeout, last)
	return "", ""
}

// This is the test the whole design exists for: a remote agent blocks on a
// question, the operator answers from their own machine, and the agent resumes
// with its context intact.
func TestAgentAsksOperatorAndResumes(t *testing.T) {
	dir := t.TempDir()
	runners := writeScriptedRunner(t, dir)
	h := startHarness(t, "--runners", runners, "--runner", "scripted")

	jobID := h.submit(map[string]any{
		"kind":   "agent",
		"crew":   "e2e",
		"runner": "scripted",
		"prompt": "migrate the compose stack to k3s",
		"label":  "migration",
	})

	// The agent should reach ask_operator and park there.
	qID, inbox := h.waitForQuestion(60 * time.Second)
	for _, want := range []string{
		"Should the scratch volume become a PVC or a hostPath mount?",
		"local-path installed",
		"PVC via local-path | hostPath",
		"urgency=high",
		"crew=e2e",
	} {
		if !strings.Contains(inbox, want) {
			t.Errorf("inbox missing %q:\n%s", want, inbox)
		}
	}

	// While it waits, the operator queues an unprompted instruction too.
	h.call("send_message", fmt.Sprintf(`{"job_id":%q,"text":"also skip the redis service"}`, jobID))

	// The job must still be running, not finished: the agent is blocked.
	if status := h.call("job_status", fmt.Sprintf(`{"job_id":%q}`, jobID)); !strings.Contains(status, "running") {
		t.Errorf("the job should still be running while its agent waits:\n%s", status)
	}

	answer := "PVC via local-path; keep it 10Gi and ReadWriteOnce"
	h.call("reply", fmt.Sprintf(`{"question_id":%q,"answer":%q}`, qID, answer))

	status := h.waitForState(jobID, 90*time.Second)
	if !strings.Contains(status, "done") {
		t.Fatalf("job did not complete after the reply:\n%s", status)
	}

	logs := h.logs(jobID)
	checks := map[string]string{
		"INIT_OK instructions=True": "the agent must receive its briefing on initialize",
		// The answer is labelled so the agent knows it came from a human, and
		// arrives verbatim after that label.
		"ANSWER_RECEIVED Operator answered: " + answer: "the operator's answer must reach the blocked agent verbatim",
		"also skip the redis service":                  "a queued operator message must ride along on the next tool call",
		"DONE":                                         "the agent must resume and finish after being unblocked",
	}
	for needle, why := range checks {
		if !strings.Contains(logs, needle) {
			t.Errorf("%s (missing %q)\nlogs:\n%s", why, needle, logs)
		}
	}

	// The agent's own tools must be exactly the channel tools, and nothing that
	// would let it dispatch jobs.
	if !strings.Contains(logs, "TOOLS ask_operator,check_messages,report_progress,task_complete") {
		t.Errorf("unexpected agent tool set:\n%s", logs)
	}
	toolsLine := ""
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(line, "TOOLS ") {
			toolsLine = line
		}
	}
	for _, forbidden := range []string{"submit_job", "list_crew", "cancel_job", "reply"} {
		if strings.Contains(toolsLine, forbidden) {
			t.Errorf("agent must not be offered the operator tool %q; its tools were: %s", forbidden, toolsLine)
		}
	}

	// report_progress and task_complete must be visible to the operator.
	if !strings.Contains(logs, "progress: [start] starting on: migrate the compose stack to k3s") {
		t.Errorf("progress report missing from the job log:\n%s", logs)
	}
	if !strings.Contains(status, "agent summary: applied the operator's decision") {
		t.Errorf("task_complete summary missing from job_status:\n%s", status)
	}

	// The answered question should no longer be pending.
	if after := h.call("inbox"); strings.Contains(after, "WAITING") {
		t.Errorf("inbox should be clear after the reply:\n%s", after)
	}
}

// An agent left waiting must eventually be released with usable guidance rather
// than hanging forever.
func TestAgentAskTimesOutWhenNobodyAnswers(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "impatient.py")
	body := `#!/usr/bin/env python3
import json, os, subprocess
radio = subprocess.Popen([os.environ["UPLINK_BIN"], "radio", "--ground", os.environ["UPLINK_GROUND"]],
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
def rpc(method, params=None):
    msg = {"jsonrpc":"2.0","id":1,"method":method}
    if params is not None: msg["params"] = params
    radio.stdin.write(json.dumps(msg)+"\n"); radio.stdin.flush()
    return json.loads(radio.stdout.readline())
rpc("initialize", {"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}})
r = rpc("tools/call", {"name":"ask_operator","arguments":{"question":"anyone there?","timeout_s":2}})
text = "".join(c.get("text","") for c in r["result"].get("content",[]))
print("IS_ERROR=%s" % r["result"].get("isError"))
print("GUIDANCE=%s" % text.replace("\n"," "))
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	runners := map[string]any{"impatient": map[string]any{
		"command": "python3", "args": []string{script}, "mcp_style": "none",
		"env": map[string]string{"UPLINK_BIN": uplinkBin},
	}}
	data, _ := json.MarshalIndent(runners, "", "  ")
	path := filepath.Join(dir, "runners.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	h := startHarness(t, "--runners", path, "--runner", "impatient")
	jobID := h.submit(map[string]any{"kind": "agent", "crew": "e2e", "runner": "impatient", "prompt": "ask and give up"})
	h.waitForState(jobID, 60*time.Second)

	logs := h.logs(jobID)
	if !strings.Contains(logs, "IS_ERROR=True") {
		t.Errorf("an unanswered question should come back as a tool error:\n%s", logs)
	}
	// The guidance matters: the agent needs to know what to do next.
	if !strings.Contains(logs, "reversible") || !strings.Contains(logs, "task_complete") {
		t.Errorf("timeout guidance should tell the agent how to proceed:\n%s", logs)
	}
}

// Cancelling a job must release an agent that is blocked on a question, rather
// than leaving the process parked until its timeout.
func TestCancelReleasesABlockedAgent(t *testing.T) {
	dir := t.TempDir()
	runners := writeScriptedRunner(t, dir)
	h := startHarness(t, "--runners", runners, "--runner", "scripted")

	jobID := h.submit(map[string]any{"kind": "agent", "crew": "e2e", "runner": "scripted", "prompt": "will be cancelled"})
	h.waitForQuestion(60 * time.Second)

	h.call("cancel_job", fmt.Sprintf(`{"job_id":%q,"reason":"operator changed plan"}`, jobID))
	status := h.waitForState(jobID, 60*time.Second)
	if !strings.Contains(status, "canceled") {
		t.Fatalf("job should end up canceled:\n%s", status)
	}

	// And the question must not linger in the inbox for a job that is gone.
	if inbox := h.call("inbox"); strings.Contains(inbox, "WAITING") {
		t.Errorf("cancelling a job should clear its pending question:\n%s", inbox)
	}
}
