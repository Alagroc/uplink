// Package proto defines the wire types shared by ground (the hub), crew
// (remote workers) and radio (the MCP bridge a remote agent talks through).
package proto

import "time"

// Version is the uplink protocol version. Ground and crew must match exactly:
// the wire types are shared, so a mismatched pair is a bug waiting to happen.
// Upgrade both ends together.
const Version = "1"

// Job kinds.
const (
	KindExec  = "exec"  // run a shell command, stream output back
	KindAgent = "agent" // run a headless AI agent session on the crew host
)

// Job states.
const (
	StateQueued   = "queued"
	StateRunning  = "running"
	StateDone     = "done"
	StateFailed   = "failed"
	StateCanceled = "canceled"
)

// Log streams.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamSystem = "system" // uplink's own narration
	StreamAgent  = "agent"  // condensed agent transcript
)

// Question states.
const (
	QPending  = "pending"
	QAnswered = "answered"
	QExpired  = "expired"
)

// Crew is a worker registered with ground.
type Crew struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Roles    []string  `json:"roles,omitempty"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	Hostname string    `json:"hostname"`
	Workdir  string    `json:"workdir"`
	Runners  []string  `json:"runners,omitempty"` // agent CLIs found on PATH
	Version  string    `json:"version"`           // protocol version
	Build    string    `json:"build,omitempty"`   // crew binary build
	JoinedAt time.Time `json:"joined_at"`
	LastSeen time.Time `json:"last_seen"`
}

// Payload carries the work for a job. Fields are interpreted per Kind.
type Payload struct {
	Command  string            `json:"command,omitempty"` // exec
	Prompt   string            `json:"prompt,omitempty"`  // agent
	Runner   string            `json:"runner,omitempty"`  // agent CLI name
	Workdir  string            `json:"workdir,omitempty"` // relative to crew workdir, or absolute
	Env      map[string]string `json:"env,omitempty"`
	TimeoutS int               `json:"timeout_s,omitempty"`
}

// Job is a unit of work dispatched to one crew.
type Job struct {
	ID       string  `json:"id"`
	CrewID   string  `json:"crew_id"`
	CrewName string  `json:"crew_name"`
	Kind     string  `json:"kind"`
	Label    string  `json:"label,omitempty"`
	Payload  Payload `json:"payload"`

	State    string `json:"state"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Summary  string `json:"summary,omitempty"` // set by the agent via task_complete

	// CancelRequested records that the operator asked for this job to stop. It
	// is kept on the job rather than only queued as a command so the request
	// survives a crew reconnect and is re-delivered.
	CancelRequested bool `json:"cancel_requested,omitempty"`

	// AgentToken authenticates the agent-side MCP tools for this job only. It
	// is sent to the crew with the job and never exposed by operator tools.
	AgentToken string `json:"agent_token,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Terminal reports whether the job has finished.
func (j *Job) Terminal() bool {
	return j.State == StateDone || j.State == StateFailed || j.State == StateCanceled
}

// LogLine is one line of output from a job.
type LogLine struct {
	Seq    int64     `json:"seq"`
	JobID  string    `json:"job_id"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// Question is an agent asking the operator something and waiting on the answer.
type Question struct {
	ID       string   `json:"id"`
	JobID    string   `json:"job_id"`
	CrewName string   `json:"crew_name"`
	Question string   `json:"question"`
	Context  string   `json:"context,omitempty"`
	Options  []string `json:"options,omitempty"`
	Urgency  string   `json:"urgency,omitempty"`

	State      string     `json:"state"`
	Answer     string     `json:"answer,omitempty"`
	AskedAt    time.Time  `json:"asked_at"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
}

// Message is an operator note pushed down to a running agent.
type Message struct {
	ID          string     `json:"id"`
	JobID       string     `json:"job_id"`
	Text        string     `json:"text"`
	At          time.Time  `json:"at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// Command types sent down the crew poll channel.
const (
	CmdNoop   = "noop"
	CmdJob    = "job"
	CmdCancel = "cancel"
)

// Command is one instruction from ground to a crew.
type Command struct {
	Type   string `json:"type"`
	Job    *Job   `json:"job,omitempty"`
	JobID  string `json:"job_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// --- crew <-> ground request/response bodies ---

type RegisterReq struct {
	Name string `json:"name"`
	// Clean asks ground to discard any unfinished job it still holds for this
	// crew name. Only a freshly started crew may set it: a crew reconnecting
	// mid-job genuinely does have work running.
	Clean    bool     `json:"clean,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Hostname string   `json:"hostname"`
	Workdir  string   `json:"workdir"`
	Runners  []string `json:"runners,omitempty"`
	Version  string   `json:"version"`         // protocol version
	Build    string   `json:"build,omitempty"` // crew binary build
}

type RegisterResp struct {
	CrewID        string `json:"crew_id"`
	GroundVersion string `json:"ground_version"`         // protocol version
	GroundBuild   string `json:"ground_build,omitempty"` // ground's binary build
	// Discarded lists jobs abandoned because the crew started clean.
	Discarded []string `json:"discarded,omitempty"`
}

type PollReq struct {
	CrewID string `json:"crew_id"`
	WaitMS int    `json:"wait_ms"`
}

type LogsReq struct {
	CrewID string    `json:"crew_id"`
	JobID  string    `json:"job_id"`
	Lines  []LogLine `json:"lines"`
}

type JobStateReq struct {
	CrewID   string `json:"crew_id"`
	JobID    string `json:"job_id"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// --- agent (radio) request/response bodies ---

type AskReq struct {
	Question string   `json:"question"`
	Context  string   `json:"context,omitempty"`
	Options  []string `json:"options,omitempty"`
	Urgency  string   `json:"urgency,omitempty"`
	TimeoutS int      `json:"timeout_s,omitempty"`
}

type AwaitResp struct {
	State  string `json:"state"`
	Answer string `json:"answer,omitempty"`
}
