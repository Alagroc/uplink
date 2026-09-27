package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxBody caps an inbound MCP request. Prompts can be long; 8 MiB is generous
// while still refusing a runaway client.
const maxBody = 8 << 20

// HTTPHandler serves the MCP streamable-HTTP transport for a Server.
//
// Auth is delegated to Authorize, which returns a context carrying whatever the
// tools need (for the agent endpoint, the job the bearer token belongs to).
type HTTPHandler struct {
	Server *Server
	// Authorize validates the request and may enrich the request context.
	// Returning an error rejects the request with 401.
	Authorize func(r *http.Request) (*http.Request, error)
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		// handled below
	case http.MethodDelete:
		// Session teardown: uplink keeps no per-session state, so just ack.
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		// uplink never initiates requests, so there is no stream to open.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "server-initiated streams are not supported", http.StatusMethodNotAllowed)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.Authorize != nil {
		authed, err := h.Authorize(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="uplink"`)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		r = authed
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	reqs, batch, err := decodeMessages(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, &Response{
			JSONRPC: "2.0",
			Error:   &RPCError{Code: CodeParse, Message: err.Error()},
		})
		return
	}

	var out []*Response
	for _, req := range reqs {
		if resp := h.Server.Handle(r.Context(), req); resp != nil {
			out = append(out, resp)
		}
	}

	// A session id lets clients correlate calls; uplink is stateless across
	// requests but returning one keeps spec-compliant clients happy.
	if isInitialize(reqs) {
		w.Header().Set("Mcp-Session-Id", newSessionID())
	}

	if len(out) == 0 {
		// Notifications only.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var payload any = out[0]
	if batch {
		payload = out
	}

	// Clients that only accept an event stream get the reply as a single SSE
	// event; everyone else gets plain JSON.
	if wantsSSE(r) {
		writeSSE(w, payload)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func isInitialize(reqs []*Request) bool {
	for _, req := range reqs {
		if req.Method == "initialize" {
			return true
		}
	}
	return false
}

// wantsSSE reports whether the client accepts text/event-stream but not JSON.
func wantsSSE(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if !strings.Contains(accept, "text/event-stream") {
		return false
	}
	return !strings.Contains(accept, "application/json") && !strings.Contains(accept, "*/*")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSSE(w http.ResponseWriter, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// BearerToken extracts a bearer token from the Authorization header, falling
// back to the X-Uplink-Token header for clients that cannot set Authorization.
func BearerToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
			return strings.TrimSpace(auth[7:])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Uplink-Token"))
}
