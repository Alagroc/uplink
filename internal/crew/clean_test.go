package crew

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
	"github.com/Alagroc/uplink/internal/runner"
)

// fakeGround records what a crew asks for, and forces one reconnect.
type fakeGround struct {
	mu          sync.Mutex
	cleanFlags  []bool // one entry per register call
	pollsServed int
}

func (f *fakeGround) snapshot() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.cleanFlags...)
}

func (f *fakeGround) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "protocol": proto.Version})
	})
	mux.HandleFunc("/v1/crew/register", func(w http.ResponseWriter, r *http.Request) {
		var req proto.RegisterReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.cleanFlags = append(f.cleanFlags, req.Clean)
		n := len(f.cleanFlags)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(proto.RegisterResp{
			CrewID:        "crew_fake",
			GroundVersion: proto.Version,
		})
		_ = n
	})
	mux.HandleFunc("/v1/crew/poll", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.pollsServed++
		served := f.pollsServed
		f.mu.Unlock()
		if served == 1 {
			// Ground has forgotten this crew: it must re-register.
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown crew"})
			return
		}
		_ = json.NewEncoder(w).Encode(proto.Command{Type: proto.CmdNoop})
	})
	return mux
}

// A reconnect must never ask ground to discard work: by then this process
// really does have jobs running, and throwing them away would be the opposite
// of the resilience a reconnect exists for.
func TestCleanIsSentOnlyOnTheFirstRegistration(t *testing.T) {
	fake := &fakeGround{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := New(Config{
		Name:      "devbox",
		GroundURL: srv.URL,
		Token:     "uplc_test",
		Workdir:   t.TempDir(),
		Runners:   runner.Defaults(),
		StateDir:  t.TempDir(),
		PollWait:  100 * time.Millisecond,
		Clean:     true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	// Wait until it has registered twice: once on start, once after the 409.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && len(fake.snapshot()) < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	flags := fake.snapshot()
	if len(flags) < 2 {
		t.Fatalf("expected at least two registrations, saw %d", len(flags))
	}
	if !flags[0] {
		t.Error("the first registration should have asked for a clean start")
	}
	for i, clean := range flags[1:] {
		if clean {
			t.Errorf("re-registration %d asked for a clean start; it must not", i+2)
		}
	}
}

// Without the flag, no registration asks to discard anything.
func TestCleanIsNeverSentWhenNotRequested(t *testing.T) {
	fake := &fakeGround{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := New(Config{
		Name: "devbox", GroundURL: srv.URL, Token: "uplc_test",
		Workdir: t.TempDir(), Runners: runner.Defaults(), StateDir: t.TempDir(),
		PollWait: 100 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && len(fake.snapshot()) < 1 {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	for i, clean := range fake.snapshot() {
		if clean {
			t.Errorf("registration %d asked for a clean start without --clean", i+1)
		}
	}
}

// Generated per-job files leak whenever a crew is killed before cleanup runs.
// Transcripts must survive: they are the record of what an agent actually did.
func TestPruneStaleFilesKeepsTranscripts(t *testing.T) {
	stateDir := t.TempDir()
	mcpDir := filepath.Join(stateDir, "mcp")
	transcriptDir := filepath.Join(stateDir, "transcripts")
	for _, d := range []string{mcpDir, transcriptDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stale := []string{"mcp-123.json", "job-token-456"}
	for _, name := range stale {
		if err := os.WriteFile(filepath.Join(mcpDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(mcpDir, "notes.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(transcriptDir, "job_abc.jsonl")
	if err := os.WriteFile(transcript, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := testCrew(t, t.TempDir())
	c.cfg.StateDir = stateDir

	if removed := c.pruneStaleFiles(); removed != 2 {
		t.Errorf("removed %d files, want 2", removed)
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(mcpDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", name)
		}
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Error("transcripts must be left alone: they are the record of what ran")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("unrelated files must be left alone")
	}
}

func TestPruneStaleFilesOnAFreshHost(t *testing.T) {
	c := testCrew(t, t.TempDir())
	c.cfg.StateDir = filepath.Join(t.TempDir(), "never-used")
	if removed := c.pruneStaleFiles(); removed != 0 {
		t.Errorf("removed %d from a nonexistent state dir", removed)
	}
}
