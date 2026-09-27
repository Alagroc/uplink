package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runShutdown stops a running ground.
//
// It goes through the same authenticated HTTP surface as everything else rather
// than hunting for a pid, so it works identically against a ground on this
// machine or one at the far end of a tunnel.
func runShutdown(ctx context.Context, args []string) error {
	fs := flagSet("shutdown", "stop a running ground")
	groundFlag := fs.String("ground", "", "ground base URL (default $UPLINK_GROUND or http://127.0.0.1:8765)")
	tokenFile := fs.String("token-file", "", "token file (default ~/.uplink/token)")
	force := fs.Bool("force", false, "shut down even if jobs are running or an agent is waiting on you")
	reason := fs.String("reason", "", "note recorded in the audit log")
	if err := fs.Parse(args); err != nil {
		return err
	}

	url := groundURL(*groundFlag)
	client := &http.Client{Timeout: 20 * time.Second}

	// Check what is actually there first, so "connection refused" and "someone
	// else owns this port" do not look like the same failure.
	switch err := probeGround(ctx, client, url); {
	case err == errNoGround:
		fmt.Printf("Nothing is running at %s.\n", url)
		return nil
	case err == errNotUplink:
		return fmt.Errorf("something is listening at %s but it is not uplink; leaving it alone", url)
	case err != nil:
		return err
	}

	token, err := loadToken(*tokenFile, false)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{"force": *force, "reason": *reason})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/shutdown", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		// Ground can close the connection as it goes down; that is a success.
		if isConnectionClosed(err) {
			fmt.Println("ground stopped.")
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Status           string   `json:"status"`
		Error            string   `json:"error"`
		ActiveJobs       []string `json:"active_jobs"`
		PendingQuestion  int      `json:"pending_question"`
		AbandonedJobs    []string `json:"abandoned_jobs"`
		AbandonedPending int      `json:"abandoned_pending"`
	}
	_ = json.Unmarshal(payload, &out)

	switch resp.StatusCode {
	case http.StatusOK:
		fmt.Println("ground stopped.")
		for _, j := range out.AbandonedJobs {
			fmt.Printf("  abandoned: %s\n", j)
		}
		if out.AbandonedPending > 0 {
			fmt.Printf("  %d question(s) left unanswered\n", out.AbandonedPending)
		}
		return nil

	case http.StatusConflict:
		var b strings.Builder
		fmt.Fprintf(&b, "ground is still busy, so it was left running.\n")
		for _, j := range out.ActiveJobs {
			fmt.Fprintf(&b, "  running: %s\n", j)
		}
		if out.PendingQuestion > 0 {
			fmt.Fprintf(&b, "  %d agent(s) waiting on an answer from you (see: uplink call inbox)\n", out.PendingQuestion)
		}
		fmt.Fprintf(&b, "\nStop them first, or shut down anyway with:  uplink shutdown --force")
		return fmt.Errorf("%s", b.String())

	case http.StatusNotFound:
		return fmt.Errorf("the ground at %s has no shutdown endpoint: it is running an older uplink binary.\n"+
			"Stop that process directly, then start the new one:  pkill -f 'uplink ground'", url)

	case http.StatusUnauthorized:
		return fmt.Errorf("ground at %s rejected the token; this machine's token may not be the one it was started with", url)

	default:
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(payload))
		}
		return fmt.Errorf("ground returned %d: %s", resp.StatusCode, msg)
	}
}

var (
	errNoGround  = fmt.Errorf("no ground")
	errNotUplink = fmt.Errorf("not uplink")
)

// probeGround reports whether an uplink ground is listening at url.
func probeGround(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return errNoGround
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return errNotUplink
	}
	var health struct {
		Status   string `json:"status"`
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal(payload, &health) != nil || health.Status != "ok" || health.Protocol == "" {
		return errNotUplink
	}
	return nil
}

func isConnectionClosed(err error) bool {
	s := err.Error()
	return strings.Contains(s, "EOF") || strings.Contains(s, "connection reset") || strings.Contains(s, "server closed")
}

// warnIfGroundAlreadyRunning turns a bare EADDRINUSE into something actionable.
func warnIfGroundAlreadyRunning(url string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := probeGround(ctx, &http.Client{Timeout: 3 * time.Second}, url); err == nil {
		return fmt.Sprintf("\nAnother uplink ground is already running there.\n"+
			"  stop it:            %s shutdown\n"+
			"  or use a new port:  %s ground --addr 127.0.0.1:8766", selfName(), selfName())
	}
	return "\nSomething else is using that port. Pick another with --addr."
}
