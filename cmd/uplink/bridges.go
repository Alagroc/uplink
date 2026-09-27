package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Alagroc/uplink/internal/bridge"
)

// runCapcom is the operator's bridge: the process an AI CLI launches as its
// stdio MCP server. It holds no state — ground does.
func runCapcom(ctx context.Context, args []string) error {
	fs := flagSet("capcom", "stdio MCP bridge from your local AI CLI to ground")
	groundFlag := fs.String("ground", "", "ground base URL (default $UPLINK_GROUND or http://127.0.0.1:8765)")
	tokenFile := fs.String("token-file", "", "token file (default ~/.uplink/token)")
	quiet := fs.Bool("quiet", false, "suppress diagnostics on stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token, err := loadToken(*tokenFile, false)
	if err != nil {
		return err
	}

	logf := logger("capcom")
	if *quiet {
		logf = func(string, ...any) {}
	}

	b := &bridge.Bridge{Endpoint: groundURL(*groundFlag) + "/mcp", Token: token}
	logf("bridging stdio to %s", b.Endpoint)
	return b.Run(ctx, os.Stdin, os.Stdout, logf)
}

// runRadio is the remote agent's bridge. Its credential is the per-job token
// the crew puts in the environment, so an agent can only reach the channel
// tools for its own job.
func runRadio(ctx context.Context, args []string) error {
	fs := flagSet("radio", "stdio MCP bridge a remote agent uses to reach the operator")
	groundFlag := fs.String("ground", "", "ground base URL (default $UPLINK_GROUND)")
	tokenFile := fs.String("token-file", "", "read the per-job token from this file, for agent CLIs that can only be configured on the command line")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := strings.TrimSpace(os.Getenv("UPLINK_JOB_TOKEN"))
	if token == "" && *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read job token: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return fmt.Errorf("no job token: radio is started by uplink crew, which supplies UPLINK_JOB_TOKEN or --token-file")
	}

	b := &bridge.Bridge{Endpoint: groundURL(*groundFlag) + "/mcp/agent", Token: token}
	return b.Run(ctx, os.Stdin, os.Stdout, logger("radio"))
}
