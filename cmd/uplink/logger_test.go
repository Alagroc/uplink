package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// withCapturedLog redirects log output and restores the package state after.
func withCapturedLog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prevOut, prevDay, prevNow, prevTS := logOut, lastLogDay, logNow, logTimestamps
	logOut, lastLogDay, logTimestamps = &buf, "", true
	t.Cleanup(func() {
		logOut, lastLogDay, logNow, logTimestamps = prevOut, prevDay, prevNow, prevTS
	})
	return &buf
}

func TestLoggerStampsEachLine(t *testing.T) {
	buf := withCapturedLog(t)
	logNow = func() time.Time { return time.Date(2026, 9, 29, 14, 5, 3, 250*int(time.Millisecond), time.UTC) }

	logger("ground")("crew %q registered", "devbox")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a date marker then the line, got %q", buf.String())
	}
	if lines[0] != "--- 2026-09-29 ---" {
		t.Errorf("date marker = %q", lines[0])
	}
	if lines[1] != `14:05:03.250 [ground] crew "devbox" registered` {
		t.Errorf("line = %q", lines[1])
	}
}

// The date is printed only when it changes, so a log spanning days stays
// unambiguous without every line carrying a full date.
func TestLoggerMarksTheDateOnlyWhenItRolls(t *testing.T) {
	buf := withCapturedLog(t)
	now := time.Date(2026, 9, 29, 23, 59, 59, 0, time.UTC)
	logNow = func() time.Time { return now }

	log := logger("ground")
	log("before midnight")
	log("still the same day")
	now = now.Add(2 * time.Second) // over the boundary
	log("after midnight")

	out := buf.String()
	if got := strings.Count(out, "--- 2026-09-29 ---"); got != 1 {
		t.Errorf("the first day should be marked exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "--- 2026-09-30 ---"); got != 1 {
		t.Errorf("the rollover should be marked once, got %d:\n%s", got, out)
	}
	if strings.Index(out, "--- 2026-09-30 ---") > strings.Index(out, "after midnight") {
		t.Error("the marker must come before the line it applies to")
	}
}

// Loggers share the rollover state, so two components do not each announce the
// date.
func TestLoggersShareTheDateMarker(t *testing.T) {
	buf := withCapturedLog(t)
	logNow = func() time.Time { return time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC) }

	logger("ground")("from ground")
	logger("crew")("from crew")

	if got := strings.Count(buf.String(), "--- 2026-09-29 ---"); got != 1 {
		t.Errorf("the date should be announced once across components, got %d:\n%s", got, buf.String())
	}
}

// Something else may already be stamping the stream — journald, a supervisor.
func TestTimestampsCanBeTurnedOff(t *testing.T) {
	buf := withCapturedLog(t)
	logTimestamps = false

	logger("ground")("listening")

	if got := buf.String(); got != "[ground] listening\n" {
		t.Errorf("got %q", got)
	}
}

// A message carrying a stray %s must not be re-interpreted as a format string:
// crew names and agent questions reach this logger.
func TestLoggerDoesNotReformatItsMessage(t *testing.T) {
	buf := withCapturedLog(t)
	logNow = func() time.Time { return time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC) }

	logger("ground")("%s", "devbox is waiting: why 100% CPU and a %s here?")

	if !strings.Contains(buf.String(), "why 100% CPU and a %s here?") {
		t.Errorf("message was mangled: %q", buf.String())
	}
	if strings.Contains(buf.String(), "%!") {
		t.Errorf("format verb leaked into the output: %q", buf.String())
	}
}

// Ground logs one line per request from many goroutines at once.
func TestConcurrentLoggingDoesNotInterleave(t *testing.T) {
	buf := withCapturedLog(t)
	logNow = func() time.Time { return time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC) }
	log := logger("ground")

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log("line-%02d-endmarker", i)
		}()
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.HasPrefix(line, "---") {
			continue
		}
		if !strings.HasSuffix(line, "endmarker") {
			t.Fatalf("line was torn by a concurrent write: %q", line)
		}
	}
}
