// Package ground implements mission control: the hub that holds crew
// registrations, dispatches jobs, and brokers questions between remote agents
// and the operator's CLI.
package ground

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
	"github.com/Alagroc/uplink/internal/store"
)

const (
	// logsInMemory caps how many recent lines per job stay resident, and
	// logBytesInMemory caps their total size. Everything is still written to
	// the log file; these only bound what job_logs can return.
	logsInMemory     = 4000
	logBytesInMemory = 4 << 20

	// retainedFinishedJobs is how many finished jobs keep their log buffer in
	// memory. Beyond that the oldest are dropped: ground is a long-running
	// daemon, and the lines remain durable in logs.jsonl.
	retainedFinishedJobs = 50
)

// Options configures a Ground.
type Options struct {
	// OfflineAfter marks a crew offline when it has not polled for this long.
	OfflineAfter time.Duration
	// NotifyCmd, if set, is run as `sh -c NotifyCmd` when a question arrives,
	// with UPLINK_* variables in its environment. This is how the operator
	// finds out a bot is waiting, since MCP cannot push to the model.
	NotifyCmd string
	// Bell writes an ASCII BEL to ground's stderr on a new question.
	Bell bool
}

type crewEntry struct {
	info  proto.Crew
	queue []proto.Command
	wake  chan struct{}
}

type questionEntry struct {
	q    proto.Question
	done chan struct{}
}

// Ground is the hub. All exported methods are safe for concurrent use.
type Ground struct {
	opts  Options
	store *store.Store

	mu        sync.Mutex
	crew      map[string]*crewEntry      // crew id -> entry
	byName    map[string]string          // crew name -> crew id
	jobs      map[string]*proto.Job      // job id -> job
	jobOrder  []string                   // job ids, oldest first
	byToken   map[string]string          // agent token -> job id
	logs      map[string][]proto.LogLine // job id -> recent lines
	logSeq    map[string]int64           // job id -> next seq
	questions map[string]*questionEntry  // question id -> entry
	qOrder    []string                   // question ids, oldest first
	messages  map[string][]proto.Message // job id -> undelivered messages
	inFlight  map[string][]proto.Message // job id -> handed out, not yet confirmed
	finished  []string                   // finished job ids, oldest first

	stderr func(string)
}

// New creates a Ground and restores state from the store.
func New(st *store.Store, opts Options, stderr func(string)) *Ground {
	if opts.OfflineAfter <= 0 {
		opts.OfflineAfter = 90 * time.Second
	}
	if stderr == nil {
		stderr = func(string) {}
	}
	g := &Ground{
		opts:      opts,
		store:     st,
		crew:      map[string]*crewEntry{},
		byName:    map[string]string{},
		jobs:      map[string]*proto.Job{},
		byToken:   map[string]string{},
		logs:      map[string][]proto.LogLine{},
		logSeq:    map[string]int64{},
		questions: map[string]*questionEntry{},
		messages:  map[string][]proto.Message{},
		inFlight:  map[string][]proto.Message{},
		stderr:    stderr,
	}
	g.restore()
	return g
}

// restore replays the event log so that jobs and unanswered questions survive a
// ground restart. Crew re-register on their own, so they are not restored.
func (g *Ground) restore() {
	_ = g.store.Replay(func(ev store.Event) {
		switch ev.Kind {
		case store.KindJob:
			var j proto.Job
			if json.Unmarshal(ev.Payload, &j) != nil || j.ID == "" {
				return
			}
			if _, seen := g.jobs[j.ID]; !seen {
				g.jobOrder = append(g.jobOrder, j.ID)
			}
			// A job that was running when ground died cannot be resumed: the
			// crew will re-register without it.
			if j.State == proto.StateRunning || j.State == proto.StateQueued {
				j.State = proto.StateFailed
				j.Error = "interrupted: ground restarted"
			}
			copied := j
			g.jobs[j.ID] = &copied
		case store.KindQuestion:
			var q proto.Question
			if json.Unmarshal(ev.Payload, &q) != nil || q.ID == "" {
				return
			}
			entry, seen := g.questions[q.ID]
			if !seen {
				g.qOrder = append(g.qOrder, q.ID)
				entry = &questionEntry{done: make(chan struct{})}
				g.questions[q.ID] = entry
			}
			entry.q = q
			if q.State == proto.QPending {
				// The agent that asked is unreachable: its per-job token is not
				// restored, so its radio gets 401 on every call. Keep the
				// question visible as history, but not as something answerable.
				now := time.Now().UTC()
				entry.q.State = proto.QExpired
				entry.q.Answer = "released: ground restarted while this question was open"
				entry.q.AnsweredAt = &now
			}
			closeOnce(entry.done)
		}
	})
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func newToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// --- crew lifecycle ---

// Register adds or refreshes a crew. Re-registering under an existing name
// replaces that crew, which is what happens when a worker restarts.
func (g *Ground) Register(req proto.RegisterReq) (proto.RegisterResp, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return proto.RegisterResp{}, fmt.Errorf("crew name is required")
	}
	if strings.ContainsAny(name, " \t\n") {
		return proto.RegisterResp{}, fmt.Errorf("crew name must not contain whitespace")
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if old, ok := g.byName[name]; ok {
		delete(g.crew, old)
	}
	id := newID("crew")
	now := time.Now().UTC()
	entry := &crewEntry{
		info: proto.Crew{
			ID: id, Name: name, Roles: req.Roles, OS: req.OS, Arch: req.Arch,
			Hostname: req.Hostname, Workdir: req.Workdir, Runners: req.Runners,
			Version: req.Version, JoinedAt: now, LastSeen: now,
		},
		wake: make(chan struct{}, 1),
	}
	g.crew[id] = entry
	g.byName[name] = id

	// Re-point every unfinished job for this name at the new crew id. A crew
	// keeps running its jobs across a reconnect, so without this the jobs it is
	// still working on would be owned by an id that no longer exists and every
	// log line and state report from them would be rejected.
	for _, jid := range g.jobOrder {
		j := g.jobs[jid]
		if j.CrewName != name || j.Terminal() {
			continue
		}
		j.CrewID = id
		switch {
		case j.State == proto.StateQueued:
			g.enqueue(entry, proto.Command{Type: proto.CmdJob, Job: j})
		case j.CancelRequested:
			// A cancel queued for the previous connection was lost with it.
			g.enqueue(entry, proto.Command{Type: proto.CmdCancel, JobID: j.ID, Reason: "cancelled by operator"})
		}
	}

	g.store.Auditf("crew %q registered from %s (%s/%s) roles=%v", name, req.Hostname, req.OS, req.Arch, req.Roles)
	_ = g.store.Append(store.KindCrew, entry.info)
	return proto.RegisterResp{CrewID: id, GroundVersion: proto.Version}, nil
}

// crewNameLocked resolves a crew id to its name. Caller holds mu.
func (g *Ground) crewNameLocked(crewID string) (string, bool) {
	entry, ok := g.crew[crewID]
	if !ok {
		return "", false
	}
	return entry.info.Name, true
}

// CrewName resolves a crew id to the name the operator knows it by.
func (g *Ground) CrewName(crewID string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.crewNameLocked(crewID)
}

// ownsJobLocked reports whether a crew may report on a job.
//
// Ownership is by crew NAME, not id. A crew that reconnects is issued a new id
// while the jobs it is running carry on, so an id comparison alone would reject
// everything that crew says about work it is genuinely doing. An empty crewID
// means an internal caller (the agent-side tools), which is already
// authenticated by its per-job token.
func (g *Ground) ownsJobLocked(crewID string, j *proto.Job) bool {
	if crewID == "" || j.CrewID == crewID {
		return true
	}
	name, ok := g.crewNameLocked(crewID)
	return ok && name == j.CrewName
}

// Poll blocks until a command is available for the crew, the wait elapses, or
// the context is cancelled. It returns a noop command on timeout, which the
// crew treats as a heartbeat.
func (g *Ground) Poll(ctx context.Context, crewID string, wait time.Duration) (proto.Command, error) {
	g.mu.Lock()
	entry, ok := g.crew[crewID]
	if !ok {
		g.mu.Unlock()
		return proto.Command{}, fmt.Errorf("unknown crew %q: re-register", crewID)
	}
	entry.info.LastSeen = time.Now().UTC()
	if len(entry.queue) > 0 {
		cmd := entry.queue[0]
		entry.queue = entry.queue[1:]
		g.mu.Unlock()
		return cmd, nil
	}
	wake := entry.wake
	g.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		// The crew hung up; there is nobody to answer.
		return proto.Command{}, ctx.Err()
	case <-timer.C:
		return proto.Command{Type: proto.CmdNoop}, nil
	case <-wake:
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok = g.crew[crewID]
	if !ok {
		return proto.Command{}, fmt.Errorf("unknown crew %q: re-register", crewID)
	}
	if len(entry.queue) == 0 {
		return proto.Command{Type: proto.CmdNoop}, nil
	}
	cmd := entry.queue[0]
	entry.queue = entry.queue[1:]
	return cmd, nil
}

// enqueue appends a command for a crew and wakes its poller. Caller holds mu.
func (g *Ground) enqueue(entry *crewEntry, cmd proto.Command) {
	entry.queue = append(entry.queue, cmd)
	select {
	case entry.wake <- struct{}{}:
	default: // a wake is already pending
	}
}

// CrewView is a crew plus derived status, as reported to the operator.
type CrewView struct {
	proto.Crew
	Online      bool     `json:"online"`
	ActiveJobs  []string `json:"active_jobs,omitempty"`
	LastSeenAgo string   `json:"last_seen_ago"`
}

// ListCrew returns every registered crew, newest registration last.
func (g *Ground) ListCrew() []CrewView {
	g.mu.Lock()
	defer g.mu.Unlock()

	active := map[string][]string{}
	for _, jid := range g.jobOrder {
		j := g.jobs[jid]
		if !j.Terminal() {
			active[j.CrewID] = append(active[j.CrewID], j.ID)
		}
	}

	out := make([]CrewView, 0, len(g.crew))
	now := time.Now().UTC()
	for _, e := range g.crew {
		out = append(out, CrewView{
			Crew:        e.info,
			Online:      now.Sub(e.info.LastSeen) < g.opts.OfflineAfter,
			ActiveJobs:  active[e.info.ID],
			LastSeenAgo: now.Sub(e.info.LastSeen).Round(time.Second).String(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- jobs ---

// SubmitReq asks ground to dispatch work.
type SubmitReq struct {
	Crew    string // exact crew name; takes precedence over Role
	Role    string // any online crew advertising this role
	Kind    string
	Label   string
	Payload proto.Payload
}

// Submit validates the request, picks a crew and queues the job.
func (g *Ground) Submit(req SubmitReq) (*proto.Job, error) {
	switch req.Kind {
	case proto.KindExec:
		if strings.TrimSpace(req.Payload.Command) == "" {
			return nil, fmt.Errorf("kind=exec needs a command")
		}
	case proto.KindAgent:
		if strings.TrimSpace(req.Payload.Prompt) == "" {
			return nil, fmt.Errorf("kind=agent needs a prompt")
		}
	default:
		return nil, fmt.Errorf("unknown kind %q (want %q or %q)", req.Kind, proto.KindExec, proto.KindAgent)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	entry, err := g.pickCrewLocked(req.Crew, req.Role)
	if err != nil {
		return nil, err
	}

	job := &proto.Job{
		ID:         newID("job"),
		CrewID:     entry.info.ID,
		CrewName:   entry.info.Name,
		Kind:       req.Kind,
		Label:      req.Label,
		Payload:    req.Payload,
		State:      proto.StateQueued,
		AgentToken: newToken(),
		CreatedAt:  time.Now().UTC(),
	}
	g.jobs[job.ID] = job
	g.jobOrder = append(g.jobOrder, job.ID)
	g.byToken[job.AgentToken] = job.ID

	g.enqueue(entry, proto.Command{Type: proto.CmdJob, Job: job})
	g.persistJobLocked(job)
	g.store.Auditf("job %s (%s) queued for crew %q: %s", job.ID, job.Kind, job.CrewName, firstLine(req.Payload.Command+req.Payload.Prompt))
	return job, nil
}

// pickCrewLocked resolves a crew by name or role. Caller holds mu.
func (g *Ground) pickCrewLocked(name, role string) (*crewEntry, error) {
	if name != "" {
		id, ok := g.byName[name]
		if !ok {
			return nil, fmt.Errorf("no crew named %q; registered: %s", name, strings.Join(g.crewNamesLocked(), ", "))
		}
		entry, ok := g.crew[id]
		if !ok {
			return nil, fmt.Errorf("crew %q is not connected", name)
		}
		return entry, nil
	}
	if role == "" {
		// A single registered crew needs no disambiguation.
		if len(g.crew) == 1 {
			for _, e := range g.crew {
				return e, nil
			}
		}
		return nil, fmt.Errorf("specify crew or role; registered: %s", strings.Join(g.crewNamesLocked(), ", "))
	}

	now := time.Now().UTC()
	load := map[string]int{}
	for _, jid := range g.jobOrder {
		if j := g.jobs[jid]; !j.Terminal() {
			load[j.CrewID]++
		}
	}
	var best *crewEntry
	for _, e := range g.crew {
		if now.Sub(e.info.LastSeen) >= g.opts.OfflineAfter {
			continue
		}
		if !hasRole(e.info.Roles, role) {
			continue
		}
		if best == nil || load[e.info.ID] < load[best.info.ID] {
			best = e
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no online crew with role %q; registered: %s", role, strings.Join(g.crewNamesLocked(), ", "))
	}
	return best, nil
}

func (g *Ground) crewNamesLocked() []string {
	names := make([]string, 0, len(g.byName))
	for n := range g.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"(none)"}
	}
	return names
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if strings.EqualFold(r, want) {
			return true
		}
	}
	return false
}

func (g *Ground) persistJobLocked(job *proto.Job) {
	redacted := *job
	redacted.AgentToken = "" // never write a live credential to the log
	_ = g.store.Append(store.KindJob, redacted)
}

// pruneFinishedLocked drops the log buffers of the oldest finished jobs.
func (g *Ground) pruneFinishedLocked() {
	for len(g.finished) > retainedFinishedJobs {
		oldest := g.finished[0]
		g.finished = g.finished[1:]
		delete(g.logs, oldest)
	}
}

// Job returns a copy of one job.
func (g *Ground) Job(id string) (proto.Job, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	j, ok := g.jobs[id]
	if !ok {
		return proto.Job{}, false
	}
	out := *j
	out.AgentToken = ""
	return out, true
}

// JobByAgentToken resolves the per-job agent credential.
func (g *Ground) JobByAgentToken(token string) (proto.Job, bool) {
	if token == "" {
		return proto.Job{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.byToken[token]
	if !ok {
		return proto.Job{}, false
	}
	j, ok := g.jobs[id]
	if !ok {
		return proto.Job{}, false
	}
	out := *j
	out.AgentToken = ""
	return out, true
}

// Jobs lists jobs, newest first. activeOnly skips finished jobs.
func (g *Ground) Jobs(crewName string, activeOnly bool, limit int) []proto.Job {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []proto.Job{}
	for i := len(g.jobOrder) - 1; i >= 0; i-- {
		j := g.jobs[g.jobOrder[i]]
		if crewName != "" && j.CrewName != crewName {
			continue
		}
		if activeOnly && j.Terminal() {
			continue
		}
		copied := *j
		copied.AgentToken = ""
		out = append(out, copied)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// SetJobState records a state transition reported by a crew.
func (g *Ground) SetJobState(crewID string, req proto.JobStateReq) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	j, ok := g.jobs[req.JobID]
	if !ok {
		return fmt.Errorf("unknown job %q", req.JobID)
	}
	if !g.ownsJobLocked(crewID, j) {
		return fmt.Errorf("job %q does not belong to this crew", req.JobID)
	}
	if j.Terminal() {
		// A late or retried "running" must not resurrect a finished job: its
		// agent token is already retired, so it would be stuck running forever.
		if req.State == proto.StateRunning {
			return fmt.Errorf("job %s is already %s", req.JobID, j.State)
		}
		return nil
	}
	now := time.Now().UTC()
	switch req.State {
	case proto.StateRunning:
		if j.StartedAt == nil {
			j.StartedAt = &now
		}
	case proto.StateDone, proto.StateFailed, proto.StateCanceled:
		if j.EndedAt == nil {
			j.EndedAt = &now
		}
		delete(g.byToken, j.AgentToken) // retire the credential with the job
		g.expireQuestionsLocked(j.ID, "job finished")
		g.finished = append(g.finished, j.ID)
		g.pruneFinishedLocked()
	default:
		return fmt.Errorf("invalid state %q", req.State)
	}
	j.State = req.State
	j.ExitCode = req.ExitCode
	if req.Error != "" {
		j.Error = req.Error
	}
	g.persistJobLocked(j)
	return nil
}

// Complete is the agent declaring the task finished, with a summary.
func (g *Ground) Complete(jobID, summary string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	j, ok := g.jobs[jobID]
	if !ok {
		return fmt.Errorf("unknown job %q", jobID)
	}
	j.Summary = summary
	g.persistJobLocked(j)
	return nil
}

// Cancel asks the owning crew to stop a job.
func (g *Ground) Cancel(jobID, reason string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	j, ok := g.jobs[jobID]
	if !ok {
		return fmt.Errorf("unknown job %q", jobID)
	}
	if j.Terminal() {
		return fmt.Errorf("job %s already %s", jobID, j.State)
	}
	if j.CancelRequested {
		return fmt.Errorf("job %s already has a cancel pending", jobID)
	}
	j.CancelRequested = true
	entry, ok := g.crew[j.CrewID]
	if !ok {
		// Crew is gone; mark it cancelled locally.
		now := time.Now().UTC()
		j.State = proto.StateCanceled
		j.EndedAt = &now
		j.Error = "crew offline: " + reason
		g.persistJobLocked(j)
		g.expireQuestionsLocked(jobID, "job cancelled")
		return nil
	}
	g.enqueue(entry, proto.Command{Type: proto.CmdCancel, JobID: jobID, Reason: reason})
	g.store.Auditf("job %s cancel requested: %s", jobID, reason)
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}
	if len(s) > 160 {
		s = s[:160] + " ..."
	}
	return s
}

// --- logs ---

// AppendLogs records output from a job.
func (g *Ground) AppendLogs(crewID, jobID string, lines []proto.LogLine) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	j, ok := g.jobs[jobID]
	if !ok {
		return 0, fmt.Errorf("unknown job %q", jobID)
	}
	if !g.ownsJobLocked(crewID, j) {
		return 0, fmt.Errorf("job %q does not belong to this crew", jobID)
	}
	recs := make([]proto.LogLine, 0, len(lines))
	for _, ln := range lines {
		seq := g.logSeq[jobID]
		g.logSeq[jobID] = seq + 1
		rec := proto.LogLine{Seq: seq, JobID: jobID, Stream: ln.Stream, Text: ln.Text, At: ln.At}
		if rec.At.IsZero() {
			rec.At = time.Now().UTC()
		}
		if rec.Stream == "" {
			rec.Stream = proto.StreamStdout
		}
		recs = append(recs, rec)
	}
	g.logs[jobID] = trimLogBuffer(append(g.logs[jobID], recs...))
	next := g.logSeq[jobID]
	g.mu.Unlock()

	// Persist outside the lock: this is disk I/O, and every crew heartbeat and
	// operator reply needs the same mutex.
	for _, rec := range recs {
		_ = g.store.Append(store.KindLog, rec)
	}

	g.mu.Lock()
	return next, nil
}

// trimLogBuffer bounds a job's resident log buffer by both line count and total
// bytes. A line cap alone is not a memory bound: lines can be 8 KB each.
func trimLogBuffer(buf []proto.LogLine) []proto.LogLine {
	if len(buf) > logsInMemory {
		buf = buf[len(buf)-logsInMemory:]
	}
	bytes := 0
	for i := len(buf) - 1; i >= 0; i-- {
		bytes += len(buf[i].Text)
		if bytes > logBytesInMemory {
			return buf[i+1:]
		}
	}
	return buf
}

// Logs returns buffered lines with Seq >= since, capped at limit.
func (g *Ground) Logs(jobID string, since int64, limit int, stream string) ([]proto.LogLine, int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.jobs[jobID]; !ok {
		return nil, 0, fmt.Errorf("unknown job %q", jobID)
	}
	switch {
	case limit <= 0:
		limit = 200
	case limit > logsInMemory:
		limit = logsInMemory
	}
	all := g.logs[jobID]
	out := make([]proto.LogLine, 0, limit)
	for _, ln := range all {
		if ln.Seq < since {
			continue
		}
		if stream != "" && ln.Stream != stream {
			continue
		}
		out = append(out, ln)
	}
	// Keep the tail when the window overflows: recent output matters most.
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, g.logSeq[jobID], nil
}

// --- questions (the downlink) ---

// Ask registers a question from an agent and returns it. The caller then waits
// via Await.
func (g *Ground) Ask(job proto.Job, req proto.AskReq) (proto.Question, error) {
	if strings.TrimSpace(req.Question) == "" {
		return proto.Question{}, fmt.Errorf("question text is required")
	}
	urgency := req.Urgency
	if urgency == "" {
		urgency = "normal"
	}

	g.mu.Lock()
	q := proto.Question{
		ID: newID("q"), JobID: job.ID, CrewName: job.CrewName,
		Question: req.Question, Context: req.Context, Options: req.Options,
		Urgency: urgency, State: proto.QPending, AskedAt: time.Now().UTC(),
	}
	g.questions[q.ID] = &questionEntry{q: q, done: make(chan struct{})}
	g.qOrder = append(g.qOrder, q.ID)
	_ = g.store.Append(store.KindQuestion, q)
	g.store.Auditf("question %s from crew %q job %s: %s", q.ID, job.CrewName, job.ID, firstLine(req.Question))
	g.mu.Unlock()

	g.notify(q)
	return q, nil
}

// notify tells the human a bot is waiting. MCP cannot push to the model, so
// this is the path by which the operator learns to check the inbox.
func (g *Ground) notify(q proto.Question) {
	line := fmt.Sprintf("[uplink] %s is waiting: %s", q.CrewName, firstLine(q.Question))
	if g.opts.Bell {
		g.stderr("\a" + line)
	} else {
		g.stderr(line)
	}
	if g.opts.NotifyCmd == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", g.opts.NotifyCmd)
		cmd.Env = append(cmd.Environ(),
			"UPLINK_CREW="+q.CrewName,
			"UPLINK_JOB="+q.JobID,
			"UPLINK_QUESTION_ID="+q.ID,
			"UPLINK_QUESTION="+q.Question,
			"UPLINK_URGENCY="+q.Urgency,
		)
		_ = cmd.Run()
	}()
}

// Await blocks until the question is answered, the wait elapses or ctx ends.
func (g *Ground) Await(ctx context.Context, questionID string, wait time.Duration) (proto.AwaitResp, error) {
	g.mu.Lock()
	entry, ok := g.questions[questionID]
	if !ok {
		g.mu.Unlock()
		return proto.AwaitResp{}, fmt.Errorf("unknown question %q", questionID)
	}
	if entry.q.State != proto.QPending {
		resp := proto.AwaitResp{State: entry.q.State, Answer: entry.q.Answer}
		g.mu.Unlock()
		return resp, nil
	}
	done := entry.done
	g.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	case <-done:
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok = g.questions[questionID]
	if !ok {
		return proto.AwaitResp{}, fmt.Errorf("unknown question %q", questionID)
	}
	return proto.AwaitResp{State: entry.q.State, Answer: entry.q.Answer}, nil
}

// Reply answers a pending question and unblocks the waiting agent.
func (g *Ground) Reply(questionID, answer string) (proto.Question, error) {
	if strings.TrimSpace(answer) == "" {
		return proto.Question{}, fmt.Errorf("answer text is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.questions[questionID]
	if !ok {
		return proto.Question{}, fmt.Errorf("unknown question %q", questionID)
	}
	if entry.q.State != proto.QPending {
		return proto.Question{}, fmt.Errorf("question %s is already %s", questionID, entry.q.State)
	}
	now := time.Now().UTC()
	entry.q.State = proto.QAnswered
	entry.q.Answer = answer
	entry.q.AnsweredAt = &now
	_ = g.store.Append(store.KindQuestion, entry.q)
	g.store.Auditf("question %s answered: %s", questionID, firstLine(answer))
	closeOnce(entry.done)
	return entry.q, nil
}

// Abandon marks a question as no longer answerable, because the agent that
// asked it stopped waiting — it timed out, its process died, or its connection
// dropped. Without this the question sits in the inbox looking live, and reply()
// cheerfully reports resuming an agent that nobody is listening for.
func (g *Ground) Abandon(questionID, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.questions[questionID]
	if !ok || entry.q.State != proto.QPending {
		return
	}
	now := time.Now().UTC()
	entry.q.State = proto.QExpired
	entry.q.Answer = reason
	entry.q.AnsweredAt = &now
	_ = g.store.Append(store.KindQuestion, entry.q)
	g.store.Auditf("question %s abandoned: %s", questionID, reason)
	closeOnce(entry.done)
}

// expireQuestionsLocked releases any agent still waiting on a finished job.
func (g *Ground) expireQuestionsLocked(jobID, reason string) {
	for _, id := range g.qOrder {
		entry := g.questions[id]
		if entry.q.JobID != jobID || entry.q.State != proto.QPending {
			continue
		}
		now := time.Now().UTC()
		entry.q.State = proto.QExpired
		entry.q.Answer = reason
		entry.q.AnsweredAt = &now
		_ = g.store.Append(store.KindQuestion, entry.q)
		closeOnce(entry.done)
	}
}

// Inbox lists questions. pendingOnly hides ones already answered.
func (g *Ground) Inbox(pendingOnly bool, limit int) []proto.Question {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []proto.Question{}
	for i := len(g.qOrder) - 1; i >= 0; i-- {
		entry := g.questions[g.qOrder[i]]
		if pendingOnly && entry.q.State != proto.QPending {
			continue
		}
		out = append(out, entry.q)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	// Oldest pending first: the bot waiting longest deserves the first answer.
	sort.SliceStable(out, func(i, j int) bool { return out[i].AskedAt.Before(out[j].AskedAt) })
	return out
}

// --- messages (the uplink) ---

// Send queues an operator note for a running agent.
func (g *Ground) Send(jobID, text string) (proto.Message, error) {
	if strings.TrimSpace(text) == "" {
		return proto.Message{}, fmt.Errorf("message text is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	j, ok := g.jobs[jobID]
	if !ok {
		return proto.Message{}, fmt.Errorf("unknown job %q", jobID)
	}
	if j.Terminal() {
		return proto.Message{}, fmt.Errorf("job %s is already %s", jobID, j.State)
	}
	msg := proto.Message{ID: newID("msg"), JobID: jobID, Text: text, At: time.Now().UTC()}
	g.messages[jobID] = append(g.messages[jobID], msg)
	_ = g.store.Append(store.KindMessage, msg)
	g.store.Auditf("message %s to job %s: %s", msg.ID, jobID, firstLine(text))
	return msg, nil
}

// TakeMessages hands out the messages waiting for a job.
//
// Delivery is at-least-once. Messages handed out are held as in-flight until the
// agent's next call rather than deleted immediately: the response carrying them
// can be lost to a dropped tunnel, and losing an instruction like "do not touch
// prod" is far worse than delivering it twice.
func (g *Ground) TakeMessages(jobID string) []proto.Message {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Reaching this point means the previous batch did get through.
	if prev := g.inFlight[jobID]; len(prev) > 0 {
		now := time.Now().UTC()
		for i := range prev {
			prev[i].DeliveredAt = &now
			_ = g.store.Append(store.KindMessage, prev[i])
		}
		delete(g.inFlight, jobID)
	}

	msgs := g.messages[jobID]
	if len(msgs) == 0 {
		return nil
	}
	delete(g.messages, jobID)
	g.inFlight[jobID] = msgs
	return msgs
}

// PeekMessages reports how many messages are waiting for a job, including any
// handed out but not yet confirmed.
func (g *Ground) PeekMessages(jobID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.messages[jobID]) + len(g.inFlight[jobID])
}
