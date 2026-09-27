package ground

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// debugPeek caps how much of a request body is parsed to describe it. Prompts
// can be megabytes; the interesting part is always at the front.
const debugPeek = 8 << 10

// debugMiddleware logs one line per request: what was asked, by whom, the
// status and how long it took.
//
// It deliberately never logs headers. The operator token and every per-job
// token travel in Authorization, and a debug flag that quietly prints your
// credentials to a terminal — and into whatever scrollback or screenshot
// follows — would be a poor trade for visibility.
func (s *Server) debugMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Buffer the body so it can be described and still served to the
		// handler. Only in debug mode, and only up to the cap.
		var peek []byte
		if r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyDebug))
			r.Body.Close()
			if err == nil {
				r.Body = io.NopCloser(bytes.NewReader(body))
				if len(body) > debugPeek {
					peek = body[:debugPeek]
				} else {
					peek = body
				}
			}
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		elapsed := time.Since(start)
		marker := " "
		if rec.status >= 400 {
			marker = "!"
		}
		s.debugf("%s %-4s %-16s %-38s %3d  %9s  %s",
			marker,
			r.Method,
			r.URL.Path,
			s.describeRequest(r.URL.Path, peek),
			rec.status,
			formatDuration(elapsed),
			formatBytes(rec.written),
		)
	})
}

// maxBodyDebug bounds debug buffering independently of the handlers' own caps.
const maxBodyDebug = 16 << 20

func (s *Server) debugf(format string, args ...any) {
	if s.Logf == nil {
		return
	}
	s.Logf(format, args...)
}

// describeRequest summarises what a request is asking for, in the vocabulary of
// the thing being debugged rather than of HTTP.
func (s *Server) describeRequest(path string, body []byte) string {
	switch path {
	case "/mcp", "/mcp/agent":
		return describeRPC(body)
	case "/v1/crew/poll":
		var req struct {
			CrewID string `json:"crew_id"`
		}
		_ = json.Unmarshal(body, &req)
		return "crew=" + s.crewLabel(req.CrewID)
	case "/v1/crew/register":
		var req struct {
			Name    string   `json:"name"`
			Roles   []string `json:"roles"`
			Runners []string `json:"runners"`
		}
		_ = json.Unmarshal(body, &req)
		desc := "crew=" + orUnknown(req.Name)
		if len(req.Runners) > 0 {
			desc += " runners=" + strings.Join(req.Runners, ",")
		} else {
			desc += " runners=none"
		}
		return desc
	case "/v1/crew/logs":
		var req struct {
			CrewID string     `json:"crew_id"`
			JobID  string     `json:"job_id"`
			Lines  []struct{} `json:"lines"`
		}
		_ = json.Unmarshal(body, &req)
		return fmt.Sprintf("crew=%s %s %d lines", s.crewLabel(req.CrewID), shortID(req.JobID), len(req.Lines))
	case "/v1/crew/state":
		var req struct {
			CrewID string `json:"crew_id"`
			JobID  string `json:"job_id"`
			State  string `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		return fmt.Sprintf("crew=%s %s -> %s", s.crewLabel(req.CrewID), shortID(req.JobID), req.State)
	case "/v1/shutdown":
		return "shutdown requested"
	case "/v1/health":
		return "health"
	}
	return ""
}

// describeRPC names the JSON-RPC method, and the tool for a tools/call.
func describeRPC(body []byte) string {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return "batch"
	}
	if json.Unmarshal(trimmed, &msg) != nil {
		return "unparsed"
	}
	if msg.Method == "tools/call" && msg.Params.Name != "" {
		return "tools/call " + msg.Params.Name
	}
	return orUnknown(msg.Method)
}

// crewLabel turns a crew id into its name, which is what the operator knows it
// by. Falls back to the id when the crew is gone — which is itself the answer
// to "why is this crew getting errors?".
func (s *Server) crewLabel(crewID string) string {
	if crewID == "" {
		return "?"
	}
	if name, ok := s.Ground.CrewName(crewID); ok {
		return name
	}
	return shortID(crewID) + "(unregistered)"
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// shortID trims the random half of an id, keeping enough to correlate lines.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	case d < time.Second:
		return fmt.Sprintf("%.0fms", float64(d.Milliseconds()))
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

func formatBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1fK", float64(n)/1024)
}

// statusRecorder captures the status and size of a response while passing
// everything else through.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
	wrote   bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// Flush keeps the SSE path working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
