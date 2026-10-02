package ground

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
)

func registerClean(t *testing.T, g *Ground, name string) proto.RegisterResp {
	t.Helper()
	resp, err := g.Register(proto.RegisterReq{
		Name: name, Clean: true, OS: "linux", Arch: "amd64", Hostname: "h", Workdir: "/w",
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The case this exists for: a crew is killed mid-job, so nothing is left to
// report a terminal state and the job would sit in "running" forever.
func TestCleanStartDiscardsAStaleRunningJob(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, err := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "sleep 600"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}

	// The crew dies and comes back clean.
	resp := registerClean(t, g, "devbox")

	if len(resp.Discarded) != 1 || resp.Discarded[0] != job.ID {
		t.Fatalf("discarded = %v, want just %s", resp.Discarded, job.ID)
	}
	got, _ := g.Job(job.ID)
	if got.State != proto.StateCanceled {
		t.Errorf("state = %s, want canceled", got.State)
	}
	if !strings.Contains(got.Error, "--clean") {
		t.Errorf("the reason should say why it was discarded, got %q", got.Error)
	}
	if len(g.Jobs("", true, 10)) != 0 {
		t.Error("a discarded job must not still count as active")
	}
}

// Queued work is discarded too: "start clean" means no inherited backlog.
func TestCleanStartDiscardsQueuedJobs(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	first, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "one"}})
	second, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "two"}})

	resp := registerClean(t, g, "devbox")
	if len(resp.Discarded) != 2 {
		t.Fatalf("discarded %d jobs, want 2: %v", len(resp.Discarded), resp.Discarded)
	}

	// And the new connection must not then be handed them anyway.
	cmd, err := g.Poll(context.Background(), resp.CrewID, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Type != proto.CmdNoop {
		t.Fatalf("a clean crew should start with an empty queue, got %+v", cmd)
	}
	for _, id := range []string{first.ID, second.ID} {
		if j, _ := g.Job(id); j.State != proto.StateCanceled {
			t.Errorf("job %s = %s, want canceled", id, j.State)
		}
	}
}

// Cleaning one crew must not touch another's work.
func TestCleanStartOnlyAffectsItsOwnCrew(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	otherID := register(t, g, "other")
	mine, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "mine"}})
	theirs, _ := g.Submit(SubmitReq{Crew: "other", Kind: proto.KindExec, Payload: proto.Payload{Command: "theirs"}})
	_ = g.SetJobState(otherID, proto.JobStateReq{CrewID: otherID, JobID: theirs.ID, State: proto.StateRunning})

	resp := registerClean(t, g, "devbox")
	if len(resp.Discarded) != 1 || resp.Discarded[0] != mine.ID {
		t.Fatalf("discarded = %v, want only %s", resp.Discarded, mine.ID)
	}
	if j, _ := g.Job(theirs.ID); j.State != proto.StateRunning {
		t.Errorf("another crew's job became %s; it must be untouched", j.State)
	}
}

// Without the flag, a reconnecting crew keeps its work — that resilience is the
// default and must not regress.
func TestRegisterWithoutCleanKeepsRunningJobs(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "long"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})

	register(t, g, "devbox") // a plain reconnect

	if j, _ := g.Job(job.ID); j.State != proto.StateRunning {
		t.Errorf("state = %s; a reconnect must not discard live work", j.State)
	}
}

// An operator sitting in await_job on a job that is about to be thrown away
// must be released, not left waiting on a corpse.
func TestCleanStartReleasesAwaitJobWaiters(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "long"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})

	woken := make(chan string, 1)
	go func() {
		event, _ := g.AwaitJob(context.Background(), job.ID, 10*time.Second)
		woken <- event
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter park

	registerClean(t, g, "devbox")

	select {
	case event := <-woken:
		if event != JobEventFinished {
			t.Errorf("waiter woke with %q, want %q", event, JobEventFinished)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("discarding a job left an await_job waiter hanging")
	}
}

// A discarded job's agent must lose its credential, and its unanswered question
// must stop looking answerable.
func TestCleanStartRetiresTokensAndQuestions(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "migrate"}})
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning})
	question, err := g.Ask(*job, proto.AskReq{Question: "which storage class?"})
	if err != nil {
		t.Fatal(err)
	}

	registerClean(t, g, "devbox")

	if _, ok := g.JobByAgentToken(job.AgentToken); ok {
		t.Error("the discarded job's agent token still works")
	}
	if len(g.Inbox(true, 10)) != 0 {
		t.Error("the discarded job's question should no longer be pending")
	}
	resp, err := g.Await(context.Background(), question.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != proto.QExpired {
		t.Errorf("question state = %s, want expired", resp.State)
	}
	if _, err := g.Reply(question.ID, "too late"); err == nil {
		t.Error("replying to a discarded job's question should fail")
	}
}

func TestCleanStartWithNothingToDiscard(t *testing.T) {
	g, _ := newTestGround(t)
	resp := registerClean(t, g, "devbox")
	if len(resp.Discarded) != 0 {
		t.Errorf("discarded = %v, want none", resp.Discarded)
	}
}

// Finished jobs are history and must be left as they are.
func TestCleanStartLeavesFinishedJobsAlone(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	exit := 0
	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone, ExitCode: &exit})

	resp := registerClean(t, g, "devbox")
	if len(resp.Discarded) != 0 {
		t.Errorf("a finished job should not be discarded, got %v", resp.Discarded)
	}
	got, _ := g.Job(job.ID)
	if got.State != proto.StateDone || got.Error != "" {
		t.Errorf("finished job was altered: state=%s error=%q", got.State, got.Error)
	}
}
