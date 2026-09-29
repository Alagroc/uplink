// Package bridge proxies MCP traffic from stdin/stdout to ground over HTTP.
//
// Every AI CLI can launch a stdio MCP server, while support for the streamable
// HTTP transport still varies between them and between versions. The bridge
// makes stdio the universal path: one small process per CLI session, with all
// tool definitions and state living in the ground daemon.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Alagroc/uplink/internal/mcp"
)

// inFlightGrace is how long a bridge waits for calls already in flight after
// its client closes stdin, before releasing them.
const inFlightGrace = 5 * time.Second

// Bridge forwards JSON-RPC messages to one ground endpoint.
type Bridge struct {
	// Endpoint is the full URL, e.g. http://127.0.0.1:8765/mcp
	Endpoint string
	// Token authenticates to ground: the operator token for /mcp, a per-job
	// token for /mcp/agent.
	Token string
	// Client is optional. The default has no overall timeout because
	// ask_operator holds a request open until a human answers.
	Client *http.Client
}

func (b *Bridge) client() *http.Client {
	if b.Client != nil {
		return b.Client
	}
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
	}
}

// Run pumps messages until r is exhausted or ctx is cancelled.
//
// Each message is forwarded on its own goroutine: a blocked ask_operator must
// not stop the agent from making other tool calls, and the CLI must stay able
// to answer a tools/list in the meantime.
func (b *Bridge) Run(ctx context.Context, r io.Reader, w io.Writer, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// A client that exits while a call is blocked (an agent CLI that crashed or
	// hit its own turn limit) must not leave the request, the goroutine and the
	// ground-side question alive for hours.
	ctx, cancelInFlight := context.WithCancel(ctx)
	defer cancelInFlight()

	client := b.client()
	frames := mcp.NewFrameReader(r)
	enc := json.NewEncoder(w)
	var writeMu sync.Mutex
	var wg sync.WaitGroup

	write := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := enc.Encode(v); err != nil {
			logf("write to client failed: %v", err)
		}
	}

	for {
		frame, err := frames.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				break
			}
			logf("read from client failed: %v", err)
			break
		}

		if !json.Valid(frame) {
			// Skip the frame rather than the session: newline framing means the
			// next line is very likely fine.
			logf("ignoring malformed frame from client")
			write(errorResponse(nil, mcp.CodeParse, "parse error: frame is not valid JSON"))
			continue
		}

		msg := make(json.RawMessage, len(frame))
		copy(msg, frame)

		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := b.forward(ctx, client, msg)
			if err != nil {
				logf("forward failed: %v", err)
				if id, isReq := requestID(msg); isReq {
					write(errorResponse(id, mcp.CodeInternal, "uplink ground unreachable: "+err.Error()))
				}
				return
			}
			if resp == nil {
				return // notification: ground accepted it, nothing to relay
			}
			write(resp)
		}()
	}

	// Stdin is closed, so the client is going away. Give calls already in flight
	// a short grace period to finish and deliver their replies, then release
	// them: a call still blocked on a human has nowhere left to deliver an
	// answer, and waiting on it would hold this process open for hours.
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(inFlightGrace):
		logf("client closed stdin with calls still in flight; releasing them")
		cancelInFlight()
		<-finished
	}
	return nil
}

// retryableMethods can be re-sent safely: they only read. tools/call is
// deliberately absent — dispatching a job twice is worse than one failed call.
var retryableMethods = map[string]bool{
	"initialize":               true,
	"tools/list":               true,
	"resources/list":           true,
	"resources/templates/list": true,
	"prompts/list":             true,
	"ping":                     true,
}

// safeToRetry reports whether re-sending this message can have no second effect.
func safeToRetry(msg json.RawMessage) bool {
	var probe struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(msg, &probe) != nil {
		return false
	}
	return retryableMethods[probe.Method]
}

// forward POSTs one message, retrying a read-only call that never reached
// ground.
//
// A tools/list that fails because ground was restarting can leave the client
// believing this server offers no tools, which does not repair itself — the
// session then cannot call anything until it re-fetches schemas by hand. One
// cheap retry removes most of that window.
func (b *Bridge) forward(ctx context.Context, client *http.Client, msg json.RawMessage) (json.RawMessage, error) {
	attempts := 1
	if safeToRetry(msg) {
		attempts = 3
	}

	var err error
	var resp json.RawMessage
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 150 * time.Millisecond):
			}
		}
		resp, err = b.forwardOnce(ctx, client, msg)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, err
}

func (b *Bridge) forwardOnce(ctx context.Context, client *http.Client, msg json.RawMessage) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Endpoint, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0:
		return nil, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("rejected by ground: %s (check the token)", strings.TrimSpace(string(body)))
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("ground returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return extractSSEData(body)
	}
	return json.RawMessage(bytes.TrimSpace(body)), nil
}

// extractSSEData pulls the JSON payload out of a single-event SSE response.
func extractSSEData(body []byte) (json.RawMessage, error) {
	for _, line := range strings.Split(string(body), "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			return json.RawMessage(strings.TrimSpace(data)), nil
		}
	}
	return nil, fmt.Errorf("no data field in event stream response")
}

// requestID reports the message id, and whether it expects a reply at all.
func requestID(msg json.RawMessage) (json.RawMessage, bool) {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(msg, &probe); err != nil || len(probe.ID) == 0 {
		return nil, false
	}
	return probe.ID, true
}

func errorResponse(id json.RawMessage, code int, message string) *mcp.Response {
	return &mcp.Response{JSONRPC: "2.0", ID: id, Error: &mcp.RPCError{Code: code, Message: message}}
}
