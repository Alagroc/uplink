package ground

import (
	"strings"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
	"github.com/Alagroc/uplink/internal/store"
)

func groundWithBuild(t *testing.T, build string) *Ground {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, Options{OfflineAfter: time.Minute, Build: build}, nil)
}

func registerBuild(t *testing.T, g *Ground, name, build string) {
	t.Helper()
	if _, err := g.Register(proto.RegisterReq{
		Name: name, Build: build, OS: "linux", Arch: "amd64", Hostname: "h", Workdir: "/w",
	}); err != nil {
		t.Fatal(err)
	}
}

// The protocol version almost never changes, so it cannot catch a crew running
// months-old code — which behaves differently for reasons nothing reports.
func TestBuildSkewIsDetected(t *testing.T) {
	g := groundWithBuild(t, "v1.4.0")

	if skew := g.buildSkew("v1.4.0"); skew != "" {
		t.Errorf("a matching build should not be flagged, got %q", skew)
	}
	skew := g.buildSkew("v0.9.1")
	if !strings.Contains(skew, "v0.9.1") || !strings.Contains(skew, "v1.4.0") {
		t.Errorf("skew should name both builds, got %q", skew)
	}
}

// A binary built without -ldflags reports "dev", which says nothing about which
// commit it came from. Comparing two of those would cry wolf on every run.
func TestDevBuildsAreNotCompared(t *testing.T) {
	if skew := groundWithBuild(t, "dev").buildSkew("v0.9.1"); skew != "" {
		t.Errorf("a dev ground cannot judge a crew's build, got %q", skew)
	}
	if skew := groundWithBuild(t, "v1.4.0").buildSkew("dev"); skew != "" {
		t.Errorf("a dev crew build is not a known mismatch, got %q", skew)
	}
	if skew := groundWithBuild(t, "").buildSkew("v0.9.1"); skew != "" {
		t.Errorf("an unknown ground build cannot judge anything, got %q", skew)
	}
}

// A crew old enough to predate build reporting sends nothing — which is itself
// the strongest evidence it is out of date.
func TestCrewReportingNoBuildIsFlagged(t *testing.T) {
	skew := groundWithBuild(t, "v1.4.0").buildSkew("")
	if skew == "" {
		t.Fatal("a crew reporting no build should be flagged")
	}
	if !strings.Contains(skew, "out of date") {
		t.Errorf("the reason should be legible, got %q", skew)
	}
}

func TestListCrewShowsBuildAndFlagsSkew(t *testing.T) {
	g := groundWithBuild(t, "v1.4.0")
	registerBuild(t, g, "current", "v1.4.0")
	registerBuild(t, g, "stale", "v0.9.1")

	out, err := toolCall(t, g, "list_crew", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "build: v1.4.0") {
		t.Errorf("the build should be shown for every crew:\n%s", out)
	}
	if !strings.Contains(out, "build: v0.9.1") {
		t.Errorf("the stale crew's build should be shown:\n%s", out)
	}
	if !strings.Contains(out, "ground is v1.4.0") {
		t.Errorf("the mismatch should be called out:\n%s", out)
	}
	// And the fix should be named, not left as an exercise.
	if !strings.Contains(out, "--clean") {
		t.Errorf("list_crew should say what to do about it:\n%s", out)
	}

	// The matching crew must not be flagged.
	currentBlock := out[strings.Index(out, "current"):strings.Index(out, "stale")]
	if strings.Contains(currentBlock, "ground is") {
		t.Errorf("an up-to-date crew was flagged:\n%s", currentBlock)
	}
}

func TestBuildIsRecordedOnTheCrew(t *testing.T) {
	g := groundWithBuild(t, "v1.4.0")
	registerBuild(t, g, "devbox", "v1.4.0")

	crew := g.ListCrew()
	if len(crew) != 1 {
		t.Fatalf("got %d crew", len(crew))
	}
	if crew[0].Build != "v1.4.0" {
		t.Errorf("build = %q, want v1.4.0", crew[0].Build)
	}
	if crew[0].BuildSkew != "" {
		t.Errorf("unexpected skew: %q", crew[0].BuildSkew)
	}
}
