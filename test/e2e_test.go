// Package e2e drives the real uplink binary end to end: ground, a crew process,
// a job, and the ask_operator round trip through a real radio bridge.
//
// These tests use a scripted stand-in for the agent rather than a live AI CLI,
// so they are deterministic and free to run. The live-agent run is a separate,
// manual exercise documented in TESTING.md.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var uplinkBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "uplink-e2e-build-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	uplinkBin = filepath.Join(dir, "uplink")
	build := exec.Command("go", "build", "-o", uplinkBin, "../cmd/uplink")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building uplink:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// harness is a running ground plus one crew.
type harness struct {
	t         *testing.T
	url       string
	token     string // operator credential
	crewToken string // credential for the "e2e" crew
	home      string
	workdir   string
}

// freePort asks the kernel for an unused port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startHarness(t *testing.T, crewArgs ...string) *harness {
	t.Helper()
	home := t.TempDir()
	workdir := t.TempDir()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	url := "http://" + addr

	h := &harness{t: t, url: url, home: home, workdir: workdir}

	ground := exec.Command(uplinkBin, "ground", "--addr", addr, "--bell=false")
	ground.Env = append(os.Environ(), "UPLINK_HOME="+home)
	ground.Stderr = prefixWriter{t, "ground"}
	if err := ground.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ground.Process.Kill()
		_, _ = ground.Process.Wait()
	})

	h.waitHealthy()
	h.token = h.readToken()

	// Mint the crew credential after ground is already up, which exercises the
	// store's live reload: a token minted now must work without a restart.
	h.crewToken = h.mintCrewToken("e2e", "builder")

	args := append([]string{"crew", "--name", "e2e",
		"--ground", url, "--workdir", workdir, "--poll", "2s"}, crewArgs...)
	crew := exec.Command(uplinkBin, args...)
	crew.Env = append(os.Environ(), "UPLINK_HOME="+home, "UPLINK_TOKEN="+h.crewToken)
	crew.Stderr = prefixWriter{t, "crew"}
	if err := crew.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = crew.Process.Kill()
		_, _ = crew.Process.Wait()
	})

	h.waitForCrew()
	return h
}

type prefixWriter struct {
	t      *testing.T
	prefix string
}

func (w prefixWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.t.Logf("%s | %s", w.prefix, line)
		}
	}
	return len(p), nil
}

func (h *harness) waitHealthy() {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.url + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatal("ground never became healthy")
}

func (h *harness) readToken() string {
	h.t.Helper()
	path := filepath.Join(h.home, "token")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("ground never wrote a token to %s", path)
	return ""
}

// mintCrewToken issues a crew credential and returns the secret.
func (h *harness) mintCrewToken(name string, roles ...string) string {
	h.t.Helper()
	args := []string{"crew-token", "add", name}
	for _, role := range roles {
		args = append(args, "--role", role)
	}
	cmd := exec.Command(uplinkBin, args...)
	cmd.Env = append(os.Environ(), "UPLINK_HOME="+h.home)
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("minting a crew token failed: %v", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		h.t.Fatal("crew-token add printed no token on stdout")
	}
	return token
}

// call runs `uplink call` and returns its stdout.
func (h *harness) call(args ...string) string {
	h.t.Helper()
	out, err := h.tryCall(args...)
	if err != nil {
		h.t.Fatalf("uplink call %v failed: %v\n%s", args, err, out)
	}
	return out
}

func (h *harness) tryCall(args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command(uplinkBin, append([]string{"call", "--ground", h.url}, args...)...)
	cmd.Env = append(os.Environ(), "UPLINK_HOME="+h.home, "UPLINK_TOKEN="+h.token)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *harness) waitForCrew() {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := h.tryCall("list_crew"); err == nil && strings.Contains(out, "ONLINE") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatal("crew never came online")
}

// submit dispatches a job and returns its id.
func (h *harness) submit(payload map[string]any) string {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	out := h.call("submit_job", string(body))
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "job_") {
			return field
		}
	}
	h.t.Fatalf("no job id in submit output: %s", out)
	return ""
}

// waitForState polls job_status until the job reaches a terminal state.
func (h *harness) waitForState(jobID string, timeout time.Duration) string {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = h.call("job_status", fmt.Sprintf(`{"job_id":%q}`, jobID))
		for _, state := range []string{"done", "failed", "canceled"} {
			if strings.Contains(last, " "+state+" ") {
				return last
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("job %s did not finish within %s; last status:\n%s", jobID, timeout, last)
	return last
}

func (h *harness) logs(jobID string) string {
	h.t.Helper()
	return h.call("job_logs", fmt.Sprintf(`{"job_id":%q,"limit":500}`, jobID))
}

// --- tests ---

func TestCrewRegistersAndReportsItself(t *testing.T) {
	h := startHarness(t)
	out := h.call("list_crew")
	for _, want := range []string{"e2e", "ONLINE", "builder", h.workdir} {
		if !strings.Contains(out, want) {
			t.Errorf("list_crew missing %q:\n%s", want, out)
		}
	}
}

func TestExecJobStreamsOutputBack(t *testing.T) {
	h := startHarness(t)
	jobID := h.submit(map[string]any{
		"kind": "exec", "crew": "e2e",
		"command": "echo hello-from-crew; echo to-stderr >&2; uname -s",
	})

	status := h.waitForState(jobID, 30*time.Second)
	if !strings.Contains(status, "done") || !strings.Contains(status, "exit=0") {
		t.Fatalf("unexpected final status:\n%s", status)
	}

	logs := h.logs(jobID)
	for _, want := range []string{"hello-from-crew", "to-stderr", "Linux"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, "[stderr]") {
		t.Errorf("stderr should be labelled in the log view:\n%s", logs)
	}
}

func TestExecFailureReportsExitCode(t *testing.T) {
	h := startHarness(t)
	jobID := h.submit(map[string]any{"kind": "exec", "crew": "e2e", "command": "exit 7"})
	status := h.waitForState(jobID, 30*time.Second)
	if !strings.Contains(status, "failed") || !strings.Contains(status, "exit=7") {
		t.Fatalf("a failing command should surface its exit code:\n%s", status)
	}
}

func TestJobLogsTailIncrementally(t *testing.T) {
	h := startHarness(t)
	jobID := h.submit(map[string]any{"kind": "exec", "crew": "e2e", "command": "echo one; echo two; echo three"})
	h.waitForState(jobID, 30*time.Second)

	first := h.call("job_logs", fmt.Sprintf(`{"job_id":%q,"limit":1,"since_seq":0}`, jobID))
	if !strings.Contains(first, "next_seq=") {
		t.Fatalf("job_logs must report next_seq so the operator can tail:\n%s", first)
	}
	// Tailing from the reported cursor must not repeat what was already read.
	var next int
	fmt.Sscanf(first[strings.Index(first, "next_seq=")+len("next_seq="):], "%d", &next)
	tail := h.call("job_logs", fmt.Sprintf(`{"job_id":%q,"since_seq":%d}`, jobID, next))
	if strings.Contains(tail, "one") {
		t.Errorf("tail from next_seq repeated earlier output:\n%s", tail)
	}
}

// await_job replaces polling: one call that sleeps until the job is done.
func TestAwaitJobBlocksUntilTheJobFinishes(t *testing.T) {
	h := startHarness(t)
	jobID := h.submit(map[string]any{
		"kind": "exec", "crew": "e2e",
		"command": "echo starting; sleep 3; echo finished",
	})

	start := time.Now()
	out := h.call("await_job", fmt.Sprintf(`{"job_id":%q,"timeout_s":60}`, jobID))
	elapsed := time.Since(start)

	if !strings.Contains(out, "Job finished") {
		t.Fatalf("await_job did not report completion:\n%s", out)
	}
	// It must actually have waited, rather than returning a snapshot.
	if elapsed < 2*time.Second {
		t.Errorf("returned after %s; it should have blocked until the job ended", elapsed)
	}
	if elapsed > 30*time.Second {
		t.Errorf("took %s; it should have woken on completion, not timed out", elapsed)
	}
	for _, want := range []string{"starting", "finished", "next_seq="} {
		if !strings.Contains(out, want) {
			t.Errorf("await_job output missing %q:\n%s", want, out)
		}
	}
}

// Waiting on work that is already over must return at once.
func TestAwaitJobReturnsImmediatelyForAFinishedJob(t *testing.T) {
	h := startHarness(t)
	jobID := h.submit(map[string]any{"kind": "exec", "crew": "e2e", "command": "true"})
	h.waitForState(jobID, 30*time.Second)

	start := time.Now()
	out := h.call("await_job", fmt.Sprintf(`{"job_id":%q,"timeout_s":60}`, jobID))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a finished job took %s to report", elapsed)
	}
	if !strings.Contains(out, "nothing more will arrive") {
		t.Errorf("should say the job is over:\n%s", out)
	}
}

func TestCancelStopsALongJob(t *testing.T) {
	h := startHarness(t)
	jobID := h.submit(map[string]any{"kind": "exec", "crew": "e2e", "command": "sleep 300"})

	// Wait until it is actually running before cancelling.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.call("job_status", fmt.Sprintf(`{"job_id":%q}`, jobID)), "running") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	h.call("cancel_job", fmt.Sprintf(`{"job_id":%q,"reason":"test"}`, jobID))
	status := h.waitForState(jobID, 30*time.Second)
	if !strings.Contains(status, "canceled") {
		t.Fatalf("job should be canceled:\n%s", status)
	}
}

func TestWorkdirEscapeIsRejected(t *testing.T) {
	h := startHarness(t)
	jobID := h.submit(map[string]any{"kind": "exec", "crew": "e2e", "command": "pwd", "workdir": "../.."})
	status := h.waitForState(jobID, 30*time.Second)
	if !strings.Contains(status, "failed") {
		t.Fatalf("a workdir outside the crew root must fail:\n%s", status)
	}
	if !strings.Contains(status, "outside the crew workdir") {
		t.Errorf("the failure should say why:\n%s", status)
	}
}

func TestSubmitToUnknownCrewFailsHelpfully(t *testing.T) {
	h := startHarness(t)
	out, err := h.tryCall("submit_job", `{"kind":"exec","crew":"ghost","command":"true"}`)
	if err == nil {
		t.Fatal("expected an error for an unknown crew")
	}
	if !strings.Contains(out, "e2e") {
		t.Errorf("the error should list the crew that do exist:\n%s", out)
	}
}

// The operator token must not open the agent channel, and vice versa: a remote
// agent that goes rogue must not be able to dispatch jobs.
func TestEndpointsEnforceSeparateCredentials(t *testing.T) {
	h := startHarness(t)

	// Operator token on the agent endpoint.
	req, err := http.NewRequest(http.MethodPost, h.url+"/mcp/agent",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the operator token should not open the agent endpoint, got %d", resp.StatusCode)
	}

	// No token at all on the operator endpoint.
	resp2, err := http.Post(h.url+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("the operator endpoint must require a token, got %d", resp2.StatusCode)
	}
}

// The credential cutover, end to end: a crew handed the operator token must be
// refused, and told what to do instead.
func TestCrewWithOperatorTokenIsRefused(t *testing.T) {
	h := startHarness(t)

	// A rejected crew retries with backoff rather than exiting, which is right
	// for a dropped tunnel but means this has to be time-boxed.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, uplinkBin, "crew", "--name", "impostor",
		"--ground", h.url, "--workdir", h.workdir, "--poll", "1s")
	cmd.Env = append(os.Environ(), "UPLINK_HOME="+h.home, "UPLINK_TOKEN="+h.token)
	out, _ := cmd.CombinedOutput()
	text := string(out)

	if !strings.Contains(text, "crew-token add") {
		t.Errorf("a crew using the operator token should be told to mint one:\n%s", text)
	}
	// And it must not have registered.
	if listed := h.call("list_crew"); strings.Contains(listed, "impostor") {
		t.Errorf("the impostor crew registered anyway:\n%s", listed)
	}
}

func TestOperatorToolsAreAllAdvertised(t *testing.T) {
	h := startHarness(t)
	out := h.call("--tools")
	for _, tool := range []string{"list_crew", "submit_job", "await_job", "job_status", "job_logs", "cancel_job", "inbox", "reply", "send_message"} {
		if !strings.Contains(out, tool) {
			t.Errorf("tool %q is not advertised:\n%s", tool, out)
		}
	}
}
