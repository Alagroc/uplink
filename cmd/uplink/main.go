// Command uplink lets an AI CLI on your laptop drive AI agents on remote hosts.
//
// Four roles, one binary:
//
//	uplink ground  mission control: the hub daemon, run on your laptop
//	uplink crew    a worker on a remote machine, waiting for jobs
//	uplink capcom  stdio MCP bridge your local AI CLI launches
//	uplink radio   stdio MCP bridge a remote agent uses to reach you
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Version is overridden at build time with -ldflags "-X main.Version=..."
var Version = "dev"

const usage = `uplink — talk to AI agents running on other machines.

Usage: uplink <command> [flags]

Commands:
  init      Prepare this machine as the operator: create its token, print next steps.
  ground    Run mission control (the hub). Start this on your laptop.
  shutdown  Stop a running ground.
  crew      Run a worker on a remote machine; connects out to ground.
  capcom    stdio MCP bridge for your local AI CLI (claude/codex/cursor-agent).
  radio     stdio MCP bridge a remote agent uses to reach the operator.
  call      Invoke one operator tool from the shell. Useful for testing.
  token     Print the operator token (needed by capcom, call and shutdown).
  crew-token  Mint, list and revoke the per-crew credentials.
  version   Print the version.

Run "uplink <command> -h" for the flags of each command.

Quick start:
  # on your laptop
  uplink init
  uplink ground
  # on the remote host, with the tunnel up and UPLINK_TOKEN set
  uplink crew --name devbox --role builder --ground http://127.0.0.1:8765
  # then point your AI CLI at: uplink capcom --ground http://127.0.0.1:8765
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "init":
		err = runInit(os.Args[2:])
	case "ground":
		err = runGround(ctx, os.Args[2:])
	case "shutdown":
		err = runShutdown(ctx, os.Args[2:])
	case "crew":
		err = runCrew(ctx, os.Args[2:])
	case "capcom":
		err = runCapcom(ctx, os.Args[2:])
	case "radio":
		err = runRadio(ctx, os.Args[2:])
	case "call":
		err = runCall(ctx, os.Args[2:])
	case "token":
		err = runToken(os.Args[2:])
	case "crew-token":
		err = runCrewToken(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("uplink " + Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintln(os.Stderr, "uplink: "+err.Error())
		os.Exit(1)
	}
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

var (
	// logOut is a variable so tests can capture what the loggers emit.
	logOut io.Writer = os.Stderr

	logMu sync.Mutex
	// logTimestamps is off only when something else is already stamping the
	// stream, such as journald or a supervisor.
	logTimestamps = true
	lastLogDay    string

	// logNow is a variable so tests can step across a midnight boundary.
	logNow = time.Now
)

// logger returns a stderr logger for one component.
//
// Lines carry the time but not the date: a full date on every line is a lot of
// width to spend on something that changes once a day, so the date is printed
// as its own marker whenever it rolls over. A log spanning days stays
// unambiguous without every line paying for it.
//
// The mutex also keeps concurrent writers from interleaving — ground logs one
// line per request from many goroutines at once.
func logger(prefix string) func(string, ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)

		logMu.Lock()
		defer logMu.Unlock()

		if !logTimestamps {
			fmt.Fprintf(logOut, "[%s] %s\n", prefix, msg)
			return
		}
		now := logNow()
		if day := now.Format("2006-01-02"); day != lastLogDay {
			fmt.Fprintf(logOut, "--- %s ---\n", day)
			lastLogDay = day
		}
		fmt.Fprintf(logOut, "%s [%s] %s\n", now.Format("15:04:05.000"), prefix, msg)
	}
}

// flagSet builds a flag set that prints a helpful header.
func flagSet(name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "uplink %s — %s\n\nFlags:\n", name, summary)
		fs.PrintDefaults()
	}
	return fs
}
