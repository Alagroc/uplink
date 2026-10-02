package crew

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
	"github.com/Alagroc/uplink/internal/runner"
)

// Config configures a crew worker.
type Config struct {
	Name      string
	Roles     []string
	GroundURL string
	Token     string
	Workdir   string
	// Runners maps a runner name to its launch spec.
	Runners map[string]runner.Spec
	// DefaultRunner is used when a job does not name one. Empty means "the
	// first runner detected on PATH".
	DefaultRunner string
	// UplinkBin is the path the agent uses to spawn its radio.
	UplinkBin string
	// StateDir holds generated MCP configs and raw agent transcripts.
	StateDir string
	// PollWait is how long each long-poll blocks.
	PollWait time.Duration
	// AgentSystemPrompt is appended to the remote agent's system prompt.
	AgentSystemPrompt string
	// MaxConcurrent bounds jobs running at once on this host.
	MaxConcurrent int
	// Clean discards whatever ground still thinks this crew is doing, and
	// removes configs left by a previous process. Applies to the first
	// registration only.
	Clean bool
}

// Crew is a worker process.
type Crew struct {
	cfg    Config
	client *client
	logf   func(string, ...any)

	mu      sync.Mutex
	crewID  string
	running map[string]context.CancelFunc // job id -> cancel
	// cleanPending is cleared after the first successful registration. A
	// reconnect must never ask ground to discard work, because by then this
	// process really does have jobs running.
	cleanPending bool
}

// New builds a Crew.
func New(cfg Config, logf func(string, ...any)) (*Crew, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("crew name is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("token is required (set UPLINK_TOKEN)")
	}
	if cfg.PollWait <= 0 {
		cfg.PollWait = 25 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.Workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cfg.Workdir = wd
	}
	abs, err := filepath.Abs(cfg.Workdir)
	if err != nil {
		return nil, err
	}
	cfg.Workdir = abs
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Crew{
		cfg:          cfg,
		client:       newClient(cfg.GroundURL, cfg.Token),
		logf:         logf,
		running:      map[string]context.CancelFunc{},
		cleanPending: cfg.Clean,
	}, nil
}

// Run registers with ground and serves jobs until ctx is cancelled.
//
// Connection loss is expected: an SSH tunnel drops, a laptop sleeps. The loop
// reconnects with backoff and re-registers when ground has forgotten it.
func (c *Crew) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		if err := c.register(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.logf("register failed: %v (retrying in %s)", err, backoff)
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		backoff = time.Second

		if err := c.pollLoop(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.logf("connection lost: %v (reconnecting in %s)", err, backoff)
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, maxBackoff)
		}
	}
	return nil
}

func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Crew) register(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.client.health(hctx); err != nil {
		return fmt.Errorf("cannot reach ground at %s: %w", c.cfg.GroundURL, err)
	}

	hostname, _ := os.Hostname()
	detected := runner.Detect(c.cfg.Runners)

	c.mu.Lock()
	clean := c.cleanPending
	c.mu.Unlock()

	rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
	defer rcancel()
	resp, err := c.client.register(rctx, proto.RegisterReq{
		Name:     c.cfg.Name,
		Clean:    clean,
		Roles:    c.cfg.Roles,
		OS:       goos(),
		Arch:     goarch(),
		Hostname: hostname,
		Workdir:  c.cfg.Workdir,
		Runners:  detected,
		Version:  proto.Version,
	})
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.crewID = resp.CrewID
	// Only the first registration starts clean. From here on, a reconnect has
	// live jobs to protect.
	c.cleanPending = false
	c.mu.Unlock()

	c.logf("registered with ground as %q (%s); runners: %s", c.cfg.Name, resp.CrewID, describe(detected))

	if clean {
		if len(resp.Discarded) > 0 {
			c.logf("started clean: ground discarded %d leftover job(s): %s",
				len(resp.Discarded), strings.Join(resp.Discarded, ", "))
		} else {
			c.logf("started clean: ground had no leftover jobs for this crew")
		}
		if removed := c.pruneStaleFiles(); removed > 0 {
			c.logf("started clean: removed %d stale generated file(s) from %s", removed, c.cfg.StateDir)
		}
	}
	return nil
}

// pruneStaleFiles removes the per-job files a previous process generated.
//
// These leak whenever a crew is killed before its cleanup runs, and accumulate
// silently. Transcripts are deliberately left alone: they are the record of
// what an agent actually did, which is exactly what you want after a crash.
func (c *Crew) pruneStaleFiles() int {
	dir := filepath.Join(c.cfg.StateDir, "mcp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "mcp-") && !strings.HasPrefix(name, "job-token-") {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed++
		}
	}
	return removed
}

func describe(runners []string) string {
	if len(runners) == 0 {
		return "none found on PATH (exec jobs only)"
	}
	return strings.Join(runners, ", ")
}

func (c *Crew) id() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crewID
}

// pollLoop consumes commands until the connection breaks.
func (c *Crew) pollLoop(ctx context.Context) error {
	for ctx.Err() == nil {
		cmd, err := c.client.poll(ctx, c.id(), c.cfg.PollWait)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if needsReregister(err) {
				c.logf("ground no longer knows this crew; re-registering")
				return nil // Run() re-registers
			}
			return err
		}

		switch cmd.Type {
		case proto.CmdNoop, "":
			// heartbeat
		case proto.CmdJob:
			if cmd.Job == nil {
				continue
			}
			c.startJob(ctx, *cmd.Job)
		case proto.CmdCancel:
			c.cancelJob(cmd.JobID, cmd.Reason)
		default:
			c.logf("ignoring unknown command %q", cmd.Type)
		}
	}
	return nil
}

func (c *Crew) startJob(parent context.Context, job proto.Job) {
	c.mu.Lock()
	if len(c.running) >= c.cfg.MaxConcurrent {
		c.mu.Unlock()
		c.logf("refusing job %s: already running %d jobs", job.ID, c.cfg.MaxConcurrent)
		_ = c.client.setState(parent, proto.JobStateReq{
			CrewID: c.id(), JobID: job.ID, State: proto.StateFailed,
			Error: fmt.Sprintf("crew at capacity (%d concurrent jobs)", c.cfg.MaxConcurrent),
		})
		return
	}
	// Job lifetime is independent of the poll context so that a reconnect does
	// not kill work in progress.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	c.running[job.ID] = cancel
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.running, job.ID)
			c.mu.Unlock()
			cancel()
		}()
		c.runJob(ctx, job)
	}()
}

func (c *Crew) cancelJob(jobID, reason string) {
	c.mu.Lock()
	cancel, ok := c.running[jobID]
	c.mu.Unlock()
	if !ok {
		c.logf("cancel for unknown job %s", jobID)
		return
	}
	c.logf("cancelling job %s: %s", jobID, reason)
	cancel()
}

// Shutdown cancels every running job.
func (c *Crew) Shutdown() {
	c.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.running))
	for _, cancel := range c.running {
		cancels = append(cancels, cancel)
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
