package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin creates an executable stub on PATH so Build can resolve it.
func fakeBin(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func testOptions(t *testing.T) Options {
	return Options{
		Prompt:    "migrate the stack",
		Workdir:   t.TempDir(),
		UplinkBin: "/usr/local/bin/uplink",
		GroundURL: "http://127.0.0.1:8765",
		JobToken:  "secret-job-token",
		System:    "you are crew",
		ConfigDir: t.TempDir(),
	}
}

func TestDefaultsCoverTheThreeCLIs(t *testing.T) {
	specs := Defaults()
	for _, name := range []string{"claude", "codex", "cursor-agent"} {
		spec, ok := specs[name]
		if !ok {
			t.Errorf("no default spec for %q", name)
			continue
		}
		if spec.Command == "" {
			t.Errorf("%s: empty command", name)
		}
		if !strings.Contains(strings.Join(spec.Args, " "), "{{prompt}}") {
			t.Errorf("%s: args never substitute the prompt: %v", name, spec.Args)
		}
	}
}

func TestBuildClaudeWritesMCPConfig(t *testing.T) {
	fakeBin(t, "claude")
	o := testOptions(t)
	built, err := Build(Defaults()["claude"], o)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	args := built.Cmd.Args
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, o.Prompt) {
		t.Error("prompt was not substituted")
	}
	if !strings.Contains(joined, "--mcp-config") {
		t.Error("claude needs --mcp-config to see the radio")
	}
	if !strings.Contains(joined, o.System) {
		t.Error("system briefing was not passed")
	}

	// The config file must describe how to launch the radio.
	var configPath string
	for i, a := range args {
		if a == "--mcp-config" && i+1 < len(args) {
			configPath = args[i+1]
		}
	}
	if configPath == "" {
		t.Fatal("no config path in args")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, data)
	}
	uplink, ok := cfg.MCPServers["uplink"]
	if !ok {
		t.Fatal("config has no uplink server")
	}
	if uplink.Command != o.UplinkBin {
		t.Errorf("command = %q, want the uplink binary", uplink.Command)
	}
	if uplink.Args[0] != "radio" {
		t.Errorf("args should start the radio, got %v", uplink.Args)
	}
	if uplink.Env["UPLINK_JOB_TOKEN"] != o.JobToken {
		t.Error("the job token must reach the radio through its environment")
	}

	// Cleanup must remove the generated config.
	built.Cleanup()
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Error("generated MCP config was left behind")
	}
}

// The job token is a credential: it must never appear in argv, which is
// readable by any process on the host via ps.
// The generated config path must be absolute. The agent runs with its own
// working directory, so a relative path resolves somewhere it does not exist —
// which surfaces as "MCP config file not found" and a dead agent job.
func TestGeneratedConfigPathIsAbsolute(t *testing.T) {
	fakeBin(t, "claude")
	dir := t.TempDir()
	t.Chdir(dir)

	o := testOptions(t)
	o.ConfigDir = "relative/state/mcp" // as a relative UPLINK_HOME would produce
	o.Workdir = dir

	built, err := Build(Defaults()["claude"], o)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	for i, a := range built.Cmd.Args {
		if a != "--mcp-config" || i+1 >= len(built.Cmd.Args) {
			continue
		}
		path := built.Cmd.Args[i+1]
		if !filepath.IsAbs(path) {
			t.Fatalf("mcp config path is relative (%q); the agent would not find it", path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("config file not written where the args point: %v", err)
		}
		return
	}
	t.Fatal("no --mcp-config in args")
}

func TestJobTokenNeverAppearsInArgv(t *testing.T) {
	for _, name := range []string{"claude", "cursor-agent"} {
		fakeBin(t, name)
		o := testOptions(t)
		built, err := Build(Defaults()[name], o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer built.Cleanup()
		if joined := strings.Join(built.Cmd.Args, " "); strings.Contains(joined, o.JobToken) {
			t.Errorf("%s: job token leaked into argv: %s", name, joined)
		}
		var found bool
		for _, kv := range built.Cmd.Env {
			if kv == "UPLINK_JOB_TOKEN="+o.JobToken {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: job token missing from the environment", name)
		}
	}
}

// Codex is the documented exception: its config overrides are command-line
// only, so the token is visible in argv on that host. The test pins the
// behaviour so the trade-off stays deliberate and documented.
func TestCodexOverridesCarryTheRadioConfig(t *testing.T) {
	fakeBin(t, "codex")
	o := testOptions(t)
	built, err := Build(Defaults()["codex"], o)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	joined := strings.Join(built.Cmd.Args, " ")
	for _, want := range []string{"mcp_servers.uplink.command=", `"radio"`, `"--token-file"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("codex args missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, `"`+o.UplinkBin+`"`) {
		t.Error("the uplink path must be TOML-quoted for -c overrides")
	}

	// Codex can only be configured on the command line, so the token goes to a
	// file and only its path is visible in ps.
	if strings.Contains(joined, o.JobToken) {
		t.Errorf("job token leaked into codex argv:\n%s", joined)
	}
	var tokenPath string
	for i, a := range built.Cmd.Args {
		if strings.Contains(a, "--token-file") {
			// The path is inside the quoted TOML array on this same argument.
			for _, field := range strings.Split(strings.Trim(a, "[]"), ",") {
				if strings.Contains(field, "job-token-") {
					tokenPath = strings.Trim(field, `"`)
				}
			}
			_ = i
		}
	}
	if tokenPath == "" {
		t.Fatalf("no token file path in codex args:\n%s", joined)
	}
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != o.JobToken {
		t.Error("token file does not contain the job token")
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file permissions = %o, want 600", perm)
	}
	built.Cleanup()
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Error("the token file must be removed when the job ends")
	}
}

// The crew is started with UPLINK_TOKEN exported, and that token authorises job
// dispatch on every crew. It must not be inherited by the agent, or the per-job
// credential split is decorative.
func TestOperatorTokenIsNotInheritedByAgents(t *testing.T) {
	fakeBin(t, "claude")
	t.Setenv("UPLINK_TOKEN", "OPERATOR-SECRET")

	built, err := Build(Defaults()["claude"], testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	for _, kv := range built.Cmd.Env {
		if strings.HasPrefix(kv, "UPLINK_TOKEN=") {
			t.Fatalf("operator token leaked into the agent environment: %s", kv)
		}
	}
	// The per-job token must still be there.
	var found bool
	for _, kv := range built.Cmd.Env {
		if strings.HasPrefix(kv, "UPLINK_JOB_TOKEN=") {
			found = true
		}
	}
	if !found {
		t.Error("the per-job token should still be passed")
	}
}

func TestChildEnvKeepsEverythingElse(t *testing.T) {
	t.Setenv("UPLINK_TOKEN", "secret")
	t.Setenv("UNRELATED_VAR", "keep-me")
	env := ChildEnv("EXTRA=1")

	var sawUnrelated, sawExtra bool
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "UPLINK_TOKEN="):
			t.Error("UPLINK_TOKEN should be stripped")
		case kv == "UNRELATED_VAR=keep-me":
			sawUnrelated = true
		case kv == "EXTRA=1":
			sawExtra = true
		}
	}
	if !sawUnrelated {
		t.Error("unrelated environment variables must be preserved")
	}
	if !sawExtra {
		t.Error("extra entries must be appended")
	}
}

func TestBuildFailsClearlyWhenCLIMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Build(Defaults()["claude"], testOptions(t))
	if err == nil {
		t.Fatal("expected an error when the agent CLI is absent")
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("error should name the cause, got: %v", err)
	}
}

func TestDetectFindsOnlyInstalledRunners(t *testing.T) {
	// Isolate PATH: the machine running the tests may have real agent CLIs
	// installed, which would otherwise be detected too.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	found := Detect(Defaults())
	if len(found) != 1 || found[0] != "codex" {
		t.Fatalf("Detect = %v, want just codex", found)
	}
}

// The default runner must be a documented choice, not an artefact of sorting.
func TestDetectReturnsPreferenceOrder(t *testing.T) {
	dir := t.TempDir()
	// Install them in an order that alphabetical sorting would get wrong for the
	// custom entry, and that proves preference beats alphabetical.
	for _, name := range []string{"cursor-agent", "codex", "claude", "aardvark-agent"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	specs := Defaults()
	specs["aardvark-agent"] = Spec{Command: "aardvark-agent", MCPStyle: StyleNone}

	got := Detect(specs)
	want := []string{"claude", "codex", "cursor-agent", "aardvark-agent"}
	if len(got) != len(want) {
		t.Fatalf("Detect = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Detect = %v, want %v (known runners in preference order, unknown ones last)", got, want)
		}
	}
}

// With only a less-preferred CLI installed, that is what gets picked.
func TestDetectPrefersWhatIsActuallyInstalled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cursor-agent"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := Detect(Defaults()); len(got) != 1 || got[0] != "cursor-agent" {
		t.Fatalf("Detect = %v, want [cursor-agent]", got)
	}
}

func TestLoadMergesOverridesOntoDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runners.json")
	override := `{"claude":{"command":"claude-next","args":["-p","{{prompt}}"],"mcp_style":"none","stream_json":false},
	              "homegrown":{"args":["run","{{prompt}}"],"mcp_style":"none"}}`
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}

	specs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if specs["claude"].Command != "claude-next" {
		t.Errorf("override ignored: %+v", specs["claude"])
	}
	if specs["claude"].StreamJSON {
		t.Error("override should be able to turn stream_json off")
	}
	// A spec with no command defaults to its own key, so a minimal override works.
	if specs["homegrown"].Command != "homegrown" {
		t.Errorf("command should default to the runner name, got %q", specs["homegrown"].Command)
	}
	// Untouched defaults survive.
	if specs["codex"].Command != "codex" {
		t.Error("unrelated defaults should be preserved")
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	specs, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing runners.json should fall back to defaults: %v", err)
	}
	if len(specs) != len(Defaults()) {
		t.Error("defaults should be returned intact")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runners.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("malformed config should fail loudly, not silently fall back")
	}
}

func TestGeneratedConfigIsOwnerOnly(t *testing.T) {
	fakeBin(t, "claude")
	o := testOptions(t)
	built, err := Build(Defaults()["claude"], o)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()
	for i, a := range built.Cmd.Args {
		if a != "--mcp-config" || i+1 >= len(built.Cmd.Args) {
			continue
		}
		info, err := os.Stat(built.Cmd.Args[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("config permissions = %o, want 600", perm)
		}
	}
}

// --- guaranteed flags ---

// Codex has no non-interactive approval mode short of this flag, and a crew
// agent has no human to approve anything, so losing it means a job that stalls
// until it times out.
func TestCodexAlwaysGetsTheUnattendedFlag(t *testing.T) {
	const flag = "--dangerously-bypass-approvals-and-sandbox"

	cases := []struct {
		name string
		spec Spec
	}{
		{"default spec", Defaults()["codex"]},
		{
			// The case this exists for: someone writes their own codex spec in
			// runners.json and leaves the flag out.
			name: "override that forgot the flag",
			spec: Spec{
				Command:    "codex",
				Args:       []string{"exec", "--json", "{{prompt}}"},
				EnsureArgs: Defaults()["codex"].EnsureArgs,
				MCPStyle:   StyleCodexOverrides,
			},
		},
		{
			name: "override that already has it",
			spec: Spec{
				Command:    "codex",
				Args:       []string{"exec", flag, "--json", "{{prompt}}"},
				EnsureArgs: Defaults()["codex"].EnsureArgs,
				MCPStyle:   StyleCodexOverrides,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin(t, "codex")
			built, err := Build(tc.spec, testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			defer built.Cleanup()

			var count int
			for _, a := range built.Cmd.Args {
				if a == flag {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("flag appears %d times, want exactly 1: %v", count, built.Cmd.Args)
			}
		})
	}
}

// The prompt is positional, and not every CLI accepts flags after a positional
// argument, so an ensured flag must land before it.
func TestEnsuredFlagsGoBeforeThePrompt(t *testing.T) {
	fakeBin(t, "codex")
	o := testOptions(t)
	built, err := Build(Defaults()["codex"], o)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	flagAt, promptAt := -1, -1
	for i, a := range built.Cmd.Args {
		switch {
		case a == "--dangerously-bypass-approvals-and-sandbox":
			flagAt = i
		case strings.Contains(a, o.Prompt):
			// Contains, not equals: for a runner with no system-prompt flag the
			// briefing is folded into this same argument.
			promptAt = i
		}
	}
	if flagAt < 0 || promptAt < 0 {
		t.Fatalf("flag or prompt missing: %v", built.Cmd.Args)
	}
	if flagAt > promptAt {
		t.Errorf("flag at %d comes after the prompt at %d: %v", flagAt, promptAt, built.Cmd.Args)
	}
}

// Removing the guarantee must stay possible: a deliberate choice to run codex
// with approvals should not be silently overridden.
func TestEnsureArgsCanBeOptedOut(t *testing.T) {
	fakeBin(t, "codex")
	spec := Defaults()["codex"]
	spec.EnsureArgs = []string{} // what "ensure_args": [] in runners.json means

	built, err := Build(spec, testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	for _, a := range built.Cmd.Args {
		if a == "--dangerously-bypass-approvals-and-sandbox" {
			t.Fatal("an explicit opt-out was overridden")
		}
	}
}

func TestEnsureArgsCarriesFlagValues(t *testing.T) {
	fakeBin(t, "claude")
	spec := Spec{
		Command:    "claude",
		Args:       []string{"-p", "{{prompt}}"},
		EnsureArgs: []string{"--permission-mode", "bypassPermissions"},
		MCPStyle:   StyleNone,
	}
	built, err := Build(spec, testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	joined := strings.Join(built.Cmd.Args, " ")
	if !strings.Contains(joined, "--permission-mode bypassPermissions") {
		t.Errorf("a flag and its value should stay together: %s", joined)
	}
}

// runners.json must be able to express the guarantee.
func TestEnsureArgsRoundTripsThroughConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runners.json")
	body := `{"codex":{"command":"codex","args":["exec","{{prompt}}"],"ensure_args":["--dangerously-bypass-approvals-and-sandbox"],"mcp_style":"codex-overrides"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := specs["codex"].EnsureArgs; len(got) != 1 || got[0] != "--dangerously-bypass-approvals-and-sandbox" {
		t.Errorf("ensure_args = %v", got)
	}
}

// --- the system briefing ---

// Only Claude Code has a flag for appending to the system prompt. Every other
// runner must still receive the briefing, or the agent never learns that
// ask_operator exists — the one thing that makes it crew rather than a shell.
func TestRunnersWithoutASystemFlagGetTheBriefingInThePrompt(t *testing.T) {
	for _, name := range []string{"cursor-agent", "codex"} {
		t.Run(name, func(t *testing.T) {
			fakeBin(t, name)
			o := testOptions(t)
			built, err := Build(Defaults()[name], o)
			if err != nil {
				t.Fatal(err)
			}
			defer built.Cleanup()

			joined := strings.Join(built.Cmd.Args, " ")
			if !strings.Contains(joined, o.System) {
				t.Errorf("%s never receives the briefing:\n%s", name, joined)
			}
			if !strings.Contains(joined, o.Prompt) {
				t.Errorf("%s lost the task prompt:\n%s", name, joined)
			}
			if !strings.Contains(joined, "Your task") {
				t.Errorf("%s should separate briefing from task:\n%s", name, joined)
			}
		})
	}
}

// Claude has --append-system-prompt, so the briefing must not also be folded
// into the prompt — that would send it twice.
func TestClaudeGetsTheBriefingOnlyViaItsFlag(t *testing.T) {
	fakeBin(t, "claude")
	o := testOptions(t)
	built, err := Build(Defaults()["claude"], o)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Cleanup()

	var count int
	for _, a := range built.Cmd.Args {
		if strings.Contains(a, o.System) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("briefing appears in %d args, want exactly 1: %v", count, built.Cmd.Args)
	}
	for _, a := range built.Cmd.Args {
		if a == o.Prompt && strings.Contains(a, o.System) {
			t.Error("the prompt should not carry the briefing when a flag does")
		}
	}
}
