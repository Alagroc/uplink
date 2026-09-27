// Package crew implements the worker that runs on a remote machine: it dials
// ground, waits for jobs, runs them, and streams output back.
package crew

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
)

// client talks to ground's crew API.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *client {
	return &client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		// No overall timeout: poll is a long-poll. Per-call contexts bound it.
		http: &http.Client{Timeout: 0},
	}
}

func (c *client) post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(payload))
		}
		return &httpError{Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

type httpError struct {
	Status  int
	Message string
}

func (e *httpError) Error() string { return fmt.Sprintf("ground returned %d: %s", e.Status, e.Message) }

// needsReregister reports whether ground has forgotten us, which happens when
// ground restarts while the crew keeps polling.
func needsReregister(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status == http.StatusConflict
	}
	return false
}

func (c *client) register(ctx context.Context, req proto.RegisterReq) (proto.RegisterResp, error) {
	var resp proto.RegisterResp
	err := c.post(ctx, "/v1/crew/register", req, &resp)
	return resp, err
}

func (c *client) poll(ctx context.Context, crewID string, wait time.Duration) (proto.Command, error) {
	var cmd proto.Command
	err := c.post(ctx, "/v1/crew/poll", proto.PollReq{CrewID: crewID, WaitMS: int(wait.Milliseconds())}, &cmd)
	return cmd, err
}

func (c *client) sendLogs(ctx context.Context, crewID, jobID string, lines []proto.LogLine) error {
	return c.post(ctx, "/v1/crew/logs", proto.LogsReq{CrewID: crewID, JobID: jobID, Lines: lines}, nil)
}

func (c *client) setState(ctx context.Context, req proto.JobStateReq) error {
	return c.post(ctx, "/v1/crew/state", req, nil)
}

// health checks the tunnel before registering, so a misconfigured port fails
// with a clear message instead of a confusing auth error.
func (c *client) health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ground health check returned %d", resp.StatusCode)
	}
	return nil
}
