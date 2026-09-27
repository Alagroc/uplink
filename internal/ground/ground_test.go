package ground

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Alagroc/uplink/internal/proto"
	"github.com/Alagroc/uplink/internal/store"
)

func newTestGround(t *testing.T) (*Ground, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, Options{OfflineAfter: time.Minute}, nil), st
}

func register(t *testing.T, g *Ground, name string, roles ...string) string {
	t.Helper()
	resp, err := g.Register(proto.RegisterReq{Name: name, Roles: roles, OS: "linux", Arch: "arm64", Hostname: "h", Workdir: "/w", Runners: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	return resp.CrewID
}

func TestRegisterRequiresName(t *testing.T) {
	g, _ := newTestGround(t)
	if _, err := g.Register(proto.RegisterReq{}); err == nil {
		t.Fatal("expected an error for an empty crew name")
	}
	if _, err := g.Register(proto.RegisterReq{Name: "bad name"}); err == nil {
		t.Fatal("whitespace in a crew name should be rejected: it breaks tool arguments")
	}
}

func TestSubmitDispatchesToPoller(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox", "builder")

	job, err := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "echo hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if job.AgentToken == "" {
		t.Error("every job needs an agent token so its agent can reach the channel tools")
	}

	cmd, err := g.Poll(context.Background(), crewID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Type != proto.CmdJob || cmd.Job.ID != job.ID {
		t.Fatalf("poll returned %+v", cmd)
	}
	if cmd.Job.AgentToken == "" {
		t.Error("the crew must receive the agent token to pass to the agent")
	}
}

// The token must never reach the operator surface: it is a credential that
// grants access to the agent channel.
func TestAgentTokenIsNotExposedToOperator(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, err := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := g.Job(job.ID)
	if got.AgentToken != "" {
		t.Error("Job() leaked the agent token")
	}
	for _, j := range g.Jobs("", false, 10) {
		if j.AgentToken != "" {
			t.Error("Jobs() leaked the agent token")
		}
	}
	if resolved, ok := g.JobByAgentToken(job.AgentToken); !ok || resolved.ID != job.ID {
		t.Error("the token should still resolve its own job")
	}
}

func TestPollBlocksUntilWorkArrives(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")

	type result struct {
		cmd proto.Command
		err error
	}
	done := make(chan result, 1)
	go func() {
		cmd, err := g.Poll(context.Background(), crewID, 5*time.Second)
		done <- result{cmd, err}
	}()

	// Give the poller time to park before submitting.
	time.Sleep(50 * time.Millisecond)
	if _, err := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}}); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.cmd.Type != proto.CmdJob {
			t.Fatalf("expected a job, got %q", r.cmd.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("submit did not wake the parked poller")
	}
}

func TestPollTimesOutAsHeartbeat(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	start := time.Now()
	cmd, err := g.Poll(context.Background(), crewID, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Type != proto.CmdNoop {
		t.Errorf("want a noop heartbeat, got %q", cmd.Type)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Error("poll returned before its wait elapsed")
	}
}

func TestPollUnknownCrewTellsItToReregister(t *testing.T) {
	g, _ := newTestGround(t)
	if _, err := g.Poll(context.Background(), "crew_missing", time.Millisecond); err == nil {
		t.Fatal("expected an error for an unknown crew")
	}
}

func TestSubmitValidatesPayload(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	cases := []struct {
		name string
		req  SubmitReq
	}{
		{"exec without command", SubmitReq{Crew: "devbox", Kind: proto.KindExec}},
		{"agent without prompt", SubmitReq{Crew: "devbox", Kind: proto.KindAgent}},
		{"unknown kind", SubmitReq{Crew: "devbox", Kind: "wat", Payload: proto.Payload{Command: "true"}}},
		{"unknown crew", SubmitReq{Crew: "ghost", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}}},
	}
	for _, tc := range cases {
		if _, err := g.Submit(tc.req); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

func TestSubmitByRolePicksLeastBusyCrew(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "builder-a", "builder")
	register(t, g, "builder-b", "builder")

	first, err := g.Submit(SubmitReq{Role: "builder", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.Submit(SubmitReq{Role: "builder", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.CrewName == second.CrewName {
		t.Errorf("both jobs went to %s; role dispatch should spread load", first.CrewName)
	}
}

func TestSubmitByRoleSkipsOfflineCrew(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Everything registered is immediately stale.
	g := New(st, Options{OfflineAfter: time.Nanosecond}, nil)
	register(t, g, "devbox", "builder")
	time.Sleep(time.Millisecond)

	if _, err := g.Submit(SubmitReq{Role: "builder", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}}); err == nil {
		t.Fatal("an offline crew should not be chosen by role")
	}
}

func TestSingleCrewNeedsNoTarget(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "onlyone")
	if _, err := g.Submit(SubmitReq{Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}}); err != nil {
		t.Fatalf("with one crew registered, targeting should be optional: %v", err)
	}
}

func TestReregisterInheritsQueuedJobs(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, err := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}

	// The crew restarts before ever polling.
	newID := register(t, g, "devbox")
	cmd, err := g.Poll(context.Background(), newID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Type != proto.CmdJob || cmd.Job.ID != job.ID {
		t.Fatalf("a restarted crew should inherit its queued job, got %+v", cmd)
	}
}

func TestLogsAppendAndTail(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})

	if _, err := g.AppendLogs(crewID, job.ID, []proto.LogLine{
		{Stream: proto.StreamStdout, Text: "one"},
		{Stream: proto.StreamStderr, Text: "two"},
	}); err != nil {
		t.Fatal(err)
	}
	lines, next, err := g.Logs(job.ID, 0, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || next != 2 {
		t.Fatalf("got %d lines, next_seq=%d", len(lines), next)
	}

	// Tailing from next_seq must return only new output.
	if _, err := g.AppendLogs(crewID, job.ID, []proto.LogLine{{Text: "three"}}); err != nil {
		t.Fatal(err)
	}
	lines, _, err = g.Logs(job.ID, next, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Text != "three" {
		t.Fatalf("tail returned %+v", lines)
	}

	// Stream filtering.
	lines, _, _ = g.Logs(job.ID, 0, 100, proto.StreamStderr)
	if len(lines) != 1 || lines[0].Text != "two" {
		t.Fatalf("stream filter returned %+v", lines)
	}
}

func TestLogsRejectForeignCrew(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	other := register(t, g, "intruder")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	if _, err := g.AppendLogs(other, job.ID, []proto.LogLine{{Text: "x"}}); err == nil {
		t.Fatal("one crew must not be able to write another crew's job log")
	}
}

// The round trip that the whole design exists for: an agent blocks, the
// operator answers, the agent resumes.
func TestAskBlocksUntilOperatorReplies(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "migrate"}})

	q, err := g.Ask(*job, proto.AskReq{Question: "which storage class?", Context: "tried local-path"})
	if err != nil {
		t.Fatal(err)
	}

	// Nobody has answered yet.
	resp, err := g.Await(context.Background(), q.ID, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != proto.QPending {
		t.Fatalf("state = %s, want pending", resp.State)
	}

	answered := make(chan proto.AwaitResp, 1)
	go func() {
		r, _ := g.Await(context.Background(), q.ID, 5*time.Second)
		answered <- r
	}()

	time.Sleep(50 * time.Millisecond)
	if _, err := g.Reply(q.ID, "use local-path"); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-answered:
		if r.State != proto.QAnswered || r.Answer != "use local-path" {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reply did not release the waiting agent")
	}
}

func TestReplyTwiceIsRejected(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	q, _ := g.Ask(*job, proto.AskReq{Question: "q?"})

	if _, err := g.Reply(q.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Reply(q.ID, "second"); err == nil {
		t.Error("answering the same question twice should fail")
	}
	if _, err := g.Reply(q.ID, ""); err == nil {
		t.Error("an empty answer should be rejected")
	}
}

// A finished job must not leave its agent blocked forever.
func TestFinishingAJobReleasesWaitingQuestions(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	q, _ := g.Ask(*job, proto.AskReq{Question: "q?"})

	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateFailed}); err != nil {
		t.Fatal(err)
	}
	resp, err := g.Await(context.Background(), q.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != proto.QExpired {
		t.Fatalf("state = %s, want expired", resp.State)
	}
}

// Retiring the job token stops a finished agent from using the channel.
func TestFinishedJobTokenStopsWorking(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	if _, ok := g.JobByAgentToken(job.AgentToken); !ok {
		t.Fatal("token should work while the job runs")
	}
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone}); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.JobByAgentToken(job.AgentToken); ok {
		t.Error("the token must stop working once the job is finished")
	}
}

func TestInboxOrdersOldestFirst(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})

	first, _ := g.Ask(*job, proto.AskReq{Question: "first"})
	time.Sleep(2 * time.Millisecond)
	_, _ = g.Ask(*job, proto.AskReq{Question: "second"})

	inbox := g.Inbox(true, 10)
	if len(inbox) != 2 {
		t.Fatalf("inbox has %d questions", len(inbox))
	}
	if inbox[0].ID != first.ID {
		t.Error("the agent waiting longest should be listed first")
	}

	if _, err := g.Reply(first.ID, "answered"); err != nil {
		t.Fatal(err)
	}
	if got := len(g.Inbox(true, 10)); got != 1 {
		t.Errorf("answered questions should leave the pending inbox, got %d", got)
	}
	if got := len(g.Inbox(false, 10)); got != 2 {
		t.Errorf("include_answered should show both, got %d", got)
	}
}

func TestMessagesAreDeliveredOnceAndOnlyWhileRunning(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})

	if _, err := g.Send(job.ID, "do not touch prod"); err != nil {
		t.Fatal(err)
	}
	if n := g.PeekMessages(job.ID); n != 1 {
		t.Fatalf("pending messages = %d", n)
	}
	msgs := g.TakeMessages(job.ID)
	if len(msgs) != 1 || msgs[0].Text != "do not touch prod" {
		t.Fatalf("got %+v", msgs)
	}
	if got := g.TakeMessages(job.ID); len(got) != 0 {
		t.Error("a message must not be delivered twice")
	}

	_ = g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone})
	if _, err := g.Send(job.ID, "too late"); err == nil {
		t.Error("messaging a finished job should fail rather than vanish")
	}
}

func TestCancelQueuesCommandForCrew(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "sleep 100"}})
	// Drain the job command first.
	if _, err := g.Poll(context.Background(), crewID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := g.Cancel(job.ID, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	cmd, err := g.Poll(context.Background(), crewID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Type != proto.CmdCancel || cmd.JobID != job.ID {
		t.Fatalf("got %+v", cmd)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := New(st, Options{}, nil)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	q, _ := g.Ask(*job, proto.AskReq{Question: "which storage class?"})
	st.Close()

	// Ground restarts.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	g2 := New(st2, Options{}, nil)

	restored, ok := g2.Job(job.ID)
	if !ok {
		t.Fatal("job did not survive the restart")
	}
	if restored.State != proto.StateFailed || !strings.Contains(restored.Error, "ground restarted") {
		t.Errorf("an interrupted job should be marked failed with a reason, got %s/%q", restored.State, restored.Error)
	}
	// The question is kept as history but must not look answerable: the agent
	// that asked it is unreachable, because per-job tokens are not restored.
	all := g2.Inbox(false, 10)
	if len(all) != 1 || all[0].ID != q.ID {
		t.Fatalf("the question should survive a restart as history, got %+v", all)
	}
	if all[0].State != proto.QExpired {
		t.Errorf("a restored question must not be pending, got %s", all[0].State)
	}
	if len(g2.Inbox(true, 10)) != 0 {
		t.Error("a restored question must not sit in the pending inbox inviting a reply")
	}
	if _, err := g2.Reply(q.ID, "too late"); err == nil {
		t.Error("replying to a restored question should fail rather than pretend to resume an agent")
	}
}

// A crew that reconnects is issued a new id while the jobs it is running carry
// on. Ownership must follow the crew name, or every log line and the final state
// from those jobs is rejected and they hang in "running" forever.
func TestReconnectedCrewKeepsOwnershipOfRunningJobs(t *testing.T) {
	g, _ := newTestGround(t)
	oldID := register(t, g, "devbox")
	job, err := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "long migration"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Poll(context.Background(), oldID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := g.SetJobState(oldID, proto.JobStateReq{CrewID: oldID, JobID: job.ID, State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}

	// The tunnel drops and the crew re-registers; the job is still running.
	newID := register(t, g, "devbox")
	if newID == oldID {
		t.Fatal("re-registering should mint a new crew id; the test is not exercising the bug")
	}

	if _, err := g.AppendLogs(newID, job.ID, []proto.LogLine{{Text: "still building"}}); err != nil {
		t.Errorf("logs from the reconnected crew were rejected: %v", err)
	}
	exit := 0
	if err := g.SetJobState(newID, proto.JobStateReq{CrewID: newID, JobID: job.ID, State: proto.StateDone, ExitCode: &exit}); err != nil {
		t.Fatalf("final state from the reconnected crew was rejected: %v", err)
	}
	got, _ := g.Job(job.ID)
	if got.State != proto.StateDone {
		t.Errorf("job state = %s, want done", got.State)
	}
	// And a genuinely foreign crew must still be refused.
	other := register(t, g, "somebody-else")
	if _, err := g.AppendLogs(other, job.ID, []proto.LogLine{{Text: "x"}}); err == nil {
		t.Error("an unrelated crew must not be able to write this job's log")
	}
}

// A cancel queued for a connection that died must be re-delivered, not lost.
func TestCancelIsRedeliveredAfterReconnect(t *testing.T) {
	g, _ := newTestGround(t)
	oldID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "sleep 300"}})
	if _, err := g.Poll(context.Background(), oldID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := g.SetJobState(oldID, proto.JobStateReq{CrewID: oldID, JobID: job.ID, State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := g.Cancel(job.ID, "operator changed plan"); err != nil {
		t.Fatal(err)
	}
	// A second cancel should be refused rather than queue a duplicate.
	if err := g.Cancel(job.ID, "again"); err == nil {
		t.Error("a second cancel for the same job should be refused")
	}

	// The crew never received it: the connection dropped and it re-registers.
	newID := register(t, g, "devbox")
	cmd, err := g.Poll(context.Background(), newID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Type != proto.CmdCancel || cmd.JobID != job.ID {
		t.Fatalf("the reconnected crew should receive the lost cancel, got %+v", cmd)
	}
}

// A late or retried "running" must not resurrect a finished job: its agent token
// is already retired, so it would hang in running with a dead credential.
func TestTerminalJobCannotReturnToRunning(t *testing.T) {
	g, _ := newTestGround(t)
	crewID := register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	exit := 0
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateDone, ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}
	if err := g.SetJobState(crewID, proto.JobStateReq{CrewID: crewID, JobID: job.ID, State: proto.StateRunning}); err == nil {
		t.Error("a finished job must not go back to running")
	}
	got, _ := g.Job(job.ID)
	if got.State != proto.StateDone {
		t.Errorf("state = %s, want it to stay done", got.State)
	}
}

// When the asking agent stops waiting, the question must stop looking live —
// otherwise reply() tells the operator it resumed an agent that is not there.
func TestAbandonedQuestionCannotBeAnswered(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	q, _ := g.Ask(*job, proto.AskReq{Question: "still there?"})

	g.Abandon(q.ID, "released: the agent stopped waiting")

	if len(g.Inbox(true, 10)) != 0 {
		t.Error("an abandoned question must leave the pending inbox")
	}
	if _, err := g.Reply(q.ID, "here you go"); err == nil {
		t.Error("replying to an abandoned question should fail")
	}
	// Abandoning twice, or abandoning an answered question, must be harmless.
	g.Abandon(q.ID, "again")
}

func TestAbandonDoesNotTouchAnAnsweredQuestion(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	q, _ := g.Ask(*job, proto.AskReq{Question: "q?"})
	if _, err := g.Reply(q.ID, "the real answer"); err != nil {
		t.Fatal(err)
	}
	g.Abandon(q.ID, "too late")

	resp, err := g.Await(context.Background(), q.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != proto.QAnswered || resp.Answer != "the real answer" {
		t.Errorf("an answered question must not be overwritten, got %+v", resp)
	}
}

// Messages are at-least-once: a batch stays in flight until the agent's next
// call proves the previous response arrived.
func TestMessagesAreRedeliveredUntilConfirmed(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})

	if _, err := g.Send(job.ID, "do not touch prod"); err != nil {
		t.Fatal(err)
	}
	first := g.TakeMessages(job.ID)
	if len(first) != 1 {
		t.Fatalf("got %d messages", len(first))
	}
	// Still counted as outstanding: the response carrying it may never land.
	if n := g.PeekMessages(job.ID); n != 1 {
		t.Errorf("an unconfirmed message should still count as pending, got %d", n)
	}
	// The next call confirms the previous batch and returns nothing new.
	if got := g.TakeMessages(job.ID); len(got) != 0 {
		t.Errorf("a confirmed message must not be delivered again, got %+v", got)
	}
	if n := g.PeekMessages(job.ID); n != 0 {
		t.Errorf("pending count should be 0 after confirmation, got %d", n)
	}
}

func TestLogBufferIsBoundedByBytes(t *testing.T) {
	line := proto.LogLine{Text: strings.Repeat("x", 100000)}
	buf := make([]proto.LogLine, 0, 100)
	for range 100 {
		buf = append(buf, line)
	}
	trimmed := trimLogBuffer(buf)
	total := 0
	for _, l := range trimmed {
		total += len(l.Text)
	}
	if total > logBytesInMemory {
		t.Errorf("buffer holds %d bytes, over the %d cap", total, logBytesInMemory)
	}
	if len(trimmed) == 0 {
		t.Error("trimming should keep the most recent lines, not drop everything")
	}
}

// --- tool-level tests, exercising the surface a model actually sees ---

func toolCall(t *testing.T, g *Ground, name string, args any) (string, error) {
	t.Helper()
	srv := newToolServer(g, name)
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return srv(context.Background(), raw)
}

func newToolServer(g *Ground, name string) func(context.Context, json.RawMessage) (string, error) {
	s := mcpServerFor(g)
	for _, tool := range s {
		if tool.Name == name {
			return tool.Handler
		}
	}
	return func(context.Context, json.RawMessage) (string, error) {
		return "", errNoTool(name)
	}
}

type errNoTool string

func (e errNoTool) Error() string { return "no such tool: " + string(e) }

func TestToolListCrewGuidesWhenEmpty(t *testing.T) {
	g, _ := newTestGround(t)
	out, err := toolCall(t, g, "list_crew", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "uplink crew") {
		t.Errorf("an empty crew list should tell the operator how to start one, got: %s", out)
	}
}

func TestToolSubmitAndStatusRoundTrip(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox", "builder")

	out, err := toolCall(t, g, "submit_job", map[string]any{"kind": "exec", "crew": "devbox", "command": "uname -a"})
	if err != nil {
		t.Fatal(err)
	}
	jobID := extractJobID(out)
	if jobID == "" {
		t.Fatalf("submit_job did not report a job id: %s", out)
	}

	status, err := toolCall(t, g, "job_status", map[string]any{"job_id": jobID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "queued") || !strings.Contains(status, "devbox") {
		t.Errorf("job_status output unhelpful: %s", status)
	}
}

func TestToolSubmitAgentMentionsInbox(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	out, err := toolCall(t, g, "submit_job", map[string]any{"kind": "agent", "crew": "devbox", "prompt": "migrate the stack"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "inbox") {
		t.Errorf("an agent job should remind the operator to watch the inbox, got: %s", out)
	}
}

func TestToolInboxAndReply(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	q, _ := g.Ask(*job, proto.AskReq{Question: "bind mount or PVC?", Context: "compose used /mnt/scratch", Options: []string{"PVC", "hostPath"}, Urgency: "high"})

	out, err := toolCall(t, g, "inbox", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{q.ID, "bind mount or PVC?", "compose used /mnt/scratch", "PVC | hostPath", "WAITING"} {
		if !strings.Contains(out, want) {
			t.Errorf("inbox output missing %q:\n%s", want, out)
		}
	}

	if _, err := toolCall(t, g, "reply", map[string]any{"question_id": q.ID, "answer": "PVC with local-path"}); err != nil {
		t.Fatal(err)
	}
	if got := g.Inbox(true, 10); len(got) != 0 {
		t.Error("reply should clear the question from the pending inbox")
	}
}

func TestAgentToolsRequireAJobContext(t *testing.T) {
	g, _ := newTestGround(t)
	handler := agentToolHandler(g, "ask_operator")
	if _, err := handler(context.Background(), json.RawMessage(`{"question":"hi"}`)); err == nil {
		t.Fatal("agent tools must refuse a connection with no job token")
	}
}

func TestAgentAskTimesOutWithUsableGuidance(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})

	handler := agentToolHandler(g, "ask_operator")
	ctx := WithJob(context.Background(), *job)
	_, err := handler(ctx, json.RawMessage(`{"question":"anyone there?","timeout_s":1}`))
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	// The agent has to know what to do next, not just that it failed.
	if !strings.Contains(err.Error(), "reversible") {
		t.Errorf("timeout message should tell the agent how to proceed, got: %v", err)
	}
}

func TestAgentProgressAndMessagePickup(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	ctx := WithJob(context.Background(), *job)

	if _, err := g.Send(job.ID, "skip the redis service"); err != nil {
		t.Fatal(err)
	}

	out, err := agentToolHandler(g, "report_progress")(ctx, json.RawMessage(`{"text":"converted 3 services","phase":"convert"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Messages ride along on whatever tool the agent calls next.
	if !strings.Contains(out, "skip the redis service") {
		t.Errorf("a queued operator message should be attached to the next tool result, got: %s", out)
	}

	lines, _, err := g.Logs(job.ID, 0, 10, proto.StreamAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0].Text, "[convert] converted 3 services") {
		t.Errorf("progress should land in the job log, got %+v", lines)
	}

	out, err = agentToolHandler(g, "check_messages")(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No messages") {
		t.Errorf("messages should not be delivered twice, got: %s", out)
	}
}

func TestAgentTaskCompleteRecordsSummary(t *testing.T) {
	g, _ := newTestGround(t)
	register(t, g, "devbox")
	job, _ := g.Submit(SubmitReq{Crew: "devbox", Kind: proto.KindAgent, Payload: proto.Payload{Prompt: "x"}})
	ctx := WithJob(context.Background(), *job)

	if _, err := agentToolHandler(g, "task_complete")(ctx, json.RawMessage(`{"summary":"converted compose to 4 manifests"}`)); err != nil {
		t.Fatal(err)
	}
	status, err := toolCall(t, g, "job_status", map[string]any{"job_id": job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "converted compose to 4 manifests") {
		t.Errorf("the agent summary should show in job_status, got: %s", status)
	}
}

func extractJobID(s string) string {
	for _, field := range strings.Fields(s) {
		if strings.HasPrefix(field, "job_") {
			return field
		}
	}
	return ""
}
