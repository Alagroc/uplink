package crew

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/runner"
)

func testCrew(t *testing.T, workdir string) *Crew {
	t.Helper()
	c, err := New(Config{
		Name:      "test",
		GroundURL: "http://127.0.0.1:1",
		Token:     "tok",
		Workdir:   workdir,
		Runners:   runner.Defaults(),
		StateDir:  t.TempDir(),
		PollWait:  time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResolveWorkdirAcceptsSubdirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "stack", "k3s"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := testCrew(t, root)

	got, err := c.resolveWorkdir("stack/k3s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join("stack", "k3s")) {
		t.Errorf("resolved to %q", got)
	}

	// An empty workdir means the crew root.
	if got, err := c.resolveWorkdir(""); err != nil || got != root {
		t.Errorf("empty workdir should be the crew root, got %q %v", got, err)
	}

	// An absolute path inside the root is fine.
	if _, err := c.resolveWorkdir(filepath.Join(root, "stack")); err != nil {
		t.Errorf("an absolute path inside the root should be allowed: %v", err)
	}
}

// A prompt must not be able to walk the agent out of the tree the operator
// pointed it at.
func TestResolveWorkdirRejectsEscapes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := testCrew(t, root)

	for _, bad := range []string{"..", "../..", "/etc", "../sibling"} {
		if _, err := c.resolveWorkdir(bad); err == nil {
			t.Errorf("workdir %q should be rejected as outside the crew workdir", bad)
		}
	}
}

func TestResolveWorkdirRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	outside := filepath.Join(base, "secrets")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink inside the root pointing out of it must not widen the boundary.
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	c := testCrew(t, root)
	if _, err := c.resolveWorkdir("escape"); err == nil {
		t.Error("a symlink out of the crew workdir should be rejected")
	}
}

func TestResolveWorkdirRejectsMissingDirectory(t *testing.T) {
	c := testCrew(t, t.TempDir())
	if _, err := c.resolveWorkdir("nope"); err == nil {
		t.Error("a nonexistent workdir should fail before the job starts")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{Token: "t"}, nil); err == nil {
		t.Error("a crew needs a name")
	}
	if _, err := New(Config{Name: "n"}, nil); err == nil {
		t.Error("a crew needs a token")
	}
}
