package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		fs.Usage()
		return fmt.Errorf("--name is required")
	}

	token, err := loadToken(*tokenFile, false)
	if err != nil {
		return err
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

	logf := logger("crew")
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
	}, logf)
	if err != nil {
		return err
	}

	logf("connecting to ground at %s", groundURL(*groundFlag))
	defer c.Shutdown()
	return c.Run(ctx)
}
