# Testing

```sh
make test        # everything, with the race detector
go test ./...    # faster, no race detector
```

144 tests across 8 packages. All pass under `-race`.

| Package | Tests | Covers |
| --- | --- | --- |
| `internal/mcp` | 18 | protocol negotiation, tool dispatch, schema shape, stdio and HTTP transports |
| `internal/ground` | 41 | crew registry, dispatch, the question/answer round trip, credential isolation, restart recovery |
| `internal/crew` | 11 | workdir boundary enforcement, agent transcript condensation |
| `internal/runner` | 15 | launch specs for all three CLIs, runner preference order, generated MCP config, token handling |
| `internal/bridge` | 10 | stdio↔HTTP pipe, concurrency under a blocking call, failure reporting |
| `internal/store` | 5 | append-only log, replay, crash tolerance, file permissions |
| `test` | 16 | end-to-end against the real binary: ground + a crew process + real jobs |
| `cmd/uplink` | 6 | log timestamping: date rollover, shared state across components, concurrency, opt-out |

The end-to-end tests build `uplink` and run an actual ground daemon and crew
process over a real socket. The stand-in for the agent is a Python script that
is a genuine MCP client — it spawns `uplink radio` and speaks JSON-RPC to it
over stdio, exactly as the real CLIs do. Only the reasoning is scripted, so the
transport, blocking, credentials and log shipping under test are all real.

## What the tests pin down

**The blocking round trip.** An agent calls `ask_operator`; the job stays
`running` while it waits; the question appears in the inbox with its context and
options; a `reply` releases the agent; the answer arrives verbatim; the agent
finishes. Also the failure modes: nobody answers and the agent is released with
guidance it can act on, and cancelling a job releases an agent blocked on it
rather than leaving it parked.

**Waiting instead of polling.** `await_job` is asserted to actually block and
wake: a waiter parked on a running job is released when the job finishes, when
its agent asks a question, and for every concurrent waiter at once. An
already-finished job returns immediately rather than holding the caller for the
full timeout, and a timeout is a normal outcome rather than an error. The
end-to-end test times it — a three-second job must take about three seconds to
report, proving it woke on completion rather than returning a snapshot or
sitting out the timeout.

**Retry safety in the bridge.** Read-only calls (`tools/list`, `initialize`,
`ping`) are retried when they fail before reaching ground, because a dropped
`tools/list` can leave a client believing uplink offers no tools at all.
`tools/call` is asserted **never** to be retried — a test counts the requests
and fails if `submit_job` is sent more than once, since dispatching a job twice
is worse than one reported failure.

**Credential scoping.** The three classes are tested against each other: a crew
token is refused on `/mcp` and `/v1/shutdown`, the operator token is refused on
every crew endpoint (with a message naming the command that fixes it), and a
token minted for `devbox` cannot register as anything else. The lateral-movement
case has its own test: crew A cannot poll, ship logs to, or set state on crew B,
while both can still serve themselves — so the check is provably not just
refusing everything. Roles on a token override whatever a crew claims. Minting
and revoking are asserted to take effect against a *running* server, since the
store is re-read when the file changes rather than at start-up only.

**Credential isolation.** The operator token is rejected on `/mcp/agent` and an
unauthenticated call is rejected on `/mcp`. Per-job tokens stop working the
moment their job finishes. The token never appears in `Job()` or `Jobs()`
output, and never in a launched agent's argv — a test greps the command line for
it, because `ps` is readable by every user on the host.

**The workdir boundary.** `..`, absolute paths outside the root, and symlinks
pointing out of the tree are all refused, end to end, with an error that says
why.

**Restart recovery.** Ground restarts: jobs and unanswered questions come back
from the event log, and a job that was running is marked failed with a reason
rather than silently disappearing. A crew that restarts re-registers and
inherits the job still queued for its name.

**Operator ergonomics.** These are the difference between a model using the
tools well and flailing: `submit_job` on an agent job reminds the caller to
watch the inbox, an empty `list_crew` explains how to start one, an unknown crew
name lists the ones that do exist, and `job_logs` reports a `next_seq` cursor so
tailing does not re-read the same output.

**Shutdown safety.** `uplink shutdown` refuses while a job is running or an
agent is waiting on an answer, and names what is in flight; `--force` overrides
and reports what it abandoned. An unauthenticated request never stops ground.
The unauthenticated health endpoint is what lets the client tell "nothing is
listening" apart from "something else owns this port".

## Findings from the pre-commit review

An independent review agent read every non-test file and verified its top
findings with throwaway probes. It confirmed there is no deadlock — no `g.mu`
critical section blocks on a channel, and both `Poll` and `Await` release the
lock before selecting and re-check state after waking. It found two genuine
critical bugs, both now fixed and both now covered by regression tests.

**A crew reconnect orphaned every running job.** `Register` minted a new crew id
on every registration but only re-pointed jobs still `queued`. A crew keeps
running its jobs across a reconnect and reports under its new id, so after any
tunnel blip every log line and the final state were rejected as "does not belong
to this crew": the job hung in `running` forever, its token was never retired,
its pending question became a dead letter, and `cancel_job` reported success
while the real process kept going. Since the README advertises surviving a
dropped tunnel, this broke the headline case. Job ownership is now keyed on crew
*name*, `Register` re-points every unfinished job, and a cancel lost with a dead
connection is re-delivered. `TestReconnectedCrewKeepsOwnershipOfRunningJobs` and
`TestCancelIsRedeliveredAfterReconnect` cover it.

**The operator token leaked into every agent's environment.** Crew is started
with `UPLINK_TOKEN` exported, and both job builders used
`cmd.Env = append(os.Environ(), ...)`. Every agent and every `exec` shell
therefore inherited the credential that authorises dispatch on *all* crew,
making the per-job token split decorative — a misbehaving or prompt-injected
agent could have read it and submitted its own jobs. Child environments are now
built explicitly with it stripped, verified both by unit test and by running a
real job that tries to print it. The residual limit (same-user access to the
token *file*) is now stated in the README rather than papered over.

Also fixed from that review: a question whose asker stopped waiting stayed
`pending` forever, so `reply()` would tell the operator it had resumed an agent
that was not listening; questions restored after a ground restart were dead
letters for the same reason; a finished job could be dragged back to `running` by
a late report; `execute` could wait forever on a pipe held by a grandchild that
escaped the process group, so `cancel_job` appeared to do nothing; the deferred
`SIGKILL` was never cancelled and could land on a recycled process group;
operator messages were marked delivered before transmission and lost if the
response was; log lines were persisted one `write` syscall at a time while
holding the global mutex; log buffers were capped by line count but not bytes
and were never pruned; a truncated stream stopped silently; and the Codex runner
put the per-job token in argv, where `ps` could read it — it now writes a `0600`
token file and passes only the path.

Three pieces of dead code went with it: `mcp.Server.ServeStdio` (~60 lines and a
whole concurrency surface, unused because both bridges proxy raw frames),
`store.ReplayLogs`, and `proto.CmdGoodbye`.

## A bug the tests caught

`TestBridgeSurvivesGarbageInput` hung for 263 seconds before being killed.

Both stdio readers used `json.Decoder`, which cannot resynchronise after a
malformed frame — it retries the same bytes forever. One bad line from a CLI
would have pinned a core and flooded stderr until the session was killed.

Fixed by reading newline-delimited frames instead, which is what the MCP stdio
transport actually specifies. A bad line is now reported and skipped. Same test,
after the fix: 0.4 seconds.

## Live run against a real agent

The scripted agent proves the plumbing. This run proves the thing works with an
actual model on the other end — a miniature of the docker-compose → k3s
migration, with a decision deliberately planted that the agent should not make
alone.

**Setup.** Ground and a crew on one host, crew running the real `claude` CLI as
its runner, workdir containing a `docker-compose.yml` with two services, where
the `api` service bind-mounts the host path `/mnt/scratch`.

**Prompt.** Convert the compose file to k3s manifests; the `/mnt/scratch` mount
is the operator's decision, so ask before writing anything for it.

**What happened**, from ground's audit log:

1. The agent read the compose file, called `report_progress`, and wrote the
   manifests that did not depend on the decision first.
2. It called `ask_operator` and blocked:

   > How should the api service's `/mnt/scratch` bind mount be represented in
   > k3s: a hostPath volume pinned to a specific node, a PVC on the local-path
   > StorageClass, an emptyDir, or dropped entirely?

   with context naming the exact compose line, four concrete options, and
   `urgency=high`. It said plainly it could not tell whether the directory held
   real data, "so I am not guessing."
3. `job_status` still showed `running` — the agent was parked, not finished.
4. The operator replied (PVC on local-path, 10Gi, ReadWriteOnce, named
   `api-scratch`, explicitly not hostPath) and, separately, sent an unprompted
   `send_message`: use a Secret for the database password.
5. The agent resumed, wrote the PVC as instructed, and picked up the queued
   message on its next tool call — the finished output includes
   `10-db-secret.yaml` and no plaintext password anywhere else, which it then
   verified with a `grep`.
6. `task_complete` recorded a summary listing all seven manifests. Exit 0.

**Result:** 7 valid manifests, 166 seconds wall clock, 15 turns, $0.74.

**The detail worth reporting.** The operator's stated reason for choosing a PVC
was "this has to survive a node rebuild." That reason is wrong — k3s's
local-path provisioner is node-local. The agent followed the instruction but
corrected the record in the file it wrote:

> k3s's local-path provisioner is itself node-local [...] It survives pod and
> PVC-remount churn, but it does NOT survive that node being rebuilt. If the
> data must outlive a node rebuild, this needs a networked StorageClass
> (Longhorn, NFS, or a CSI driver) instead.

That is the behaviour the channel exists for: not a remote process taking
orders, but a colleague who does what you asked and tells you where your
reasoning was off.

**Log condensation.** The raw agent transcript on the crew host was 116 KB.
What reached the operator was 32 readable lines — tool calls with their key
argument, the agent's own narration, failures, and progress reports. Thinking
blocks and successful tool results are dropped; the full transcript stays on the
crew host at `~/.uplink/crew/transcripts/<job_id>.jsonl`.

## CLI integration, verified against a live client

Both operator transports were driven by a real `claude` session, not simulated.

**stdio via capcom** — the documented default, using exactly the `mcpServers`
block from the README. The session reported `status: connected` and discovered
all eight operator tools as `mcp__uplink__*`. Asked to inspect the crew and read
a file on it, it called `list_crew`, `submit_job`, `job_status` and `job_logs`
in sequence and returned the remote file's contents correctly, in 6 turns.

**streamable HTTP direct to ground** — skipping capcom, with a bearer token in
the config headers. Same result: connected, tools discovered, job dispatched and
read back.

So the README's two wiring options are both confirmed working end to end.
Codex and Cursor use the same stdio mechanism and the same config shape, but
have not been run live — see below.

## Not covered by automated tests

- **Codex and cursor-agent.** Neither CLI is installed on the machine uplink was
  built on, so neither has been exercised live in either of its two roles. As a
  *crew runner*, their argument construction, MCP registration and TOML quoting
  are unit-tested but never launched — expect to adjust
  `~/.uplink/runners.json` on first use, which is exactly why the launch specs
  are data rather than code. As an *operator*, the config snippets in the README
  follow each tool's documented format and drive the same `capcom` bridge that
  Claude Code was verified against, but the wiring itself is untested.
- **Windows crew.** Process-group termination is implemented for unix only;
  the fallback kills just the direct child.
- **Long-haul tunnel behaviour.** Reconnection with backoff and crew
  re-registration are tested by killing connections locally, not across a real
  SSH link left open for hours.
