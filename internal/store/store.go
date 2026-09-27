// Package store provides ground's append-only event log. It doubles as the
// audit trail: every job, command, question and answer lands here, and replay
// on start-up restores jobs and pending questions after a restart.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event kinds written to the log.
const (
	KindCrew     = "crew"
	KindJob      = "job"
	KindLog      = "log"
	KindQuestion = "question"
	KindMessage  = "message"
	KindAudit    = "audit"
)

// Event is one line in the log.
type Event struct {
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// Store appends events to disk. It is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	events *os.File
	logs   *os.File
	dir    string
}

// Open creates dir if needed and opens the log files for appending.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	events, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	logs, err := os.OpenFile(filepath.Join(dir, "logs.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		events.Close()
		return nil, err
	}
	return &Store{events: events, logs: logs, dir: dir}, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// Append records one event. Log lines go to a separate file so that replaying
// state does not have to wade through job output.
func (s *Store) Append(kind string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	line, err := json.Marshal(Event{At: time.Now().UTC(), Kind: kind, Payload: data})
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.events
	if kind == KindLog {
		f = s.logs
	}
	_, err = f.Write(line)
	return err
}

// Auditf records a human-readable audit note.
func (s *Store) Auditf(format string, args ...any) {
	_ = s.Append(KindAudit, map[string]string{"note": fmt.Sprintf(format, args...)})
}

// Replay walks the state event log oldest-first, calling fn for each event. A
// corrupt trailing line (from a crash mid-write) is skipped rather than fatal.
//
// Job output in logs.jsonl is deliberately not replayed: it is kept for
// after-the-fact inspection, not to rebuild in-memory state.
func (s *Store) Replay(fn func(ev Event)) error {
	return replayFile(filepath.Join(s.dir, "events.jsonl"), fn)
}

func replayFile(path string, fn func(ev Event)) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // partial write from a crash; ignore
		}
		fn(ev)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Close flushes and closes the log files.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err1 := s.events.Close()
	err2 := s.logs.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
