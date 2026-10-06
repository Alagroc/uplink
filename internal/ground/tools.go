package ground

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Alagroc/uplink/internal/mcp"
	"github.com/Alagroc/uplink/internal/proto"
)

// OperatorInstructions is sent to the operator's CLI on initialize. It is the
// only briefing the local model gets, so it states the polling contract
// plainly: ground cannot interrupt a session, so the model must look.
const OperatorInstructions = `uplink mission control. Your crew are AI agents and shells on remote machines.

Dispatch work with submit_job (kind="agent" for an autonomous agent session, kind="exec" for a plain
command). Jobs are asynchronous: submit_job returns immediately with a job_id.

Then call await_job rather than polling. It blocks until the job finishes or its agent needs you, and
returns the output produced while you waited. Repeated job_status/job_logs calls re-read a transcript
that only grows; await_job costs one call and sleeps. When you do read logs directly, pass the
since_seq the previous call reported so you get only what is new.

A remote agent that needs a decision calls ask_operator and BLOCKS until you answer. Those questions
appear in inbox() and are cleared with reply(). Nothing can interrupt you to announce a question, so
check inbox() whenever you are waiting on remote work, and before you conclude that a job is stuck.
A blocked agent keeps its full context, so answering resumes the same train of thought.

Read job_logs() before answering a question: the agent's own view of what it tried is usually the
context you need. Use send_message() to redirect a running agent without being asked.`

// AgentInstructions briefs the remote agent about its channel home.
const AgentInstructions = `You are running as uplink crew on a remote machine, driven by a human operator on another host.

Use ask_operator when a decision is the operator's to make: ambiguous requirements, a destructive or
irreversible step, missing credentials, or a fork in approach. It blocks until they answer, which is
fine and expected. Always attach what you already tried in the context argument; a bare question
wastes a round trip.

Use report_progress at milestones so the operator can follow along without reading raw logs. Call
check_messages periodically during long stretches of work: the operator can send instructions that
are waiting for you. Call task_complete with a summary when you finish.`

// RegisterOperatorTools adds the operator-facing tools to an MCP server.
func (g *Ground) RegisterOperatorTools(s *mcp.Server) {
	s.Add(mcp.Tool{
		Name:        "list_crew",
		Description: "List remote crew registered with ground: name, roles, OS/arch, binary build, available agent runners, online status and active jobs. Flags any crew running a different build from ground, which is otherwise invisible.",
		Schema:      mcp.Schema{Props: map[string]mcp.Prop{}},
		Handler:     g.toolListCrew,
	})

	s.Add(mcp.Tool{
		Name:        "submit_job",
		Description: "Dispatch work to a crew and return a job_id immediately. kind=\"agent\" starts an autonomous agent session on the remote host with the given prompt; kind=\"exec\" runs a single shell command. Target a crew by name, or by role to let ground pick the least busy one.",
		Schema: mcp.Schema{
			Props: map[string]mcp.Prop{
				"kind":      {Type: "string", Description: "agent or exec", Enum: []string{proto.KindAgent, proto.KindExec}},
				"crew":      {Type: "string", Description: "Exact crew name (see list_crew)"},
				"role":      {Type: "string", Description: "Instead of crew: any online crew with this role"},
				"prompt":    {Type: "string", Description: "For kind=agent: the task, written as you would brief a colleague"},
				"command":   {Type: "string", Description: "For kind=exec: the shell command"},
				"runner":    {Type: "string", Description: "Agent CLI to use (claude, codex, cursor-agent). Defaults to the crew's first available."},
				"workdir":   {Type: "string", Description: "Directory to run in, absolute or relative to the crew workdir"},
				"timeout_s": {Type: "integer", Description: "Kill the job after this many seconds (0 = no limit)"},
				"label":     {Type: "string", Description: "Short human label for the job"},
			},
			Required: []string{"kind"},
		},
		Handler: g.toolSubmitJob,
	})

	s.Add(mcp.Tool{
		Name:        "await_job",
		Description: "Wait for a job instead of polling it. Blocks until the job finishes or its agent asks you a question, whichever comes first, and returns the final status plus whatever output appeared while you waited. Use this rather than repeated job_status/job_logs calls: a twenty-minute job should cost one call that sleeps, not forty that each re-read the same transcript.",
		Schema: mcp.Schema{
			Props: map[string]mcp.Prop{
				"job_id":    {Type: "string"},
				"timeout_s": {Type: "integer", Description: "Give up waiting after this long and report progress so far (default 300, max 3600)", Default: 300},
				"since_seq": {Type: "integer", Description: "Return only output at or after this sequence number. Pass the next_seq from your previous call.", Default: 0},
				"log_limit": {Type: "integer", Description: "Max output lines to return (default 100)", Default: 100},
			},
			Required: []string{"job_id"},
		},
		Handler: g.toolAwaitJob,
	})

	s.Add(mcp.Tool{
		Name:        "job_status",
		Description: "Status of one job, or of all active jobs when job_id is omitted.",
		Schema: mcp.Schema{Props: map[string]mcp.Prop{
			"job_id": {Type: "string", Description: "Omit to list active jobs"},
			"crew":   {Type: "string", Description: "Filter by crew name"},
			"all":    {Type: "boolean", Description: "Include finished jobs", Default: false},
			"limit":  {Type: "integer", Description: "Max jobs to list", Default: 20},
		}},
		Handler: g.toolJobStatus,
	})

	s.Add(mcp.Tool{
		Name:        "job_logs",
		Description: "Read a job's output. Pass since_seq from a previous call to tail only what is new. Recent output is buffered in memory; older finished jobs keep theirs only in ground's logs.jsonl.",
		Schema: mcp.Schema{
			Props: map[string]mcp.Prop{
				"job_id":    {Type: "string"},
				"since_seq": {Type: "integer", Description: "Return lines with seq >= this", Default: 0},
				"limit":     {Type: "integer", Description: "Max lines (default 200)", Default: 200},
				"stream":    {Type: "string", Description: "Only this stream", Enum: []string{proto.StreamStdout, proto.StreamStderr, proto.StreamSystem, proto.StreamAgent}},
			},
			Required: []string{"job_id"},
		},
		Handler: g.toolJobLogs,
	})

	s.Add(mcp.Tool{
		Name:        "cancel_job",
		Description: "Stop a running job. The crew kills the process tree; any agent blocked on ask_operator is released.",
		Schema: mcp.Schema{
			Props:    map[string]mcp.Prop{"job_id": {Type: "string"}, "reason": {Type: "string"}},
			Required: []string{"job_id"},
		},
		Handler: g.toolCancelJob,
	})

	s.Add(mcp.Tool{
		Name:        "inbox",
		Description: "Questions from remote agents that are blocked waiting on you, oldest first. Answer with reply().",
		Schema: mcp.Schema{Props: map[string]mcp.Prop{
			"include_answered": {Type: "boolean", Description: "Also show recently answered questions", Default: false},
			"limit":            {Type: "integer", Default: 20},
		}},
		Handler: g.toolInbox,
	})

	s.Add(mcp.Tool{
		Name:        "reply",
		Description: "Answer a question from inbox(). The remote agent resumes immediately with its context intact, so answer as you would speak to a colleague — reasons help it generalise.",
		Schema: mcp.Schema{
			Props:    map[string]mcp.Prop{"question_id": {Type: "string"}, "answer": {Type: "string"}},
			Required: []string{"question_id", "answer"},
		},
		Handler: g.toolReply,
	})

	s.Add(mcp.Tool{
		Name:        "send_message",
		Description: "Send an unprompted instruction to a running agent (a correction, a new constraint). It is delivered the next time the agent calls any uplink tool, so delivery is prompt but not instant.",
		Schema: mcp.Schema{
			Props:    map[string]mcp.Prop{"job_id": {Type: "string"}, "text": {Type: "string"}},
			Required: []string{"job_id", "text"},
		},
		Handler: g.toolSendMessage,
	})
}

// --- operator handlers ---

func (g *Ground) toolListCrew(_ context.Context, _ json.RawMessage) (string, error) {
	crew := g.ListCrew()
	if len(crew) == 0 {
		return "No crew registered. Start one with: uplink crew --name <name> --ground <url>", nil
	}
	var b strings.Builder
	for _, c := range crew {
		status := "ONLINE"
		if !c.Online {
			status = "offline (last seen " + c.LastSeenAgo + " ago)"
		}
		fmt.Fprintf(&b, "%s  [%s]\n", c.Name, status)
		fmt.Fprintf(&b, "  host: %s  %s/%s\n", c.Hostname, c.OS, c.Arch)
		if c.BuildSkew != "" {
			fmt.Fprintf(&b, "  build: %s  ← %s; rebuild and restart it with --clean\n", orUnset(c.Build), c.BuildSkew)
		} else if c.Build != "" {
			fmt.Fprintf(&b, "  build: %s\n", c.Build)
		}
		if len(c.Roles) > 0 {
			fmt.Fprintf(&b, "  roles: %s\n", strings.Join(c.Roles, ", "))
		}
		if len(c.Runners) > 0 {
			fmt.Fprintf(&b, "  runners: %s\n", strings.Join(c.Runners, ", "))
		} else {
			fmt.Fprintf(&b, "  runners: none found (exec jobs only)\n")
		}
		fmt.Fprintf(&b, "  workdir: %s\n", c.Workdir)
		if len(c.ActiveJobs) > 0 {
			fmt.Fprintf(&b, "  active jobs: %s\n", strings.Join(c.ActiveJobs, ", "))
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type submitArgs struct {
	Kind     string `json:"kind"`
	Crew     string `json:"crew"`
	Role     string `json:"role"`
	Prompt   string `json:"prompt"`
	Command  string `json:"command"`
	Runner   string `json:"runner"`
	Workdir  string `json:"workdir"`
	TimeoutS int    `json:"timeout_s"`
	Label    string `json:"label"`
}

func (g *Ground) toolSubmitJob(_ context.Context, raw json.RawMessage) (string, error) {
	var a submitArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	job, err := g.Submit(SubmitReq{
		Crew:  a.Crew,
		Role:  a.Role,
		Kind:  a.Kind,
		Label: a.Label,
		Payload: proto.Payload{
			Command: a.Command, Prompt: a.Prompt, Runner: a.Runner,
			Workdir: a.Workdir, TimeoutS: a.TimeoutS,
		},
	})
	if err != nil {
		return "", err
	}
	hint := "Poll job_status/job_logs for progress."
	if job.Kind == proto.KindAgent {
		hint = "The agent may call ask_operator; check inbox() while you wait."
	}
	return fmt.Sprintf("job %s queued: kind=%s crew=%s\n%s", job.ID, job.Kind, job.CrewName, hint), nil
}

// maxAwait bounds a single wait. Longer than this and the caller should come
// back rather than hold a connection open indefinitely.
const maxAwait = time.Hour

func (g *Ground) toolAwaitJob(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		JobID    string `json:"job_id"`
		TimeoutS int    `json:"timeout_s"`
		SinceSeq int64  `json:"since_seq"`
		LogLimit int    `json:"log_limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}

	wait := 300 * time.Second
	if a.TimeoutS > 0 {
		wait = time.Duration(a.TimeoutS) * time.Second
	}
	if wait > maxAwait {
		wait = maxAwait
	}
	if a.LogLimit <= 0 {
		a.LogLimit = 100
	}

	started := time.Now()
	event, err := g.AwaitJob(ctx, a.JobID, wait)
	if err != nil {
		return "", err
	}

	job, ok := g.Job(a.JobID)
	if !ok {
		return "", fmt.Errorf("unknown job %q", a.JobID)
	}
	lines, next, err := g.Logs(a.JobID, a.SinceSeq, a.LogLimit, "")
	if err != nil {
		return "", err
	}

	var b strings.Builder
	switch event {
	case JobEventFinished:
		fmt.Fprintf(&b, "Job finished after waiting %s.\n\n", time.Since(started).Round(time.Second))
	case JobEventQuestion:
		fmt.Fprintf(&b, "The agent is waiting on you after %s. Read inbox() and answer with reply().\n\n", time.Since(started).Round(time.Second))
	default:
		fmt.Fprintf(&b, "Still running after %s. Progress so far:\n\n", time.Since(started).Round(time.Second))
	}

	b.WriteString(formatJob(job, true, g.PeekMessages(job.ID), 0))
	b.WriteString("\n\n")

	if len(lines) == 0 {
		fmt.Fprintf(&b, "No new output.\n")
	} else {
		fmt.Fprintf(&b, "Output since seq %d:\n", a.SinceSeq)
		for _, ln := range lines {
			prefix := ""
			if ln.Stream != proto.StreamStdout {
				prefix = "[" + ln.Stream + "] "
			}
			fmt.Fprintf(&b, "%s%s\n", prefix, ln.Text)
		}
	}

	switch event {
	case JobEventFinished:
		fmt.Fprintf(&b, "\n--- next_seq=%d (job is %s; nothing more will arrive)", next, job.State)
	case JobEventQuestion:
		fmt.Fprintf(&b, "\n--- next_seq=%d — answer the question, then call await_job again with since_seq=%d", next, next)
	default:
		fmt.Fprintf(&b, "\n--- next_seq=%d — call await_job again with since_seq=%d to keep waiting", next, next)
	}
	return b.String(), nil
}

func (g *Ground) toolJobStatus(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		JobID string `json:"job_id"`
		Crew  string `json:"crew"`
		All   bool   `json:"all"`
		Limit int    `json:"limit"`
	}
	_ = json.Unmarshal(raw, &a)

	if a.JobID != "" {
		job, ok := g.Job(a.JobID)
		if !ok {
			return "", fmt.Errorf("unknown job %q", a.JobID)
		}
		return formatJob(job, true, g.PeekMessages(job.ID), g.LogPosition(job.ID)), nil
	}

	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	jobs := g.Jobs(a.Crew, !a.All, limit)
	if len(jobs) == 0 {
		if a.All {
			return "No jobs yet.", nil
		}
		return "No active jobs. Pass all=true to see finished ones.", nil
	}
	var b strings.Builder
	for _, j := range jobs {
		b.WriteString(formatJob(j, false, 0, 0))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func formatJob(j proto.Job, verbose bool, pendingMsgs int, logNext int64) string {
	var b strings.Builder
	label := j.Label
	if label == "" {
		label = firstLine(j.Payload.Command + j.Payload.Prompt)
	}
	fmt.Fprintf(&b, "%s  %-8s %-8s crew=%s  %s", j.ID, j.State, j.Kind, j.CrewName, label)
	if j.ExitCode != nil {
		fmt.Fprintf(&b, "  exit=%d", *j.ExitCode)
	}
	if !verbose {
		return b.String()
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  created: %s\n", j.CreatedAt.Format(time.RFC3339))
	if j.StartedAt != nil {
		end := time.Now().UTC()
		if j.EndedAt != nil {
			end = *j.EndedAt
		}
		fmt.Fprintf(&b, "  runtime: %s\n", end.Sub(*j.StartedAt).Round(time.Second))
	}
	if j.Payload.Workdir != "" {
		fmt.Fprintf(&b, "  workdir: %s\n", j.Payload.Workdir)
	}
	if j.Payload.Runner != "" {
		fmt.Fprintf(&b, "  runner: %s\n", j.Payload.Runner)
	}
	if j.Error != "" {
		fmt.Fprintf(&b, "  error: %s\n", j.Error)
	}
	if j.Summary != "" {
		fmt.Fprintf(&b, "  agent summary: %s\n", j.Summary)
	}
	if pendingMsgs > 0 {
		fmt.Fprintf(&b, "  %d operator message(s) not yet picked up by the agent\n", pendingMsgs)
	}
	if logNext > 0 {
		fmt.Fprintf(&b, "  output: next_seq=%d\n", logNext)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (g *Ground) toolJobLogs(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		JobID    string `json:"job_id"`
		SinceSeq int64  `json:"since_seq"`
		Limit    int    `json:"limit"`
		Stream   string `json:"stream"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	lines, next, err := g.Logs(a.JobID, a.SinceSeq, a.Limit, a.Stream)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		if next > 0 {
			return fmt.Sprintf("no buffered output for this job (next_seq=%d). "+
				"Output from before a ground restart, or from an old finished job, is not kept in memory — "+
				"it is on disk in the ground state directory as logs.jsonl", next), nil
		}
		return fmt.Sprintf("no output yet (next_seq=%d)", next), nil
	}
	var b strings.Builder
	for _, ln := range lines {
		prefix := ""
		if ln.Stream != proto.StreamStdout {
			prefix = "[" + ln.Stream + "] "
		}
		fmt.Fprintf(&b, "%s%s\n", prefix, ln.Text)
	}
	// Spell out what the cursor is for: a bare number invites re-reading the
	// whole transcript on the next call.
	job, _ := g.Job(a.JobID)
	if job.Terminal() {
		fmt.Fprintf(&b, "--- next_seq=%d (job is %s; nothing more will arrive)", next, job.State)
	} else {
		fmt.Fprintf(&b, "--- next_seq=%d — pass since_seq=%d for only what is new, or await_job to wait for it", next, next)
	}
	return b.String(), nil
}

func (g *Ground) toolCancelJob(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		JobID  string `json:"job_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	reason := a.Reason
	if reason == "" {
		reason = "cancelled by operator"
	}
	if err := g.Cancel(a.JobID, reason); err != nil {
		return "", err
	}
	return fmt.Sprintf("cancel sent for %s", a.JobID), nil
}

func (g *Ground) toolInbox(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		IncludeAnswered bool `json:"include_answered"`
		Limit           int  `json:"limit"`
	}
	_ = json.Unmarshal(raw, &a)
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	qs := g.Inbox(!a.IncludeAnswered, limit)
	if len(qs) == 0 {
		return "Inbox empty: no agent is waiting on you.", nil
	}
	var b strings.Builder
	now := time.Now().UTC()
	for _, q := range qs {
		state := q.State
		if q.State == proto.QPending {
			state = fmt.Sprintf("WAITING %s", now.Sub(q.AskedAt).Round(time.Second))
		}
		fmt.Fprintf(&b, "%s  [%s]  crew=%s job=%s urgency=%s\n", q.ID, state, q.CrewName, q.JobID, q.Urgency)
		fmt.Fprintf(&b, "  Q: %s\n", q.Question)
		if q.Context != "" {
			fmt.Fprintf(&b, "  context: %s\n", indent(q.Context, "    "))
		}
		if len(q.Options) > 0 {
			fmt.Fprintf(&b, "  options: %s\n", strings.Join(q.Options, " | "))
		}
		if q.Answer != "" {
			fmt.Fprintf(&b, "  A: %s\n", q.Answer)
		}
		b.WriteString("\n")
	}
	b.WriteString("Answer with reply(question_id, answer).")
	return b.String(), nil
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

func (g *Ground) toolReply(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		QuestionID string `json:"question_id"`
		Answer     string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	q, err := g.Reply(a.QuestionID, a.Answer)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("answered %s; crew %q resumed job %s", q.ID, q.CrewName, q.JobID), nil
}

func (g *Ground) toolSendMessage(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		JobID string `json:"job_id"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	msg, err := g.Send(a.JobID, a.Text)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("message %s queued for job %s; the agent picks it up on its next uplink tool call", msg.ID, a.JobID), nil
}

// --- agent-side tools ---

type jobCtxKey struct{}

// WithJob attaches the authenticated job to a context.
func WithJob(ctx context.Context, job proto.Job) context.Context {
	return context.WithValue(ctx, jobCtxKey{}, job)
}

func jobFrom(ctx context.Context) (proto.Job, error) {
	job, ok := ctx.Value(jobCtxKey{}).(proto.Job)
	if !ok {
		return proto.Job{}, fmt.Errorf("no job on this connection: the agent MCP endpoint needs a per-job token")
	}
	return job, nil
}

// defaultAskTimeout bounds how long an agent waits on a human before it has to
// decide for itself. Long enough to cover a lunch break.
const defaultAskTimeout = 2 * time.Hour

// RegisterAgentTools adds the tools a remote agent uses to reach the operator.
func (g *Ground) RegisterAgentTools(s *mcp.Server) {
	s.Add(mcp.Tool{
		Name:        "ask_operator",
		Description: "Ask the human operator a question and wait for their answer. This call blocks — that is intended, and your context is preserved while you wait. Use it for decisions that are theirs: ambiguous requirements, destructive or irreversible steps, missing credentials, or a genuine fork in approach. Always fill in context with what you already tried.",
		Schema: mcp.Schema{
			Props: map[string]mcp.Prop{
				"question":  {Type: "string", Description: "The decision you need, stated so it can be answered in a sentence"},
				"context":   {Type: "string", Description: "What you tried, what you found, the relevant file or error"},
				"options":   {Type: "array", Items: "string", Description: "Concrete choices, if the decision is a pick"},
				"urgency":   {Type: "string", Enum: []string{"low", "normal", "high"}, Description: "high if you are fully blocked"},
				"timeout_s": {Type: "integer", Description: "Give up waiting after this long (default 7200)"},
			},
			Required: []string{"question"},
		},
		Handler: g.toolAskOperator,
	})

	s.Add(mcp.Tool{
		Name:        "report_progress",
		Description: "Tell the operator what you have done or are about to do. Does not block. Use it at milestones so they can follow along without reading raw logs.",
		Schema: mcp.Schema{
			Props:    map[string]mcp.Prop{"text": {Type: "string"}, "phase": {Type: "string", Description: "Optional short phase name"}},
			Required: []string{"text"},
		},
		Handler: g.toolReportProgress,
	})

	s.Add(mcp.Tool{
		Name:        "check_messages",
		Description: "Collect instructions the operator sent you without being asked. Call this periodically during long work; a message may change your task.",
		Schema:      mcp.Schema{Props: map[string]mcp.Prop{}},
		Handler:     g.toolCheckMessages,
	})

	s.Add(mcp.Tool{
		Name:        "task_complete",
		Description: "Report that the task is finished, with a summary the operator will read. Call this before you stop.",
		Schema: mcp.Schema{
			Props:    map[string]mcp.Prop{"summary": {Type: "string", Description: "What you changed, what you verified, what is left"}},
			Required: []string{"summary"},
		},
		Handler: g.toolTaskComplete,
	})
}

func (g *Ground) toolAskOperator(ctx context.Context, raw json.RawMessage) (string, error) {
	job, err := jobFrom(ctx)
	if err != nil {
		return "", err
	}
	var a proto.AskReq
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	q, err := g.Ask(job, a)
	if err != nil {
		return "", err
	}

	timeout := defaultAskTimeout
	if a.TimeoutS > 0 {
		timeout = time.Duration(a.TimeoutS) * time.Second
	}
	deadline := time.Now().Add(timeout)

	// Wait in slices so a dropped HTTP connection surfaces promptly rather than
	// hanging for hours on a socket nobody is reading.
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		slice := min(remaining, 30*time.Second)
		resp, err := g.Await(ctx, q.ID, slice)
		if err != nil {
			return "", err
		}
		switch resp.State {
		case proto.QAnswered:
			out := "Operator answered: " + resp.Answer
			if extra := g.formatMessages(job.ID); extra != "" {
				out += "\n\n" + extra
			}
			return out, nil
		case proto.QExpired:
			return "", fmt.Errorf("question released without an answer: %s", resp.Answer)
		}
		if err := ctx.Err(); err != nil {
			// The agent's connection is gone: stop showing the operator a
			// question that nobody is waiting on.
			g.Abandon(q.ID, "released: the agent stopped waiting")
			return "", err
		}
	}
	g.Abandon(q.ID, fmt.Sprintf("released: no answer within %s", timeout))
	return "", fmt.Errorf("the operator did not answer within %s, so this question was released. "+
		"Proceed with the safest reversible option and say clearly in report_progress what you assumed, "+
		"or stop and call task_complete explaining what is blocked", timeout)
}

func (g *Ground) toolReportProgress(ctx context.Context, raw json.RawMessage) (string, error) {
	job, err := jobFrom(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Text  string `json:"text"`
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if strings.TrimSpace(a.Text) == "" {
		return "", fmt.Errorf("text is required")
	}
	text := a.Text
	if a.Phase != "" {
		text = "[" + a.Phase + "] " + text
	}
	if _, err := g.AppendLogs("", job.ID, []proto.LogLine{{Stream: proto.StreamAgent, Text: "progress: " + text}}); err != nil {
		return "", err
	}
	out := "noted"
	if extra := g.formatMessages(job.ID); extra != "" {
		out += "\n\n" + extra
	}
	return out, nil
}

func (g *Ground) toolCheckMessages(ctx context.Context, _ json.RawMessage) (string, error) {
	job, err := jobFrom(ctx)
	if err != nil {
		return "", err
	}
	if extra := g.formatMessages(job.ID); extra != "" {
		return extra, nil
	}
	return "No messages from the operator. Carry on.", nil
}

func (g *Ground) toolTaskComplete(ctx context.Context, raw json.RawMessage) (string, error) {
	job, err := jobFrom(ctx)
	if err != nil {
		return "", err
	}
	var a struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if strings.TrimSpace(a.Summary) == "" {
		return "", fmt.Errorf("summary is required")
	}
	if err := g.Complete(job.ID, a.Summary); err != nil {
		return "", err
	}
	_, _ = g.AppendLogs("", job.ID, []proto.LogLine{{Stream: proto.StreamAgent, Text: "complete: " + a.Summary}})
	return "Recorded. The operator can read your summary in job_status.", nil
}

// formatMessages drains queued operator messages into text for an agent.
func (g *Ground) formatMessages(jobID string) string {
	msgs := g.TakeMessages(jobID)
	if len(msgs) == 0 {
		return ""
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].At.Before(msgs[j].At) })
	var b strings.Builder
	b.WriteString("--- Message(s) from the operator, treat as instructions ---\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "- %s\n", m.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}
