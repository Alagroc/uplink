package ground

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Alagroc/uplink/internal/mcp"
	"github.com/Alagroc/uplink/internal/proto"
)

// maxPollWait bounds a crew long-poll. Shorter than most idle proxy timeouts so
// tunnels stay healthy.
const maxPollWait = 30 * time.Second

// Server wires Ground to HTTP.
type Server struct {
	Ground  *Ground
	Token   string // operator + crew credential
	Version string
}

// Handler builds the mux. Two MCP endpoints with different credentials:
// /mcp holds the operator tools, /mcp/agent only the agent's channel tools, so
// a remote agent cannot dispatch jobs or read another crew's traffic.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// An empty token would make ConstantTimeCompare succeed for everyone, and
	// this is the only thing between a local process and remote execution on
	// every crew.
	if s.Token == "" {
		panic("ground: refusing to serve without an operator token")
	}

	operator := mcp.NewServer("uplink-ground", s.Version, OperatorInstructions)
	s.Ground.RegisterOperatorTools(operator)
	mux.Handle("/mcp", &mcp.HTTPHandler{
		Server: operator,
		Authorize: func(r *http.Request) (*http.Request, error) {
			if !s.validOperator(r) {
				return nil, fmt.Errorf("invalid or missing operator token")
			}
			return r, nil
		},
	})

	agent := mcp.NewServer("uplink-radio", s.Version, AgentInstructions)
	s.Ground.RegisterAgentTools(agent)
	mux.Handle("/mcp/agent", &mcp.HTTPHandler{
		Server: agent,
		Authorize: func(r *http.Request) (*http.Request, error) {
			job, ok := s.Ground.JobByAgentToken(mcp.BearerToken(r))
			if !ok {
				return nil, fmt.Errorf("invalid or expired job token")
			}
			return r.WithContext(WithJob(r.Context(), job)), nil
		},
	})

	mux.HandleFunc("/v1/crew/register", s.crewOnly(s.handleRegister))
	mux.HandleFunc("/v1/crew/poll", s.crewOnly(s.handlePoll))
	mux.HandleFunc("/v1/crew/logs", s.crewOnly(s.handleLogs))
	mux.HandleFunc("/v1/crew/state", s.crewOnly(s.handleState))
	mux.HandleFunc("/v1/health", s.handleHealth)

	return mux
}

func (s *Server) validOperator(r *http.Request) bool {
	got := mcp.BearerToken(r)
	// Constant-time compare: this is the only thing standing between a local
	// process and remote code execution on every crew.
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) crewOnly(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.validOperator(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="uplink"`)
			http.Error(w, "invalid or missing token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

func respond(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func fail(w http.ResponseWriter, status int, err error) {
	respond(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req proto.RegisterReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if req.Version != "" && req.Version != proto.Version {
		fail(w, http.StatusConflict, fmt.Errorf("crew speaks protocol %s, ground speaks %s: upgrade the crew binary", req.Version, proto.Version))
		return
	}
	resp, err := s.Ground.Register(req)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, http.StatusOK, resp)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	var req proto.PollReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	wait := time.Duration(req.WaitMS) * time.Millisecond
	if wait <= 0 || wait > maxPollWait {
		wait = maxPollWait
	}
	cmd, err := s.Ground.Poll(r.Context(), req.CrewID, wait)
	if err != nil {
		// 409 tells the crew to re-register rather than retry blindly.
		fail(w, http.StatusConflict, err)
		return
	}
	respond(w, http.StatusOK, cmd)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	var req proto.LogsReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	next, err := s.Ground.AppendLogs(req.CrewID, req.JobID, req.Lines)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, http.StatusOK, map[string]int64{"next_seq": next})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var req proto.JobStateReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Ground.SetJobState(req.CrewID, req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Unauthenticated on purpose so a crew can check the tunnel before it has a
	// token. Reveals nothing but liveness and protocol version.
	respond(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"protocol": proto.Version,
		"version":  s.Version,
	})
}

// HTTPServer returns a configured http.Server. WriteTimeout is deliberately
// zero: ask_operator holds a response open until a human answers, potentially
// for hours.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
}
