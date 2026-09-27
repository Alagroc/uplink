package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendAndReplaySeparatesLogsFromState(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Append(KindJob, map[string]string{"id": "job_1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(KindLog, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	st.Auditf("crew %q registered", "devbox")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	var kinds []string
	if err := st2.Replay(func(ev Event) { kinds = append(kinds, ev.Kind) }); err != nil {
		t.Fatal(err)
	}
	// Job output lives in its own file so replaying state stays cheap.
	for _, k := range kinds {
		if k == KindLog {
			t.Error("log lines must not be in the state log")
		}
	}
	if len(kinds) != 2 || kinds[0] != KindJob || kinds[1] != KindAudit {
		t.Errorf("replayed kinds = %v", kinds)
	}

	// Job output is persisted for after-the-fact inspection but deliberately not
	// replayed into memory, so it must live in its own file.
	data, err := os.ReadFile(filepath.Join(dir, "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("log line missing from logs.jsonl: %s", data)
	}
}

// A crash can leave a half-written final line; replay must not choke on it.
func TestReplaySkipsCorruptTrailingLine(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(KindJob, map[string]string{"id": "job_1"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"at":"2026-01-01T00:00:00Z","kind":"job","pay`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var count int
	if err := st2.Replay(func(Event) { count++ }); err != nil {
		t.Fatalf("replay should tolerate a truncated line: %v", err)
	}
	if count != 1 {
		t.Errorf("replayed %d events, want the 1 intact event", count)
	}
}

func TestReplayOfMissingLogIsEmpty(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Nothing written yet: replay must be a no-op, not an error.
	if err := st.Replay(func(Event) { t.Error("no events expected") }); err != nil {
		t.Fatal(err)
	}
}

// The event log records jobs, commands and answers: it is the audit trail, and
// must not be world-readable.
func TestStateFilesAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Append(KindJob, map[string]string{"id": "j"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir permissions = %o, want 700", perm)
	}
	for _, name := range []string{"events.jsonl", "logs.jsonl"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s permissions = %o, want 600", name, perm)
		}
	}
}

func TestPayloadRoundTrips(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	type job struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := st.Append(KindJob, job{ID: "job_1", Label: "migrate compose"}); err != nil {
		t.Fatal(err)
	}
	var got job
	if err := st.Replay(func(ev Event) {
		if err := json.Unmarshal(ev.Payload, &got); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got.ID != "job_1" || !strings.Contains(got.Label, "migrate") {
		t.Errorf("round trip lost data: %+v", got)
	}
}
