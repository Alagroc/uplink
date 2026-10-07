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

	// RequestShutdown, if set, is called when an authenticated shutdown request
	// is accepted. It runs after the response has been handed back, so the
	// caller learns the outcome before the process goes away.
	RequestShutdown func(reason string)

	// CrewTokens holds the per-crew credentials. Crew endpoints are closed
	// without it.
	CrewTokens *CrewTokenStore

	// Debug logs one line per request through Logf. Off by default: it buffers
	// request bodies to describe them.
	Debug bool
	Logf  func(format string, args ...any)
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
			if err := s.operatorAuthError(r); err != nil {
				return nil, err
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

	mux.HandleFunc("/v1/crew/register", s.crewAuth(s.handleRegister))
	mux.HandleFunc("/v1/crew/poll", s.crewAuth(s.handlePoll))
	mux.HandleFunc("/v1/crew/logs", s.crewAuth(s.handleLogs))
	mux.HandleFunc("/v1/crew/state", s.crewAuth(s.handleState))
	mux.HandleFunc("/v1/shutdown", s.operatorOnly(s.handleShutdown))
	mux.HandleFunc("/v1/health", s.handleHealth)

	if s.Debug {
		return s.debugMiddleware(mux)
	}
	return mux
}

func (s *Server) validOperator(r *http.Request) bool {
	got := mcp.BearerToken(r)
	// Constant-time compare: this is the only thing standing between a local
	// process and remote code execution on every crew.
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

// operatorAuthError validates the operator credential, naming the most likely
// mistake now that crew have their own.
func (s *Server) operatorAuthError(r *http.Request) error {
	if s.validOperator(r) {
		return nil
	}
	if IsCrewToken(mcp.BearerToken(r)) {
		return fmt.Errorf("that is a crew token; this endpoint needs the operator token")
	}
	return fmt.Errorf("invalid or missing operator token")
}

// operatorOnly guards the surfaces that can dispatch work and stop the daemon.
func (s *Server) operatorOnly(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		if err := s.operatorAuthError(r); err != nil {
			unauthorized(w, err.Error())
			return
		}
		h(w, r)
	}
}

// crewAuth authenticates a crew and hands its credential to the handler.
//
// This is the credential split: a crew token opens only these endpoints, and
// only for the one crew name it was minted for. Before this existed, every crew
// held the operator token and could dispatch jobs to other crew, read the whole
// inbox, poll another crew's queue, and shut ground down.
func (s *Server) crewAuth(h func(http.ResponseWriter, *http.Request, *CrewToken)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		presented := mcp.BearerToken(r)
		if presented == "" {
			unauthorized(w, "no token: set UPLINK_TOKEN on this crew host to a token from `uplink crew-token add <name>`")
			return
		}
		if s.CrewTokens == nil {
			unauthorized(w, "this ground has no crew token store configured")
			return
		}

		token, ok := s.CrewTokens.Lookup(presented)
		if !ok {
			// The most likely mistake, now that the two are different.
			if subtle.ConstantTimeCompare([]byte(presented), []byte(s.Token)) == 1 {
				unauthorized(w, "the operator token no longer works for crew. Mint a crew credential on the ground machine:\n"+
					"  uplink crew-token add <name> --role <role>\n"+
					"then set UPLINK_TOKEN to it on this host")
				return
			}
			if s.CrewTokens.Count() == 0 {
				unauthorized(w, "no crew tokens have been minted yet; on the ground machine run: uplink crew-token add <name>")
				return
			}
			unauthorized(w, "unrecognised crew token")
			return
		}
		if token.Revoked() {
			unauthorized(w, fmt.Sprintf("the token for crew %q was revoked at %s", token.Name, token.RevokedAt.Format(time.RFC3339)))
			return
		}
		h(w, r, token)
	}
}

func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="uplink"`)
	http.Error(w, reason, http.StatusUnauthorized)
}

// ownedBy reports whether a crew_id from a request body belongs to the
// authenticated crew. Without this a crew could poll another crew's queue and
// take its jobs, agent tokens included.
func (s *Server) ownedBy(crewID string, token *CrewToken) error {
	if crewID == "" {
		return fmt.Errorf("crew_id is required")
	}
	name, ok := s.Ground.CrewName(crewID)
	if !ok {
		return fmt.Errorf("unknown crew %q: re-register", crewID)
	}
	if name != token.Name {
		return fmt.Errorf("crew %q does not belong to this token", name)
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	// Unknown fields are ignored on purpose. Ground and crew are separate
	// binaries on separate machines and will be upgraded at different times, so
	// rejecting a field the sender added is a breaking change for every future
	// release: a newer crew simply cannot talk to an older ground, with a 400
	// that names a field rather than the version skew behind it. Tolerating
	// what we do not understand is what makes a rolling upgrade possible.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
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

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request, token *CrewToken) {
	var req proto.RegisterReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// The token names the crew, so a stolen credential cannot masquerade as a
	// different one.
	if req.Name != token.Name {
		fail(w, http.StatusForbidden, fmt.Errorf("this token is for crew %q, not %q", token.Name, req.Name))
		return
	}
	// Roles on the token are authoritative when present.
	if len(token.Roles) > 0 {
		req.Roles = token.Roles
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

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, token *CrewToken) {
	var req proto.PollReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ownedBy(req.CrewID, token); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	wait := time.Duration(req.WaitMS) * time.Millisecond
	if wait <= 0 || wait > maxPollWait {
		wait = maxPollWait
	}
	cmd, err := s.Ground.Poll(r.Context(), req.CrewID, wait)
	if err != nil {
		if r.Context().Err() != nil {
			// The crew hung up mid-poll, which is ordinary: it restarted, or
			// the tunnel dropped. Nobody is listening for a reply, and calling
			// it a conflict would tell the next reader to look for a problem
			// that is not there.
			return
		}
		// 409 tells the crew to re-register rather than retry blindly.
		fail(w, http.StatusConflict, err)
		return
	}
	respond(w, http.StatusOK, cmd)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, token *CrewToken) {
	var req proto.LogsReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ownedBy(req.CrewID, token); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	next, err := s.Ground.AppendLogs(req.CrewID, req.JobID, req.Lines)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, http.StatusOK, map[string]int64{"next_seq": next})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, token *CrewToken) {
	var req proto.JobStateReq
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ownedBy(req.CrewID, token); err != nil {
		fail(w, http.StatusForbidden, err)
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

// handleShutdown stops ground on request.
//
// Losing ground mid-flight costs work: a running job is marked failed on the
// next start, and an agent blocked on a question is left without an answer. So
// the default refuses while anything is in flight, and says what.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force  bool   `json:"force"`
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if s.RequestShutdown == nil {
		fail(w, http.StatusNotImplemented, fmt.Errorf("this ground was not started with shutdown support"))
		return
	}

	active := s.Ground.Jobs("", true, 0)
	pending := s.Ground.Inbox(true, 0)

	if !req.Force && (len(active) > 0 || len(pending) > 0) {
		respond(w, http.StatusConflict, map[string]any{
			"error":            "refusing to shut down while work is in flight; pass force to override",
			"active_jobs":      jobSummaries(active),
			"pending_question": len(pending),
		})
		return
	}

	reason := req.Reason
	if reason == "" {
		reason = "shutdown requested by operator"
	}
	s.Ground.store.Auditf("shutdown: %s (active_jobs=%d pending_questions=%d force=%v)",
		reason, len(active), len(pending), req.Force)

	respond(w, http.StatusOK, map[string]any{
		"status":            "shutting down",
		"abandoned_jobs":    jobSummaries(active),
		"abandoned_pending": len(pending),
	})

	// Shut down after this handler returns, so the reply is delivered.
	go s.RequestShutdown(reason)
}

func jobSummaries(jobs []proto.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		label := j.Label
		if label == "" {
			label = j.Kind
		}
		out = append(out, fmt.Sprintf("%s (%s, crew=%s, %s)", j.ID, j.State, j.CrewName, label))
	}
	return out
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
