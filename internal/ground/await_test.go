package ground

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
)

// The point of AwaitJob: one call that sleeps, rather than a poll loop.
func TestAwaitJobWakesOnCompletion(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, err := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "slow"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		event string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		event, err := g.AwaitJob(context.Background(), job.ID, 10*time.Second)
		done <- result{event, err}
	}()

	time.Sleep(50 * time.Millisecond) // let the waiter park
	exit := 0
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone, ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.event != JobEventFinished {
			t.Errorf("event = %q, want %q", r.event, JobEventFinished)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("finishing the job did not wake the waiter")
	}
}

// A blocked agent needs the operator, so waiting on the job must return then
// too — otherwise the operator sleeps through the question they are needed for.
func TestAwaitJobWakesWhenTheAgentAsks(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "migrate"}})
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}

	done := make(chan string, 1)
	go func() {
		event, _ := g.AwaitJob(context.Background(), job.ID, 10*time.Second)
		done <- event
	}()

	time.Sleep(50 * time.Millisecond)
	if _, err := g.Ask(*job, proto.AskReq{Question: "which storage class?"}); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-done:
		if event != JobEventQuestion {
			t.Errorf("event = %q, want %q", event, JobEventQuestion)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a new question did not wake the waiter")
	}
}

// An already-finished job must return immediately, not hold the caller for the
// full timeout.
func TestAwaitJobReturnsImmediatelyWhenAlreadyDone(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	exit := 0
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone, ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	event, err := g.AwaitJob(context.Background(), job.ID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if event != JobEventFinished {
		t.Errorf("event = %q", event)
	}
	if time.Since(start) > time.Second {
		t.Error("a finished job should return at once")
	}
}

func TestAwaitJobTimesOutWithoutError(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "slow"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})

	event, err := g.AwaitJob(context.Background(), job.ID, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("a timeout is a normal outcome, not an error: %v", err)
	}
	if event != JobEventTimeout {
		t.Errorf("event = %q, want %q", event, JobEventTimeout)
	}
}

func TestAwaitJobRejectsUnknownJob(t *testing.T) {
	g, _ := newTestGround(t)
	if _, err := g.AwaitJob(context.Background(), "job_nope", time.Second); err == nil {
		t.Error("expected an error for an unknown job")
	}
}

// Several watchers on one job must all be released.
func TestAwaitJobWakesEveryWaiter(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "slow"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})

	done := make(chan string, 3)
	for range 3 {
		go func() {
			event, _ := g.AwaitJob(context.Background(), job.ID, 10*time.Second)
			done <- event
		}()
	}
	time.Sleep(100 * time.Millisecond)
	exit := 0
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone, ExitCode: &exit})

	for i := range 3 {
		select {
		case event := <-done:
			if event != JobEventFinished {
				t.Errorf("waiter %d got %q", i, event)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("waiter %d was never woken", i)
		}
	}
}

// --- the tool surface ---

func TestAwaitJobToolReportsProgressAndACursor(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "build"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})
	if _, err := g.AppendLogs(crewID, job.ID, []proto.LogLine{{Text: "compiling"}, {Text: "linking"}}); err != nil {
		t.Fatal(err)
	}

	out, err := toolCall(t, g, "await_job", map[string]any{"job_id": job.ID, "timeout_s": 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Still running", "compiling", "linking", "next_seq=2", "since_seq=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("await_job output missing %q:\n%s", want, out)
		}
	}

	// A second call with the cursor must not repeat what was already read.
	out, err = toolCall(t, g, "await_job", map[string]any{"job_id": job.ID, "timeout_s": 1, "since_seq": 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "compiling") {
		t.Errorf("since_seq was ignored; output repeated earlier lines:\n%s", out)
	}
	if !strings.Contains(out, "No new output") {
		t.Errorf("expected an explicit no-new-output note:\n%s", out)
	}
}

func TestAwaitJobToolSaysWhenTheAgentNeedsYou(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})
	if _, err := g.Ask(*job, proto.AskReq{Question: "hostPath or PVC?"}); err != nil {
		t.Fatal(err)
	}

	out, err := toolCall(t, g, "await_job", map[string]any{"job_id": job.ID, "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "waiting on you") || !strings.Contains(out, "inbox") {
		t.Errorf("should point the operator at the inbox:\n%s", out)
	}
}

// The cursor has to say what it is for, or it invites re-reading everything.
func TestJobLogsExplainsItsCursor(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "x"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})
	_, _ = g.AppendLogs(crewID, job.ID, []proto.LogLine{{Text: "one"}})

	out, err := toolCall(t, g, "job_logs", map[string]any{"job_id": job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "since_seq=1") || !strings.Contains(out, "await_job") {
		t.Errorf("the footer should name both ways to avoid re-reading:\n%s", out)
	}

	// Once finished, it should say there is no point coming back.
	exit := 0
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone, ExitCode: &exit})
	out, err = toolCall(t, g, "job_logs", map[string]any{"job_id": job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing more will arrive") {
		t.Errorf("a finished job should say so:\n%s", out)
	}
}

// job_status should let you tell whether there is new output without fetching it.
func TestJobStatusReportsLogPosition(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "x"}})
	_, _ = g.AppendLogs(crewID, job.ID, []proto.LogLine{{Text: "a"}, {Text: "b"}, {Text: "c"}})

	out, err := toolCall(t, g, "job_status", map[string]any{"job_id": job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "next_seq=3") {
		t.Errorf("job_status should report the log position:\n%s", out)
	}
}

func TestAwaitJobToolRejectsBadArgs(t *testing.T) {
	g, _ := newTestGround(t)
	if _, err := toolCall(t, g, "await_job", map[string]any{"job_id": "job_missing", "timeout_s": 1}); err == nil {
		t.Error("expected an error for an unknown job")
	}
}
