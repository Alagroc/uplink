package crew

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Alagroc/uplink/internal/proto"
	"github.com/Alagroc/uplink/internal/runner"
)

// runJob executes one job and reports its outcome.
func (c *Crew) runJob(ctx context.Context, job proto.Job) {
	shipper := newShipper(c, job.ID)
	defer shipper.close()

	if err := c.client.setState(ctx, proto.JobStateReq{CrewID: c.id(), JobID: job.ID, State: proto.StateRunning}); err != nil {
		c.logf("job %s: cannot report running state: %v", job.ID, err)
	}

	workdir, err := c.resolveWorkdir(job.Payload.Workdir)
	if err != nil {
		c.finish(ctx, job, proto.StateFailed, nil, err.Error())
		return
	}
	shipper.system(fmt.Sprintf("crew %s starting %s job in %s", c.cfg.Name, job.Kind, workdir))

	if job.Payload.TimeoutS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(job.Payload.TimeoutS)*time.Second)
		defer cancel()
	}

	var built *runner.Built
	switch job.Kind {
	case proto.KindExec:
		built = c.buildExec(job, workdir)
	case proto.KindAgent:
		built, err = c.buildAgent(job, workdir)
		if err != nil {
			shipper.system("cannot start agent: " + err.Error())
			c.finish(ctx, job, proto.StateFailed, nil, err.Error())
			return
		}
	default:
		c.finish(ctx, job, proto.StateFailed, nil, "unknown job kind "+job.Kind)
		return
	}
	defer built.Cleanup()

	exit, runErr := c.execute(ctx, built, job, shipper)

	switch {
	case ctx.Err() != nil && exit != 0:
		// Cancelled or timed out: distinguish the two for the operator.
		reason := "cancelled by operator"
		if ctx.Err() == context.DeadlineExceeded {
			reason = fmt.Sprintf("timed out after %ds", job.Payload.TimeoutS)
		}
		shipper.system(reason)
		shipper.flush()
		c.finish(context.WithoutCancel(ctx), job, proto.StateCanceled, &exit, reason)
	case runErr != nil:
		shipper.system("failed: " + runErr.Error())
		shipper.flush()
		c.finish(ctx, job, proto.StateFailed, &exit, runErr.Error())
	case exit != 0:
		shipper.system(fmt.Sprintf("exited with code %d", exit))
		shipper.flush()
		c.finish(ctx, job, proto.StateFailed, &exit, fmt.Sprintf("exit code %d", exit))
	default:
		shipper.system("completed successfully")
		shipper.flush()
		c.finish(ctx, job, proto.StateDone, &exit, "")
	}
}

// resolveWorkdir maps a job's workdir onto the crew's sandbox. A relative path
// is joined to the crew workdir; an absolute path must stay inside it, so a
// prompt cannot walk the agent out of the tree the operator pointed it at.
func (c *Crew) resolveWorkdir(dir string) (string, error) {
	if dir == "" {
		return c.cfg.Workdir, nil
	}
	candidate := dir
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(c.cfg.Workdir, candidate)
	}
	candidate = filepath.Clean(candidate)

	root, err := filepath.EvalSymlinks(c.cfg.Workdir)
	if err != nil {
		root = c.cfg.Workdir
	}
	resolved := candidate
	if r, err := filepath.EvalSymlinks(candidate); err == nil {
		resolved = r
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workdir %q is outside the crew workdir %q", dir, c.cfg.Workdir)
	}
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("workdir %q: %w", dir, err)
	}
	return candidate, nil
}

func (c *Crew) buildExec(job proto.Job, workdir string) *runner.Built {
	cmd := exec.Command("sh", "-c", job.Payload.Command)
	cmd.Dir = workdir
	// Strip the operator token: an exec job is a shell the agent can also reach.
	cmd.Env = runner.ChildEnv("UPLINK_JOB_ID="+job.ID, "UPLINK_CREW="+c.cfg.Name)
	for k, v := range job.Payload.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return &runner.Built{Cmd: cmd, Cleanup: func() {}}
}

func (c *Crew) buildAgent(job proto.Job, workdir string) (*runner.Built, error) {
	name := job.Payload.Runner
	if name == "" {
		name = c.cfg.DefaultRunner
	}
	if name == "" {
		detected := runner.Detect(c.cfg.Runners)
		if len(detected) == 0 {
			return nil, fmt.Errorf("no agent CLI found on PATH on crew %q (looked for %s)", c.cfg.Name, strings.Join(knownNames(c.cfg.Runners), ", "))
		}
		name = detected[0]
	}
	spec, ok := c.cfg.Runners[name]
	if !ok {
		return nil, fmt.Errorf("unknown runner %q; configured: %s", name, strings.Join(knownNames(c.cfg.Runners), ", "))
	}
	if job.AgentToken == "" {
		return nil, fmt.Errorf("ground did not issue an agent token for job %s", job.ID)
	}

	built, err := runner.Build(spec, runner.Options{
		Prompt:    job.Payload.Prompt,
		Workdir:   workdir,
		UplinkBin: c.cfg.UplinkBin,
		GroundURL: c.cfg.GroundURL,
		JobToken:  job.AgentToken,
		System:    c.cfg.AgentSystemPrompt,
		ConfigDir: filepath.Join(c.cfg.StateDir, "mcp"),
	})
	if err != nil {
		return nil, err
	}
	for k, v := range job.Payload.Env {
		built.Cmd.Env = append(built.Cmd.Env, k+"="+v)
	}
	built.Cmd.Env = append(built.Cmd.Env, "UPLINK_JOB_ID="+job.ID, "UPLINK_CREW="+c.cfg.Name)
	return built, nil
}

func knownNames(specs map[string]runner.Spec) []string {
	out := make([]string, 0, len(specs))
	for n := range specs {
		out = append(out, n)
	}
	return out
}

// execute runs the command, streaming both pipes, and returns its exit code.
func (c *Crew) execute(ctx context.Context, built *runner.Built, job proto.Job, sh *shipper) (int, error) {
	cmd := built.Cmd
	setProcessGroup(cmd) // so cancel kills children, not just the shell

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	var transcript *os.File
	if built.StreamJSON {
		// Keep the raw agent transcript on the crew host: it is far too verbose
		// to ship, but invaluable when debugging what an agent actually did.
		path := filepath.Join(c.cfg.StateDir, "transcripts")
		if err := os.MkdirAll(path, 0o700); err == nil {
			if f, err := os.OpenFile(filepath.Join(path, job.ID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				transcript = f
				defer transcript.Close()
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if built.StreamJSON {
			scanAgentStream(stdout, sh, transcript)
			return
		}
		scanLines(stdout, proto.StreamStdout, sh)
	}()
	go func() {
		defer wg.Done()
		scanLines(stderr, proto.StreamStderr, sh)
	}()

	// Kill the process group when the job context ends.
	done := make(chan struct{})
	var killTimer *time.Timer
	go func() {
		select {
		case <-ctx.Done():
			killTimer = killProcessGroup(cmd)
		case <-done:
		}
	}()

	// Wait for both pipes to drain, but not indefinitely. A grandchild that
	// escaped the process group (setsid, nohup, a container runtime, a dev
	// server the agent started) still holds the write end, so EOF may never
	// arrive — and then this job would sit in "running" forever.
	scanned := make(chan struct{})
	go func() {
		wg.Wait()
		close(scanned)
	}()
	select {
	case <-scanned:
	case <-ctx.Done():
		select {
		case <-scanned:
		case <-time.After(gracePeriod + time.Second):
			sh.system("output truncated: the process tree did not close its pipes after being killed")
		}
	}

	waitErr := cmd.Wait()
	close(done)
	if killTimer != nil {
		// The process is reaped; stop the pending SIGKILL so it cannot land on
		// a process group the kernel has since reused.
		killTimer.Stop()
	}

	exit := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitCode()
			// A signalled process reports -1; make that legible.
			if exit < 0 {
				exit = 137
			}
			return exit, nil
		}
		return -1, waitErr
	}
	return exit, nil
}

func (c *Crew) finish(ctx context.Context, job proto.Job, state string, exit *int, errMsg string) {
	req := proto.JobStateReq{CrewID: c.id(), JobID: job.ID, State: state, ExitCode: exit, Error: errMsg}
	// Use a fresh deadline: the job context may already be cancelled, and
	// ground still needs the final state.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := c.client.setState(rctx, req); err != nil {
		c.logf("job %s: cannot report final state %s: %v", job.ID, state, err)
		return
	}
	c.logf("job %s finished: %s", job.ID, state)
}

// --- log shipping ---

// maxLineLen truncates pathological single lines (minified bundles, base64).
const maxLineLen = 8000

// shipper batches log lines and flushes them to ground.
type shipper struct {
	crew  *Crew
	jobID string

	mu      sync.Mutex
	pending []proto.LogLine
	// flushMu serialises transmission so ground assigns sequence numbers in the
	// order the lines were produced.
	flushMu sync.Mutex
	stop    chan struct{}
	done    chan struct{}
}

func newShipper(c *Crew, jobID string) *shipper {
	s := &shipper{crew: c, jobID: jobID, stop: make(chan struct{}), done: make(chan struct{})}
	go s.loop()
	return s
}

func (s *shipper) loop() {
	defer close(s.done)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			s.flush()
			return
		case <-ticker.C:
			s.flush()
		}
	}
}

func (s *shipper) add(stream, text string) {
	if len(text) > maxLineLen {
		cut := maxLineLen
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + " …[truncated]"
	}
	s.mu.Lock()
	s.pending = append(s.pending, proto.LogLine{Stream: stream, Text: text, At: time.Now().UTC()})
	full := len(s.pending) >= 100
	s.mu.Unlock()
	if full {
		s.flush()
	}
}

func (s *shipper) system(text string) { s.add(proto.StreamSystem, text) }

func (s *shipper) flush() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.crew.client.sendLogs(ctx, s.crew.id(), s.jobID, batch); err != nil {
		s.crew.logf("job %s: log flush failed (%d lines lost): %v", s.jobID, len(batch), err)
	}
}

func (s *shipper) close() {
	close(s.stop)
	<-s.done
}

func scanLines(r io.Reader, stream string, sh *shipper) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		sh.add(stream, sc.Text())
	}
	// A line longer than the buffer stops the scanner. Say so: output that just
	// stops with no explanation sends the operator hunting for the wrong bug.
	if err := sc.Err(); err != nil {
		sh.system(fmt.Sprintf("%s truncated: %v", stream, err))
	}
}

// scanAgentStream condenses an agent's JSON event stream into readable lines,
// writing the raw events to transcript when one is open.
func scanAgentStream(r io.Reader, sh *shipper, transcript io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if transcript != nil {
			_, _ = transcript.Write(append(append([]byte{}, line...), '\n'))
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "{") {
			sh.add(proto.StreamAgent, trimmed)
			continue
		}
		for _, summary := range summarizeAgentEvent([]byte(trimmed)) {
			sh.add(proto.StreamAgent, summary)
		}
	}
	if err := sc.Err(); err != nil {
		sh.system(fmt.Sprintf("agent transcript truncated: %v", err))
	}
}
