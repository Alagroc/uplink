package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/mcp"
)

// stubGround is a minimal MCP server with one fast tool and one that blocks
// until released, so tests can check that a blocked call does not stall others.
func stubGround(t *testing.T, release <-chan struct{}) (*httptest.Server, *int32Counter) {
	t.Helper()
	calls := &int32Counter{}
	s := mcp.NewServer("stub", "1", "stub instructions")
	s.Add(mcp.Tool{
		Name:   "fast",
		Schema: mcp.Schema{Props: map[string]mcp.Prop{}},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			calls.inc()
			return "fast done", nil
		},
	})
	s.Add(mcp.Tool{
		Name:   "blocking",
		Schema: mcp.Schema{Props: map[string]mcp.Prop{}},
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			select {
			case <-release:
				return "released", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	})
	srv := httptest.NewServer(&mcp.HTTPHandler{
		Server: s,
		Authorize: func(r *http.Request) (*http.Request, error) {
			if mcp.BearerToken(r) != "good-token" {
				return nil, errUnauthorized
			}
			return r, nil
		},
	})
	t.Cleanup(srv.Close)
	return srv, calls
}

var errUnauthorized = &mcp.RPCError{Code: 401, Message: "bad token"}

type int32Counter struct {
	mu sync.Mutex
	n  int
}

func (c *int32Counter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

// runBridge pipes the given input through a bridge and returns the replies it
// wrote, keyed by JSON-RPC id.
func runBridge(t *testing.T, endpoint, token, input string, wantReplies int) map[float64]map[string]any {
	t.Helper()
	b := &Bridge{Endpoint: endpoint, Token: token}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go func() {
		_ = b.Run(ctx, inR, outW, nil)
		outW.Close()
	}()
	go func() {
		_, _ = io.WriteString(inW, input)
		// Leave stdin open until the replies are collected: closing it early
		// would race with in-flight calls.
		time.AfterFunc(10*time.Second, func() { inW.Close() })
	}()

	replies := map[float64]map[string]any{}
	dec := json.NewDecoder(outR)
	for len(replies) < wantReplies {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			t.Fatalf("decoding reply %d: %v", len(replies)+1, err)
		}
		id, _ := msg["id"].(float64)
		replies[id] = msg
	}
	inW.Close()
	return replies
}

func TestBridgeForwardsToolCalls(t *testing.T) {
	srv, _ := stubGround(t, nil)
	replies := runBridge(t, srv.URL, "good-token",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fast","arguments":{}}}`+"\n", 2)

	init := replies[1]["result"].(map[string]any)
	if init["instructions"] != "stub instructions" {
		t.Errorf("initialize did not pass through: %+v", init)
	}
	result := replies[2]["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "fast done" {
		t.Errorf("tool result did not pass through: %+v", result)
	}
}

// Notifications get no reply, and must not produce a stray line that would
// confuse the CLI's JSON-RPC parser.
func TestBridgeSwallowsNotifications(t *testing.T) {
	srv, _ := stubGround(t, nil)
	b := &Bridge{Endpoint: srv.URL, Token: "good-token"}

	var out strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := b.Run(ctx, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Run returns once stdin is exhausted; give the dispatch goroutine a moment.
	time.Sleep(200 * time.Millisecond)
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("a notification must not be answered, got: %q", out.String())
	}
}

// The reason each message gets its own goroutine: ask_operator can block for
// hours, and the session must stay usable.
func TestBridgeDoesNotSerializeBlockingCalls(t *testing.T) {
	release := make(chan struct{})
	srv, _ := stubGround(t, release)

	b := &Bridge{Endpoint: srv.URL, Token: "good-token"}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		_ = b.Run(ctx, inR, outW, nil)
		outW.Close()
	}()

	// The blocking call goes first; the fast call must still come back.
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blocking","arguments":{}}}`+"\n")
	time.Sleep(200 * time.Millisecond)
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fast","arguments":{}}}`+"\n")

	dec := json.NewDecoder(outR)
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if id, _ := first["id"].(float64); id != 2 {
		t.Fatalf("the fast call should return while the other is blocked; got id %v first", first["id"])
	}

	close(release)
	var second map[string]any
	if err := dec.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if id, _ := second["id"].(float64); id != 1 {
		t.Errorf("expected the released blocking call, got id %v", second["id"])
	}
	inW.Close()
}

// If ground is down, the CLI must see a JSON-RPC error rather than a hung tool.
func TestBridgeReportsUnreachableGround(t *testing.T) {
	b := &Bridge{Endpoint: "http://127.0.0.1:1/mcp", Token: "t"}
	var out strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Run(ctx, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &out, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && out.Len() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("want an unreachable-ground error, got: %q", out.String())
	}
}

func TestBridgeSurfacesAuthFailure(t *testing.T) {
	srv, _ := stubGround(t, nil)
	b := &Bridge{Endpoint: srv.URL, Token: "wrong-token"}
	var out strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Run(ctx, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &out, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && out.Len() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "token") {
		t.Errorf("an auth failure should mention the token, got: %q", out.String())
	}
}

// A malformed frame must not kill the session: the CLI may recover.
func TestBridgeSurvivesGarbageInput(t *testing.T) {
	srv, _ := stubGround(t, nil)
	b := &Bridge{Endpoint: srv.URL, Token: "good-token"}
	var out safeWriter
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := b.Run(ctx, strings.NewReader("not json\n"+`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n"), &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), `"id":2`) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), `"id":2`) {
		t.Errorf("the bridge should keep serving after a bad frame, got: %q", out.String())
	}
}

type safeWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *safeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *safeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestExtractSSEData(t *testing.T) {
	got, err := extractSSEData([]byte("event: message\ndata: {\"ok\":true}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != `{"ok":true}` {
		t.Errorf("got %q", got)
	}
	if _, err := extractSSEData([]byte("event: message\n\n")); err == nil {
		t.Error("an event stream with no data field should error")
	}
}
