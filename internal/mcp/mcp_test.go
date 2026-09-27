package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer() *Server {
	s := NewServer("test", "1.0", "briefing")
	s.Add(Tool{
		Name:        "echo",
		Description: "echo back",
		Schema:      Schema{Props: map[string]Prop{"text": {Type: "string"}}, Required: []string{"text"}},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct{ Text string }
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			return "echo: " + a.Text, nil
		},
	})
	s.Add(Tool{
		Name:    "boom",
		Schema:  Schema{Props: map[string]Prop{}},
		Handler: func(context.Context, json.RawMessage) (string, error) { return "", errContext },
	})
	return s
}

var errContext = &RPCError{Code: 1, Message: "tool blew up"}

func call(t *testing.T, s *Server, method string, params any) *Response {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handle(context.Background(), &Request{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method, Params: raw})
}

func TestInitializeEchoesKnownProtocol(t *testing.T) {
	s := testServer()
	resp := call(t, s, "initialize", map[string]any{"protocolVersion": "2024-11-05"})
	result := resp.Result.(map[string]any)
	if got := result["protocolVersion"]; got != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want the client's own 2024-11-05", got)
	}
	if result["instructions"] != "briefing" {
		t.Errorf("instructions not passed through: %v", result["instructions"])
	}
}

func TestInitializeFallsBackForUnknownProtocol(t *testing.T) {
	s := testServer()
	resp := call(t, s, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if got := resp.Result.(map[string]any)["protocolVersion"]; got != LatestProtocol {
		t.Errorf("protocolVersion = %v, want %s", got, LatestProtocol)
	}
}

func TestToolsListPreservesRegistrationOrder(t *testing.T) {
	s := testServer()
	resp := call(t, s, "tools/list", map[string]any{})
	tools := resp.Result.(map[string]any)["tools"].([]toolDesc)
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "boom" {
		t.Fatalf("unexpected tool list: %+v", tools)
	}
}

func TestToolCallSuccess(t *testing.T) {
	s := testServer()
	resp := call(t, s, "tools/call", map[string]any{"name": "echo", "arguments": map[string]string{"text": "hi"}})
	result := resp.Result.(callResult)
	if result.IsError {
		t.Fatal("unexpected isError")
	}
	if result.Content[0].Text != "echo: hi" {
		t.Errorf("got %q", result.Content[0].Text)
	}
}

// A failing tool must come back as a tool result, not a JSON-RPC error: the
// model needs to read the failure and adapt.
func TestToolFailureIsAToolResult(t *testing.T) {
	s := testServer()
	resp := call(t, s, "tools/call", map[string]any{"name": "boom"})
	if resp.Error != nil {
		t.Fatalf("expected a result, got transport error %v", resp.Error)
	}
	result := resp.Result.(callResult)
	if !result.IsError {
		t.Error("isError should be set")
	}
	if !strings.Contains(result.Content[0].Text, "tool blew up") {
		t.Errorf("error text missing: %q", result.Content[0].Text)
	}
}

func TestUnknownToolIsMethodNotFound(t *testing.T) {
	s := testServer()
	resp := call(t, s, "tools/call", map[string]any{"name": "nope"})
	if resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("want method-not-found, got %+v", resp.Error)
	}
}

func TestNotificationGetsNoReply(t *testing.T) {
	s := testServer()
	resp := s.Handle(context.Background(), &Request{JSONRPC: "2.0", Method: "notifications/initialized"})
	if resp != nil {
		t.Fatalf("notifications must not be answered, got %+v", resp)
	}
}

func TestEmptyProbesReturnEmptyLists(t *testing.T) {
	s := testServer()
	for _, method := range []string{"resources/list", "prompts/list", "resources/templates/list"} {
		resp := call(t, s, method, map[string]any{})
		if resp.Error != nil {
			t.Errorf("%s returned error %v; clients probe these and should not see failures", method, resp.Error)
		}
	}
}

func TestSchemaMarshalsAsJSONSchema(t *testing.T) {
	s := Schema{
		Props: map[string]Prop{
			"kind":  {Type: "string", Enum: []string{"a", "b"}, Description: "which"},
			"tags":  {Type: "array", Items: "string"},
			"count": {Type: "integer", Default: 5},
		},
		Required: []string{"kind"},
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "object" {
		t.Error("schema must be an object type")
	}
	props := got["properties"].(map[string]any)
	if kind := props["kind"].(map[string]any); kind["enum"] == nil || kind["description"] != "which" {
		t.Errorf("enum/description lost: %+v", kind)
	}
	if tags := props["tags"].(map[string]any); tags["items"] == nil {
		t.Error("array items schema is required by strict clients")
	}
	if req := got["required"].([]any); len(req) != 1 || req[0] != "kind" {
		t.Errorf("required = %v", req)
	}
}

// A schema with no required arguments must still emit "required": [] — some
// clients reject the key being absent.
func TestSchemaAlwaysEmitsRequired(t *testing.T) {
	data, err := json.Marshal(Schema{Props: map[string]Prop{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"required":[]`) {
		t.Errorf("want an empty required array, got %s", data)
	}
}

// The stdio framing is newline-delimited. Its important property is recovery: a
// malformed frame must be skippable, because a streaming JSON decoder cannot
// resynchronise and spins on the same bytes forever.
func TestFrameReaderSkipsBlankLinesAndRecovers(t *testing.T) {
	fr := NewFrameReader(strings.NewReader("\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n" + "not json\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"))

	var got []string
	for {
		frame, err := fr.Next()
		if err != nil {
			break
		}
		got = append(got, string(frame))
	}
	if len(got) != 3 {
		t.Fatalf("want 3 frames (blank lines skipped, bad frame still delivered), got %d: %q", len(got), got)
	}
	if got[1] != "not json" {
		t.Errorf("the bad frame should be handed up for the caller to reject, got %q", got[1])
	}
	if !strings.Contains(got[2], `"id":2`) {
		t.Errorf("reading must continue past a bad frame, got %q", got[2])
	}
}

func TestFrameReaderEOF(t *testing.T) {
	fr := NewFrameReader(strings.NewReader(""))
	if _, err := fr.Next(); err == nil {
		t.Fatal("an empty stream should report EOF")
	}
}

// --- HTTP transport ---

func newHTTPTest(t *testing.T, authorize func(*http.Request) (*http.Request, error)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(&HTTPHandler{Server: testServer(), Authorize: authorize})
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPRejectsBadToken(t *testing.T) {
	srv := newHTTPTest(t, func(r *http.Request) (*http.Request, error) {
		if BearerToken(r) != "good" {
			return nil, errContext
		}
		return r, nil
	})
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("WWW-Authenticate header should be set on 401")
	}
}

func TestHTTPCallAndSessionHeader(t *testing.T) {
	srv := newHTTPTest(t, nil)
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Mcp-Session-Id") == "" {
		t.Error("initialize should return a session id")
	}
}

func TestHTTPNotificationIsAccepted(t *testing.T) {
	srv := newHTTPTest(t, nil)
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}

// A client that accepts only an event stream must still get its reply.
func TestHTTPSSEResponse(t *testing.T) {
	srv := newHTTPTest(t, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "data: ") {
		t.Errorf("SSE body missing data field: %q", body[:n])
	}
}

func TestHTTPGetIsRejected(t *testing.T) {
	srv := newHTTPTest(t, nil)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestBearerTokenSources(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	if got := BearerToken(r); got != "abc123" {
		t.Errorf("Authorization header: got %q", got)
	}
	r2, _ := http.NewRequest(http.MethodPost, "/", nil)
	r2.Header.Set("X-Uplink-Token", "xyz")
	if got := BearerToken(r2); got != "xyz" {
		t.Errorf("fallback header: got %q", got)
	}
}
