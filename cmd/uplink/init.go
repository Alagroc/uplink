package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Alagroc/uplink/internal/runner"
)

// runInit prepares this machine to be the operator: it creates the state
// directory and token, then prints what to do next.
//
// This exists so that first-time setup is an explicit step. Creating a
// credential as a side effect of starting a daemon is the wrong shape — you
// should be able to lay the groundwork, read what it tells you, and start
// ground when you are ready.
func runInit(args []string) error {
	fs := flagSet("init", "prepare this machine as the uplink operator: create its state directory and token")
	groundFlag := fs.String("ground", "", "ground URL to print in the wiring instructions (default http://127.0.0.1:8765)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := homeDir()
	if err != nil {
		return err
	}
	path, err := tokenPath()
	if err != nil {
		return err
	}

	created := false
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if _, err := generateToken(path); err != nil {
			return err
		}
		created = true
	}

	url := groundURL(*groundFlag)
	bin := selfName()

	var b strings.Builder
	if created {
		fmt.Fprintf(&b, "uplink initialised.\n\n")
	} else {
		fmt.Fprintf(&b, "uplink is already initialised; nothing changed.\n\n")
	}
	fmt.Fprintf(&b, "  state:  %s\n", home)
	fmt.Fprintf(&b, "  token:  %s (0600)\n\n", path)

	fmt.Fprintf(&b, "Next, on this machine:\n\n")
	fmt.Fprintf(&b, "  1. start mission control, and leave it running:\n")
	fmt.Fprintf(&b, "       %s ground\n\n", bin)
	fmt.Fprintf(&b, "  2. wire your AI CLI to it, once:\n")
	fmt.Fprintf(&b, "       claude mcp add uplink --scope user -- %s capcom --ground %s\n", bin, url)
	fmt.Fprintf(&b, "     Codex and Cursor use a config file instead — see the README.\n\n")

	fmt.Fprintf(&b, "Then for each remote machine:\n\n")
	fmt.Fprintf(&b, "  3. mint that crew its own credential, here on the ground machine:\n")
	fmt.Fprintf(&b, "       %s crew-token add devbox --role builder\n", bin)
	fmt.Fprintf(&b, "  4. copy this binary there, open a tunnel from here:\n")
	fmt.Fprintf(&b, "       ssh -R %s:127.0.0.1:%s <devbox>\n", portOf(url), portOf(url))
	fmt.Fprintf(&b, "  5. and on that machine, with an agent CLI installed and authenticated:\n")
	fmt.Fprintf(&b, "       export UPLINK_TOKEN=<the crew token from step 3>\n")
	fmt.Fprintf(&b, "       %s crew --name devbox --workdir /path/to/project\n\n", bin)

	fmt.Fprintf(&b, "Print the operator token again:    %s token\n", bin)
	fmt.Fprintf(&b, "List or revoke crew credentials:   %s crew-token list | revoke <name>\n", bin)
	fmt.Fprintf(&b, "Check the whole chain afterwards:  %s call list_crew\n", bin)

	// Knowing what is here is useful on an operator box too: the same machine is
	// often its own first crew while you are trying things out.
	specs, err := runner.Load(defaultRunnersFile(home))
	if err == nil {
		if found := runner.Detect(specs); len(found) > 0 {
			fmt.Fprintf(&b, "\nAgent CLIs found on this machine: %s\n", strings.Join(found, ", "))
		} else {
			fmt.Fprintf(&b, "\nNo agent CLI found on this machine's PATH; it can still run exec jobs as crew.\n")
		}
	}

	if !onPath() {
		fmt.Fprintf(&b, "\nNote: uplink is not on your PATH, so the lines above use its full path.\n")
		fmt.Fprintf(&b, "      Installing it makes them tidier, and makes the MCP config portable:\n")
		fmt.Fprintf(&b, "        sudo install -m 0755 %s /usr/local/bin/uplink\n", bin)
	}

	if created {
		fmt.Fprintf(&b, "\nThat token is this machine's OPERATOR credential: treat it as a password.\n")
		fmt.Fprintf(&b, "It dispatches jobs and stops ground, so it stays on this machine. Crew get\n")
		fmt.Fprintf(&b, "their own scoped credentials from `%s crew-token add`, which can only\n", bin)
		fmt.Fprintf(&b, "reach the crew endpoints and only as the one crew they name.\n")
		fmt.Fprintf(&b, "Do NOT run init on a crew host: it would generate an unrelated operator\n")
		fmt.Fprintf(&b, "token that authenticates against nothing.\n")
	}

	fmt.Print(b.String())
	return nil
}

// selfName is how to refer to this binary in instructions meant to be pasted.
//
// Never a relative path. These lines end up in an MCP server definition that
// the AI CLI spawns from whatever directory it happens to be in, and a
// `./bin/uplink` there simply fails to launch. Absolute, or the bare name when
// it is on PATH.
func selfName() string {
	exe, err := os.Executable()
	if err != nil {
		return "uplink"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	base := filepath.Base(exe)
	if onPath, err := execLookPath(base); err == nil && onPath != "" {
		return base
	}
	return exe
}

// onPath reports whether this binary can be launched by bare name.
func onPath() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	_, err = execLookPath(filepath.Base(exe))
	return err == nil
}

func defaultRunnersFile(home string) string {
	candidate := filepath.Join(home, "runners.json")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}
