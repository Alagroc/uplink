package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Alagroc/uplink/internal/crew"
	"github.com/Alagroc/uplink/internal/ground"
	"github.com/Alagroc/uplink/internal/runner"
)

func runCrew(ctx context.Context, args []string) error {
	fs := flagSet("crew", "run a worker on this machine and wait for jobs from ground")
	name := fs.String("name", "", "crew name, unique per ground (required)")
	var roles stringList
	fs.Var(&roles, "role", "role this crew advertises; repeatable (e.g. --role builder --role reviewer)")
	groundFlag := fs.String("ground", "", "ground base URL (default $UPLINK_GROUND or http://127.0.0.1:8765)")
	workdir := fs.String("workdir", "", "directory jobs run in, and the boundary they may not escape (default: current directory)")
	tokenFile := fs.String("token-file", "", "token file (default ~/.uplink/token)")
	defaultRunner := fs.String("runner", "", "agent CLI to use by default (claude, codex, cursor-agent); default: first one found on PATH")
	runnersFile := fs.String("runners", "", "JSON file overriding how agent CLIs are launched (default ~/.uplink/runners.json)")
	maxConcurrent := fs.Int("max-concurrent", 4, "maximum jobs running at once on this host")
	pollWait := fs.Duration("poll", 25*time.Second, "how long each long-poll waits; also the heartbeat interval")
	stateDir := fs.String("state", "", "state directory for generated configs and raw transcripts (default ~/.uplink/crew)")
	clean := fs.Bool("clean", false, "start fresh: ask ground to discard any job it still thinks this crew is running, and remove configs left by a previous process. Use after this crew was killed; never while another copy of it is running")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		fs.Usage()
		return fmt.Errorf("--name is required")
	}

	// UPLINK_CREW_TOKEN wins, so a machine that is both operator and crew — a
	// testing box — can hold both credentials at once.
	token := strings.TrimSpace(os.Getenv("UPLINK_CREW_TOKEN"))
	if token == "" {
		var err error
		if token, err = loadToken(*tokenFile, false); err != nil {
			return err
		}
	}
	warn := logger("crew")
	if !ground.IsCrewToken(token) {
		warn("warning: this does not look like a crew token. Crew need their own credential now:")
		warn("  on the ground machine:  uplink crew-token add %s --role <role>", *name)
		warn("  then on this host:      export UPLINK_TOKEN=uplc_...")
	}

	home, err := homeDir()
	if err != nil {
		return err
	}
	if *runnersFile == "" {
		candidate := filepath.Join(home, "runners.json")
		if _, err := os.Stat(candidate); err == nil {
			*runnersFile = candidate
		}
	}
	specs, err := runner.Load(*runnersFile)
	if err != nil {
		return err
	}
	dir := *stateDir
	if dir == "" {
		dir = filepath.Join(home, "crew")
	}

	logf := warn
	c, err := crew.New(crew.Config{
		Name:              *name,
		Roles:             roles,
		GroundURL:         groundURL(*groundFlag),
		Token:             token,
		Workdir:           *workdir,
		Runners:           specs,
		DefaultRunner:     *defaultRunner,
		UplinkBin:         runner.DefaultUplinkBin(),
		StateDir:          dir,
		PollWait:          *pollWait,
		AgentSystemPrompt: ground.AgentInstructions,
		MaxConcurrent:     *maxConcurrent,
		Clean:             *clean,
		Build:             Version,
	}, logf)
	if err != nil {
		return err
	}

	logf("connecting to ground at %s", groundURL(*groundFlag))
	defer c.Shutdown()
	return c.Run(ctx)
}
