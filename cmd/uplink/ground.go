package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Alagroc/uplink/internal/ground"
	"github.com/Alagroc/uplink/internal/store"
)

func runGround(ctx context.Context, args []string) error {
	fs := flagSet("ground", "run mission control (the hub) on your laptop")
	addr := fs.String("addr", "127.0.0.1:8765", "listen address; keep it on loopback and reach it through an SSH tunnel")
	stateDir := fs.String("state", "", "state directory (default ~/.uplink/ground)")
	tokenFile := fs.String("token-file", "", "token file (default ~/.uplink/token, created if missing)")
	notify := fs.String("notify", "", "shell command run when an agent asks a question; UPLINK_CREW, UPLINK_QUESTION and UPLINK_QUESTION_ID are in its environment. The question text is written by an agent, so quote it (\"$UPLINK_QUESTION\") in your command")
	bell := fs.Bool("bell", true, "ring the terminal bell when an agent asks a question")
	debug := fs.Bool("debug", false, "log every request: what was asked, by which crew, the status and how long it took")
	timestamps := fs.Bool("timestamps", true, "prefix each line with the time; turn off when a supervisor such as journald already stamps the stream")
	offlineAfter := fs.Duration("offline-after", 90*time.Second, "mark a crew offline after this long without a poll")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logTimestamps = *timestamps

	if err := warnIfPubliclyBound(*addr); err != nil {
		return err
	}

	token, err := loadToken(*tokenFile, true)
	if err != nil {
		return err
	}

	dir := *stateDir
	if dir == "" {
		home, err := homeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, "ground")
	}
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer st.Close()

	crewTokens, err := ground.OpenCrewTokens(filepath.Join(dir, "crew-tokens.json"))
	if err != nil {
		return err
	}

	logf := logger("ground")
	g := ground.New(st, ground.Options{
		OfflineAfter: *offlineAfter,
		NotifyCmd:    *notify,
		Bell:         *bell,
		Build:        Version,
	}, func(line string) { logf("%s", line) })

	stopped := make(chan string, 1)
	srv := &ground.Server{
		Ground:     g,
		Token:      token,
		Version:    Version,
		CrewTokens: crewTokens,
		Debug:      *debug,
		Logf:       logf,
		RequestShutdown: func(reason string) {
			select {
			case stopped <- reason:
			default:
			}
		},
	}
	httpSrv := srv.HTTPServer(*addr)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("cannot listen on %s: address already in use.%s", *addr, warnIfGroundAlreadyRunning("http://"+*addr))
		}
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	printGroundBanner(ln.Addr().String(), dir)
	if crewTokens.Count() == 0 {
		fmt.Fprintf(os.Stderr, "[ground] no crew tokens yet — mint one before starting a crew:\n")
		fmt.Fprintf(os.Stderr, "[ground]   %s crew-token add devbox --role builder\n", selfName())
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logf("shutting down")
	case reason := <-stopped:
		logf("shutting down: %s", reason)
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// warnIfPubliclyBound refuses a wildcard bind. Ground is remote code execution
// by design; it belongs on loopback behind an SSH tunnel.
func warnIfPubliclyBound(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		if os.Getenv("UPLINK_ALLOW_PUBLIC_BIND") != "1" {
			return fmt.Errorf("refusing to bind %s: ground can run commands on every crew, so it must not listen on a public interface.\n"+
				"Use an SSH tunnel (see the README), or set UPLINK_ALLOW_PUBLIC_BIND=1 if you genuinely mean it", addr)
		}
		fmt.Fprintln(os.Stderr, "[ground] WARNING: bound to a public interface with UPLINK_ALLOW_PUBLIC_BIND=1")
	}
	return nil
}

func printGroundBanner(addr, stateDir string) {
	var b strings.Builder
	fmt.Fprintf(&b, "[ground] uplink %s listening on http://%s (started %s)\n", Version, addr, time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "[ground] state + audit log: %s\n", stateDir)
	fmt.Fprintf(&b, "[ground] operator MCP endpoint: http://%s/mcp\n", addr)
	fmt.Fprintf(&b, "[ground] agent MCP endpoint:    http://%s/mcp/agent\n", addr)
	fmt.Fprintf(&b, "[ground]\n")
	fmt.Fprintf(&b, "[ground] Point your local AI CLI at:  uplink capcom --ground http://%s\n", addr)
	fmt.Fprintf(&b, "[ground] Reverse-tunnel to a devbox:  ssh -R %s:%s <devbox>\n", portOf(addr), addr)
	fmt.Fprint(os.Stderr, b.String())
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return "8765"
}
