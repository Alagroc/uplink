// Package runner launches a headless AI agent CLI on a crew host with its
// uplink radio attached, so the agent can reach the operator.
//
// Every CLI spells its flags differently and they move between versions, so the
// specs are data, not code: built-in defaults that a runners.json overrides.
package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// MCP wiring styles.
const (
	// StyleFlag passes an MCP config file path on the command line, the way
	// Claude Code's --mcp-config works.
	StyleFlag = "config-flag"
	// StyleCodexOverrides injects `-c mcp_servers.uplink.*` TOML overrides.
	StyleCodexOverrides = "codex-overrides"
	// StyleNone assumes the agent already has uplink configured. The radio
	// credentials are still exported into its environment.
	StyleNone = "none"
)

// Spec describes how to launch one agent CLI.
//
// Args support placeholders: {{prompt}}, {{mcp_config}} (path to a generated
// MCP config file) and {{system}} (uplink's briefing for the agent).
type Spec struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// MCPStyle selects how the radio is registered with the agent.
	MCPStyle string `json:"mcp_style"`
	// MCPConfigFlag is the flag name used by StyleFlag when {{mcp_config}} does
	// not already appear in Args.
	MCPConfigFlag string `json:"mcp_config_flag,omitempty"`
	// StreamJSON says stdout is a JSON-per-line agent transcript.
	StreamJSON bool              `json:"stream_json"`
	Env        map[string]string `json:"env,omitempty"`
}

// Defaults returns the built-in specs.
//
// The permission flags deliberately grant the remote agent full autonomy: the
// whole point is unattended work on a machine the operator owns, and a headless
// agent that stops at a permission prompt is useless. Confinement is the
// container's job, not the flag's — see the security notes in the README.
func Defaults() map[string]Spec {
	return map[string]Spec{
		"claude": {
			Command: "claude",
			Args: []string{
				"-p", "{{prompt}}",
				"--output-format", "stream-json",
				"--verbose",
				"--permission-mode", "bypassPermissions",
				"--append-system-prompt", "{{system}}",
				"--mcp-config", "{{mcp_config}}",
			},
			MCPStyle:      StyleFlag,
			MCPConfigFlag: "--mcp-config",
			StreamJSON:    true,
		},
		"codex": {
			Command: "codex",
			Args: []string{
				"exec",
				"--json",
				"--dangerously-bypass-approvals-and-sandbox",
				"{{prompt}}",
			},
			MCPStyle:   StyleCodexOverrides,
			StreamJSON: true,
		},
		"cursor-agent": {
			Command: "cursor-agent",
			Args: []string{
				"-p", "{{prompt}}",
				"--output-format", "stream-json",
				"--force",
			},
			MCPStyle:   StyleNone,
			StreamJSON: true,
		},
	}
}

// Load merges overrides from path onto the defaults. A missing file is not an
// error: the defaults stand.
func Load(path string) (map[string]Spec, error) {
	specs := Defaults()
	if path == "" {
		return specs, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return specs, nil
		}
		return nil, err
	}
	var overrides map[string]Spec
	if err := json.Unmarshal(data, &overrides); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for name, spec := range overrides {
		if spec.Command == "" {
			spec.Command = name
		}
		specs[name] = spec
	}
	return specs, nil
}

// Detect returns the specs whose command is on PATH, sorted by name.
func Detect(specs map[string]Spec) []string {
	var found []string
	for name, spec := range specs {
		cmd := spec.Command
		if cmd == "" {
			cmd = name
		}
		if _, err := exec.LookPath(cmd); err == nil {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// ChildEnv returns the parent environment with uplink's own operator
// credentials stripped, plus the extra entries given.
//
// This matters: crew is started with UPLINK_TOKEN exported, and that token
// authorises job dispatch on every crew. Inheriting it blindly would hand full
// operator access to any agent or shell a job starts, making the per-job token
// scoping decorative. An agent running as the same user can still read the
// token file, so this closes the easy path, not the class — real confinement is
// the container's job.
func ChildEnv(extra ...string) []string {
	const drop = "UPLINK_TOKEN="
	parent := os.Environ()
	out := make([]string, 0, len(parent)+len(extra))
	for _, kv := range parent {
		if strings.HasPrefix(kv, drop) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// Options are the per-job details needed to build a command.
type Options struct {
	Prompt string
	// Workdir is where the agent runs.
	Workdir string
	// UplinkBin is the absolute path to this binary, so the agent can spawn
	// `uplink radio` as its MCP server.
	UplinkBin string
	// GroundURL is the base URL the radio dials, e.g. http://127.0.0.1:8765
	GroundURL string
	// JobToken is the per-job agent credential. It is passed through the
	// environment, never argv, so it does not show up in `ps`.
	JobToken string
	// System is the briefing appended to the agent's system prompt.
	System string
	// ConfigDir is where a generated MCP config file is written.
	ConfigDir string
}

// Built is a launchable command plus a cleanup for any temporary files.
type Built struct {
	Cmd        *exec.Cmd
	StreamJSON bool
	Cleanup    func()
}

// radioArgs is the command line the agent uses to start its radio.
func radioArgs(o Options) []string {
	return []string{"radio", "--ground", o.GroundURL}
}

// tokenFileArgs adds --token-file, for runners whose only way to configure an
// MCP server is the command line. Passing the path keeps the token itself out
// of argv, which any user on the host can read via ps.
func tokenFileArgs(path string) []string {
	return []string{"--token-file", path}
}

// mcpServerConfig is the standard mcpServers JSON block most CLIs understand.
func mcpServerConfig(o Options) map[string]any {
	return map[string]any{
		"mcpServers": map[string]any{
			"uplink": map[string]any{
				"command": o.UplinkBin,
				"args":    radioArgs(o),
				"env":     map[string]string{"UPLINK_JOB_TOKEN": o.JobToken},
			},
		},
	}
}

// Build assembles the command for one agent job.
func Build(spec Spec, o Options) (*Built, error) {
	command := spec.Command
	if command == "" {
		return nil, fmt.Errorf("runner has no command")
	}
	bin, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("agent CLI %q not found on PATH: %w", command, err)
	}

	var cleanup func()
	configPath := ""
	tokenPath := ""

	needsConfig := spec.MCPStyle == StyleFlag
	if needsConfig {
		if err := os.MkdirAll(o.ConfigDir, 0o700); err != nil {
			return nil, err
		}
		f, err := os.CreateTemp(o.ConfigDir, "mcp-*.json")
		if err != nil {
			return nil, err
		}
		configPath = f.Name()
		// 0600: the file names the ground URL. The token stays in the env.
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return nil, err
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(mcpServerConfig(o)); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		cleanup = func() { _ = os.Remove(configPath) }
	}

	args := make([]string, 0, len(spec.Args)+6)
	sawConfig := false
	for _, a := range spec.Args {
		if strings.Contains(a, "{{mcp_config}}") {
			sawConfig = true
		}
		args = append(args, expand(a, o, configPath))
	}
	if needsConfig && !sawConfig && spec.MCPConfigFlag != "" {
		args = append(args, spec.MCPConfigFlag, configPath)
	}
	if spec.MCPStyle == StyleCodexOverrides {
		// Codex can only be told about an MCP server through -c overrides, so
		// the token goes to a 0600 file and only its path appears in argv.
		path, cleanupToken, err := writeTokenFile(o)
		if err != nil {
			return nil, err
		}
		tokenPath = path
		prev := cleanup
		cleanup = func() {
			cleanupToken()
			if prev != nil {
				prev()
			}
		}
		args = append(args, codexOverrides(o, tokenPath)...)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = o.Workdir
	cmd.Env = ChildEnv(
		"UPLINK_JOB_TOKEN="+o.JobToken,
		"UPLINK_GROUND="+o.GroundURL,
	)
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	if cleanup == nil {
		cleanup = func() {}
	}
	return &Built{Cmd: cmd, StreamJSON: spec.StreamJSON, Cleanup: cleanup}, nil
}

func expand(arg string, o Options, configPath string) string {
	arg = strings.ReplaceAll(arg, "{{prompt}}", o.Prompt)
	arg = strings.ReplaceAll(arg, "{{system}}", o.System)
	arg = strings.ReplaceAll(arg, "{{mcp_config}}", configPath)
	return arg
}

// writeTokenFile stores the job token where only its owner can read it.
func writeTokenFile(o Options) (path string, cleanup func(), err error) {
	if err := os.MkdirAll(o.ConfigDir, 0o700); err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(o.ConfigDir, "job-token-*")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if _, err := f.WriteString(o.JobToken + "\n"); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// codexOverrides expresses the radio as Codex `-c key=value` TOML overrides.
func codexOverrides(o Options, tokenPath string) []string {
	args := append(radioArgs(o), tokenFileArgs(tokenPath)...)
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, tomlString(a))
	}
	return []string{
		"-c", "mcp_servers.uplink.command=" + tomlString(o.UplinkBin),
		"-c", "mcp_servers.uplink.args=[" + strings.Join(quoted, ",") + "]",
	}
}

// tomlString renders a TOML/JSON basic string. Kept local so the quoting
// rule used for config injection is obvious at the call site.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// DefaultUplinkBin resolves this executable's absolute path.
func DefaultUplinkBin() string {
	exe, err := os.Executable()
	if err != nil {
		return "uplink"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}
