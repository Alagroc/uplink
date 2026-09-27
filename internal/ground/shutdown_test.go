package ground

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
	"github.com/Alagroc/uplink/internal/store"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server, chan string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	crewTokens, err := OpenCrewTokens(filepath.Join(dir, "crew-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}

	g := New(st, Options{OfflineAfter: time.Minute}, nil)
	stopped := make(chan string, 1)
	srv := &Server{
		Ground:     g,
		Token:      "operator-token",
		Version:    "test",
		CrewTokens: crewTokens,
		RequestShutdown: func(reason string) {
			select {
			case stopped <- reason:
			default:
			}
		},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, stopped
}

func postShutdown(t *testing.T, base, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/shutdown", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(payload)
}

func TestShutdownWhenIdle(t *testing.T) {
	_, ts, stopped := newTestServer(t)

	status, body := postShutdown(t, ts.URL, "operator-token", `{}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	select {
	case reason := <-stopped:
		if reason == "" {
			t.Error("a reason should always be recorded for the audit log")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown was accepted but never actually requested")
	}
}

// Losing ground mid-job costs work — a running job is marked failed on the next
// start — so the default must refuse and say what is in flight.
func TestShutdownRefusesWhileBusy(t *testing.T) {
	srv, ts, stopped := newTestServer(t)
	crewID := register(t, srv.Ground, "devbox")
	job, err := srv.Ground.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "sleep 100"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Ground.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}

	status, body := postShutdown(t, ts.URL, "operator-token", `{}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
	if !strings.Contains(body, job.ID) {
		t.Errorf("the refusal should name the running job: %s", body)
	}
	select {
	case <-stopped:
		t.Fatal("ground must not shut down when it reported refusing to")
	case <-time.After(200 * time.Millisecond):
	}

	// force overrides, and reports what it abandoned.
	status, body = postShutdown(t, ts.URL, "operator-token", `{"force":true,"reason":"operator insisted"}`)
	if status != http.StatusOK {
		t.Fatalf("force status = %d: %s", status, body)
	}
	var out struct {
		AbandonedJobs []string `json:"abandoned_jobs"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.AbandonedJobs) != 1 || !strings.Contains(out.AbandonedJobs[0], job.ID) {
		t.Errorf("force should report what it abandoned, got %+v", out.AbandonedJobs)
	}
	select {
	case reason := <-stopped:
		if reason != "operator insisted" {
			t.Errorf("reason = %q, want it passed through", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("force did not trigger shutdown")
	}
}

// An agent blocked on a question is work in flight too: shutting down would
// leave it without an answer.
func TestShutdownRefusesWhileAnAgentWaits(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	register(t, srv.Ground, "devbox")
	job, _ := srv.Ground.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	if _, err := srv.Ground.Ask(*job, proto.AskReq{Question: "which storage class?"}); err != nil {
		t.Fatal(err)
	}

	status, body := postShutdown(t, ts.URL, "operator-token", `{}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", status, body)
	}
	if !strings.Contains(body, "pending_question") {
		t.Errorf("the refusal should mention the waiting agent: %s", body)
	}
}

func TestShutdownNeedsTheOperatorToken(t *testing.T) {
	_, ts, stopped := newTestServer(t)
	status, _ := postShutdown(t, ts.URL, "wrong-token", `{}`)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	select {
	case <-stopped:
		t.Fatal("an unauthenticated request must never stop ground")
	case <-time.After(200 * time.Millisecond):
	}
}

// Health is unauthenticated on purpose: a crew checks it before it has a token,
// and `uplink shutdown` uses it to tell "nothing there" apart from "something
// else owns this port".
func TestHealthIdentifiesUplink(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health["status"] != "ok" || health["protocol"] != proto.Version {
		t.Errorf("health payload = %+v, want status ok and a protocol version", health)
	}
}
