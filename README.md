# uplink

Talk to AI agents running on other machines, from the AI CLI on your laptop.

You are working in `claude`, `codex` or `cursor-agent` on your laptop. The real
work has to happen somewhere else — a devbox with the right architecture, a
container with the toolchain, a machine that can actually reach the cluster.
uplink lets your local session dispatch an agent over there, watch it work, and
**answer its questions when it gets stuck**.

That last part is the point. A remote agent that hits a decision it should not
make alone calls `ask_operator`, which blocks. The question appears in your
local session's inbox. You answer. The remote agent resumes mid-thought, with
its full context intact.

```
   your laptop                                     devbox / container
┌────────────────────┐                          ┌──────────────────────┐
│ claude / codex /   │                          │  claude -p  (crew)   │
│ cursor-agent       │                          │         │            │
│      │             │                          │  ask_operator blocks │
│  MCP │ stdio       │                          │         │ MCP stdio  │
│      ▼             │                          │         ▼            │
│ uplink capcom ─────┼──── HTTP over SSH ───────┼──── uplink radio     │
│      │             │                          │         ▲            │
│      ▼             │                          │         │            │
│ uplink ground  ◄───┼──── crew long-poll ──────┼──── uplink crew      │
│  (mission control) │                          │                      │
└────────────────────┘                          └──────────────────────┘
```

Standard MCP at both edges; uplink's own protocol only in the middle. One static
Go binary with no dependencies, so the crew side is a single `scp` to any
architecture.

## Build

```sh
make build                 # ./bin/uplink for this machine
make cross                 # ./dist/ for linux amd64/arm64 and darwin arm64
```

Or plainly: `go build -o bin/uplink ./cmd/uplink`.

## Quick start, all on one machine

```sh
# 1. set this machine up as the operator. Creates ~/.uplink/token and prints
#    the next steps, tailored to where you installed the binary.
uplink init

# 2. mission control, in its own terminal
uplink ground

# 3. a credential for that worker, then the worker itself in another shell
uplink crew-token add devbox --role builder      # prints uplc_...
export UPLINK_TOKEN=uplc_...
uplink crew --name devbox --workdir ~/projects/myapp

# 4. check it from the shell, without an LLM in the loop
uplink call list_crew
uplink call submit_job '{"kind":"exec","crew":"devbox","command":"uname -a"}'
uplink call job_logs '{"job_id":"job_..."}'
```

Stop it again with `uplink shutdown`. That refuses while a job is running or an
agent is waiting on you, and tells you what — pass `--force` to override.

`uplink init` is worth running first even though `ground` would create the token
by itself: it is idempotent, it tells you which agent CLIs it can see on this
machine, and it prints the exact `claude mcp add` and `ssh -R` lines for your
install, so the rest of this README is mostly confirmation.

Then point your AI CLI at it — see [Wiring up your CLI](#wiring-up-your-cli).

## The real setup: laptop → EC2 devbox → container

Your laptop runs ground. The container runs the agent. SSH carries the
connection, so **nothing listens on a public interface** and the devbox needs no
inbound ports beyond SSH.

### 1. On the laptop

```sh
uplink init                        # creates ~/.uplink/token, prints your next steps
uplink ground                      # listens on 127.0.0.1:8765
uplink crew-token add devbox --role builder    # copy this; the crew needs it
```

Run `init` **only** on the machine that runs ground. A crew host takes a crew
token instead; running `init` there would generate an unrelated operator token
that authenticates against nothing.

### 2. Open a reverse tunnel from the laptop

```sh
ssh -R 8765:127.0.0.1:8765 ec2-devbox
```

This makes the devbox's own `localhost:8765` reach ground on your laptop. Use
`autossh -M 0 -R 8765:127.0.0.1:8765 ec2-devbox` to have it survive a dropped
link or a closed lid.

A crew keeps running its jobs across a reconnect, and re-attaches to them when
it comes back, so a blipped tunnel costs you the log lines in flight and nothing
else. A cancel that was in flight when the link dropped is re-delivered too.

### 3. In the container on the devbox

Run the container with `--network host` so it shares the devbox's loopback,
where the tunnel lands:

```sh
docker run --network host -it -v /work:/work myimage
```

Inside it, after you have installed and authenticated your agent CLI:

```sh
export UPLINK_TOKEN=<the crew token minted in step 1>
uplink crew --name devbox --workdir /work/myapp
```

It should print `registered with ground as "devbox"; runners: claude`.

<details>
<summary>If you cannot use <code>--network host</code></summary>

A container on the default bridge cannot reach the devbox's loopback, and
`ssh -R` binds to loopback only. Two ways round it:

- **socat on the devbox** (no sshd changes):
  `socat TCP-LISTEN:8765,bind=172.17.0.1,fork,reuseaddr TCP:127.0.0.1:8765`
  then use `--ground http://172.17.0.1:8765` in the container.
- **sshd GatewayPorts**: set `GatewayPorts clientspecified` in the devbox's
  `/etc/ssh/sshd_config`, then tunnel with
  `ssh -R 172.17.0.1:8765:127.0.0.1:8765 ec2-devbox`.

</details>

### 4. Drive it from the laptop

In your local AI CLI, once capcom is wired up:

> Ask the devbox crew to bring up the k3s stack and run the integration suite
> against it. Tell me what fails.

The model calls `submit_job`, polls `job_logs`, and you have a conversation
about the results. When the remote agent needs a decision, it lands in `inbox`.

## Connecting your AI CLI to uplink

Your CLI appears twice in an uplink setup, and keeping the two apart saves a lot
of confusion:

| | Where it runs | Who starts it | What you configure |
| --- | --- | --- | --- |
| **the operator** | your laptop | you, normally (`claude`, `codex`, `cursor-agent`) | wire it to `uplink capcom`, once |
| **the crew agent** | the devbox | `uplink crew`, per job | nothing — uplink wires it itself |

**You only ever configure the operator side.** On the devbox you install the CLI
and authenticate it, and that is all: the crew launches it per job with its
`radio` already attached, so there is no MCP config to write over there.

All three CLIs are MCP clients, so the operator wiring is the same shape
everywhere: run `uplink capcom` as a stdio MCP server. It holds no state — ground
does — so a fresh one per session is fine and several can run at once.

Everything below assumes ground is up on your laptop (`uplink ground`).

### Claude Code

```sh
claude mcp add uplink --scope user -- uplink capcom --ground http://127.0.0.1:8765
```

`--scope user` makes uplink available in every project. Use `--scope local` for
just the current directory, or `--scope project` to write a `.mcp.json` that
teammates get too. Verify with `claude mcp list`, or `/mcp` inside a session:

```
uplink: uplink capcom --ground http://127.0.0.1:8765 - ✔ Connected
```

Claude Code also speaks the HTTP transport, if you would rather not run a bridge
process:

```sh
claude mcp add uplink --scope user --transport http \
  http://127.0.0.1:8765/mcp --header "Authorization: Bearer $(uplink token)"
```

### Codex

Add the server to `~/.codex/config.toml`:

```toml
[mcp_servers.uplink]
command = "uplink"
args = ["capcom", "--ground", "http://127.0.0.1:8765"]
```

Recent Codex versions also manage this from the command line (`codex mcp add`,
`codex mcp list`); the file above is the underlying configuration either way.
Codex's own streamable-HTTP client has been behind an experimental config flag,
which is the main reason uplink ships the stdio bridge as the default path for
every CLI.

### Cursor

Add the server to `~/.cursor/mcp.json` for all projects, or `.cursor/mcp.json`
inside one:

```json
{
  "mcpServers": {
    "uplink": {
      "command": "uplink",
      "args": ["capcom", "--ground", "http://127.0.0.1:8765"]
    }
  }
}
```

The same file is what Cursor's Settings → MCP panel edits, and `cursor-agent`
reads it too, so the CLI and the editor share one definition.

### The token, and what all three need

`capcom` finds the operator token in `~/.uplink/token`, or in `UPLINK_TOKEN` if
it is set. Nothing needs it in the config, but you can pin it if your token lives
somewhere unusual — Claude Code takes `-e`, and the other two take an `env`
block:

```sh
claude mcp add uplink --scope user -e UPLINK_TOKEN="$(uplink token)" -- \
  uplink capcom --ground http://127.0.0.1:8765
```

```json
{ "mcpServers": { "uplink": {
  "command": "uplink",
  "args": ["capcom", "--ground", "http://127.0.0.1:8765"],
  "env": { "UPLINK_TOKEN": "..." }
} } }
```

`uplink` must be on `PATH`, **or the config must give its absolute path**. A
relative path like `./bin/uplink` will not work: your CLI spawns the server from
whatever directory it happens to be in, and at `--scope user` that is rarely the
one you ran `mcp add` from. `uplink init` prints an absolute path for exactly
this reason, and suggests installing the binary so the config can stay tidy.

Changing a server definition needs a CLI restart, though ground itself can be
restarted freely without touching any of this.

> Verified live: Claude Code, over both the stdio bridge and the HTTP endpoint.
> The Codex and Cursor definitions follow each tool's documented config format
> and the same `capcom` mechanism, but have not been run end to end here — see
> [TESTING.md](TESTING.md).

### Checking it actually works

Tools being listed only proves the bridge is up. Ask for something real:

> Call list_crew and tell me what is connected.

You should get your crew back with its host, OS, arch and available runners. If
no crew has registered, `list_crew` says so and gives you the command to start
one.

### When it will not connect

Run the bridge by hand — it says what it is doing and fails loudly:

```sh
uplink capcom --ground http://127.0.0.1:8765
# [capcom] bridging stdio to http://127.0.0.1:8765/mcp
```

Then, in order:

- **`no token found`** — ground has not run yet, or its token is elsewhere.
  `uplink token` prints it; `uplink token --create` makes one.
- **`connection refused`** — ground is not running, or is on another port. Check
  with `curl -s http://127.0.0.1:8765/v1/health`.
- **Connected, but no `mcp__uplink__*` tools** — something else is on that port.
  The health endpoint identifies uplink.
- **Tools work, `list_crew` is empty** — the operator side is fine; the crew has
  not registered. Read the crew's own output on the devbox.

## Restarting a crew cleanly

If a crew is killed while a job is running, nothing is left to report that the
job ended. Ground keeps showing it as `running` — holding a live agent token and
counting against that crew's load — and it will not clear by itself:

```sh
uplink crew --name devbox --workdir /work/app --clean
```

That tells ground to discard anything it still thinks this crew is doing, marks
those jobs `canceled` with the reason, releases any question their agents were
blocked on, wakes anyone sitting in `await_job`, and removes the per-job config
files a killed process left behind. Transcripts are kept — after a crash they
are exactly what you want.

Use it when the crew was killed. **Do not** use it while another copy of that
crew is running, and note that it is the *first* registration only: a crew that
merely reconnects after a dropped tunnel keeps its jobs, which is the whole
point of the reconnect.

Ground cannot do this for you automatically. A crew that reconnects mid-job and
a crew that restarted from scratch look identical from the outside — only the
crew knows whether it still has work running, which is why this is a flag on the
crew rather than something ground infers.

## Which agent CLI does a crew run?

A crew reports what it found when it registers, which is what `list_crew` shows:

```
devbox  [ONLINE]
  host: ip-10-0-1-42  linux/arm64
  runners: claude, codex
```

For an `agent` job, the CLI is chosen by the first of these that applies:

1. **What the job asked for** — `submit_job` with `runner: "codex"`. Highest
   precedence, and how you send one task to Claude and the next to Codex on the
   same host.
2. **The crew's default** — `uplink crew --runner codex`. Pins that whole crew.
3. **The most preferred CLI installed** — `claude`, then `codex`, then
   `cursor-agent`. This order is deliberate, not alphabetical accident; anything
   uplink has no built-in preference for comes after, alphabetically.

"Installed" means the spec's command resolves on the crew's `PATH`, checked at
registration. A crew with no agent CLI still registers and still runs `exec`
jobs — `list_crew` shows `runners: none found`, and an `agent` job fails with a
message naming what it looked for.

Naming a runner that is not installed fails fast with the same kind of message,
rather than silently falling back to a different CLI — if you asked for Codex,
you want to know it was not there.

This is also how you run several models against one job queue: give each
container its own crew name and `--runner`, then dispatch by role.

```sh
uplink crew --name impl-claude  --role implementer --runner claude
uplink crew --name review-codex --role reviewer    --runner codex
```

## Operator tools

What your local model sees:

| Tool | What it does |
| --- | --- |
| `list_crew` | Who is connected: roles, OS/arch, available agent runners, active jobs |
| `submit_job` | Dispatch work. Returns a `job_id` immediately — nothing blocks |
| `await_job` | Wait for a job instead of polling it: blocks until it finishes or its agent needs you |
| `job_status` | One job, or all active jobs, with the current log position |
| `job_logs` | Output, with `since_seq` for cheap incremental tailing |
| `cancel_job` | Kill the process tree; releases an agent blocked on a question |
| `inbox` | Agents waiting on you, oldest first, with the context they attached |
| `reply` | Answer one; the remote agent resumes immediately |
| `send_message` | Redirect a running agent without being asked |

`submit_job` takes `kind: "agent"` for an autonomous agent session with a
`prompt`, or `kind: "exec"` for a single `command`. Target a specific `crew`, or
a `role` and let ground pick the least busy one.

## Agent tools

What the remote agent sees — only these four. It cannot dispatch jobs, see other
crew, or read another job's traffic:

| Tool | What it does |
| --- | --- |
| `ask_operator` | Ask you a question and **block** until you answer |
| `report_progress` | Narrate a milestone, without blocking |
| `check_messages` | Collect instructions you sent unprompted |
| `task_complete` | Finish with a summary you will read in `job_status` |

## Scaling to several bots

Crew advertise roles, and jobs can target a role instead of a name. One crew per
container, several containers per host:

```sh
# one credential per workload, minted on the ground machine
uplink crew-token add impl-1   --role implementer
uplink crew-token add review-1 --role reviewer
uplink crew-token add docs-1   --role documenter

# then on each host, with its own token in UPLINK_TOKEN
uplink crew --name impl-1   --workdir /work/myapp
uplink crew --name review-1 --workdir /work/myapp
uplink crew --name docs-1   --workdir /work/myapp
```

Because roles ride on the token, a crew cannot promote itself into a role you
did not grant, and revoking one workload does not disturb the others.

Then from your laptop: *"send the migration to an implementer, and when it is
done have a reviewer check the diff."* Your local model dispatches by role, and
each bot's questions arrive in the same inbox tagged with its crew name.

Ground picks the least loaded online crew for a role, so adding capacity is just
starting another one. Add `--runner` to put different models behind different
roles — see [Which agent CLI does a crew run?](#which-agent-cli-does-a-crew-run).

## Watching a job without polling it

A job that runs for twenty minutes should cost one call that sleeps, not forty
that each re-read the same transcript:

```
await_job(job_id, timeout_s=600)
```

It blocks until the job reaches a terminal state **or its agent asks you a
question**, whichever comes first, then returns the final status and the output
produced while you waited. On a timeout it reports progress rather than failing,
so a longer job is a handful of calls end to end.

Every response ends with a cursor:

```
--- next_seq=412 — call await_job again with since_seq=412 to keep waiting
```

Pass that back and you get only what is new. `job_logs` reports the same cursor,
and `job_status` shows the log position, so you can tell whether there is new
output without fetching any.

There is no true streaming tail — the tool interface is request/response — but
waking on completion plus an incremental cursor covers what a tail is usually
for.

## One notification caveat, stated plainly

MCP is pull-only: ground **cannot** interrupt your local session to announce
that a question is waiting. Your model only sees the inbox when it looks.

`await_job` is the closest thing to a push: the *call* sleeps, so a model that
is already waiting on a job learns about a question the moment it is asked. But
a session doing something else cannot be interrupted, so for those the human is
the notification channel. Ground rings the terminal bell, and `--notify` runs
any command you like when a question arrives:

```sh
uplink ground --notify 'notify-send "uplink: $UPLINK_CREW needs you" "$UPLINK_QUESTION"'
# macOS:
uplink ground --notify 'osascript -e "display notification \"$UPLINK_QUESTION\" with title \"uplink\""'
```

`UPLINK_CREW`, `UPLINK_JOB`, `UPLINK_QUESTION`, `UPLINK_QUESTION_ID` and
`UPLINK_URGENCY` are in the command's environment. You glance over and tell your
local session to check the inbox.

## Watching what it is doing

Ground stamps every line with the time. The date is printed as its own marker
only when it rolls over, so a log left running for days stays unambiguous
without every line paying for a full date:

```
--- 2026-09-29 ---
13:54:24.013 [ground] crew "devbox" registered
13:54:27.052 [ground] devbox is waiting: which storage class should the PVC use?
--- 2026-09-30 ---
09:02:11.887 [ground] job job_9e8f906b finished: done
```

Pass `--timestamps=false` when something else already stamps the stream, such as
journald or a supervisor.

`uplink ground --debug` logs one line per request — what was asked, by which
crew, the status and how long it took:

```
13:54:24.013 [ground]   POST /v1/crew/register crew=devbox runners=claude      200   0.1ms   57B
13:54:27.045 [ground]   POST /mcp             tools/call submit_job            200   0.2ms  168B
13:54:27.046 [ground]   POST /v1/crew/poll    crew=devbox                      200   3.00s  298B
13:54:27.047 [ground]   POST /v1/crew/state   crew=devbox job_fbd1e80e -> running  200  0.0ms  14B
13:54:27.051 [ground]   POST /v1/crew/logs    crew=devbox job_fbd1e80e 3 lines 200   0.2ms   15B
13:54:31.002 [ground] ! POST /mcp             tools/list                       401   0.0ms   34B
```

Lines are described in uplink's vocabulary rather than HTTP's: MCP calls name
the tool, crew calls name the crew and job. Anything that failed is marked `!`,
so a wrong token or a crew that needs to re-register stands out.

Long `/v1/crew/poll` durations are normal — that is the long-poll waiting, and
it doubles as the crew heartbeat.

Debug mode never logs headers. The operator token and every per-job token
travel in `Authorization`, and a debug flag that printed your credentials into
terminal scrollback would be a poor trade for visibility.

For what an individual job did, prefer `job_logs`; for a full history including
what was condensed away, read the crew's raw transcript at
`~/.uplink/crew/transcripts/<job_id>.jsonl`.

### What gets condensed, and what does not

Intermediate output is summarised hard — a tool call becomes one line naming its
key argument, and ordinary messages are collapsed onto a single line and clipped.
That keeps a chatty twenty-minute job readable.

The agent's **closing message is treated differently**: it is the deliverable of
the job, not a log line. It keeps its line breaks and gets 8000 characters, so a
review or a migration summary arrives as the markdown it was written as rather
than as a flattened paragraph. Past that it is cut with a `…[truncated]` marker,
so a clipped answer never looks complete.

Nothing is lost either way. The crew writes every raw event verbatim to its
transcript before any of this, so the untouched text of even a truncated final
message is on the crew host:

```sh
# on the crew host
python3 -c "import json,sys;[print(json.loads(l).get('result','')) for l in open(sys.argv[1]) if '\"result\"' in l]" \
  ~/.uplink/crew/transcripts/<job_id>.jsonl
```

## Crew credentials

Each crew gets its own token, minted on the machine that runs ground:

```sh
uplink crew-token add devbox --role builder     # prints uplc_... once
uplink crew-token list
uplink crew-token revoke devbox                 # effective on that crew's next request
```

Only the hash is stored, in `~/.uplink/ground/crew-tokens.json` (0600), so a
token cannot be recovered — mint a replacement instead. These commands edit that
file directly, so they work whether or not ground is running, and ground picks
up a change on its next crew authentication. No restart, either way.

A crew token is bound to one crew name and opens only the crew endpoints. It
cannot dispatch jobs, read the inbox, reply to an agent, or stop ground. If the
token carries roles, they are authoritative: a crew cannot claim a role it was
not granted.

On the crew host:

```sh
export UPLINK_TOKEN=uplc_...         # or UPLINK_CREW_TOKEN, which takes precedence
uplink crew --name devbox --workdir /work/yourapp
```

`UPLINK_CREW_TOKEN` exists so a machine that is both operator and crew — a
testing box — can hold both credentials at once.

**Migrating an existing setup:** the operator token no longer works for crew.
Mint a token per crew and replace `UPLINK_TOKEN` on those hosts. The operator
token is unchanged and still drives `capcom`, `call` and `shutdown`.

## Security

uplink executes commands on remote machines. That is the feature, so the
boundaries are deliberate:

- **Loopback only.** Ground refuses to bind a public interface. Reach it through
  an SSH tunnel; the encryption and authentication are SSH's job.
- **A token that is never in argv.** `UPLINK_TOKEN` or a `0600` token file,
  never a flag — `ps` is readable by every user on the box. Compared in constant
  time. Where a runner can only be configured on the command line (Codex), the
  token goes to a `0600` file and only its path appears in argv.
- **Three credentials, each scoped to one surface.** The operator token opens
  `/mcp` and `/v1/shutdown` and stays on the ground machine. Each crew gets its
  own token, scoped to the crew endpoints and bound to one crew name. Each job
  gets its own, opening only `/mcp/agent` for that job and retired when it ends.
  The operator token is stripped from the environment of every process a job
  starts.
- **A workdir default, not a sandbox.** A crew refuses any job whose `workdir`
  resolves outside `--workdir`, symlinks included. Treat that as a guardrail
  against a careless path, not a boundary: an `exec` job runs a real shell, so
  `cd /` defeats it trivially, and an agent job can do the same. Only the
  operator token can submit jobs, so this was never a privilege boundary.
- **An audit trail.** Every job, command, question, answer and message is
  appended to `~/.uplink/ground/events.jsonl`, owner-readable only.

Found a hole in one of those? [SECURITY.md](SECURITY.md) has the reporting path
and says which of uplink's rough edges are deliberate rather than bugs.

**The one loud caveat:** agent jobs launch the remote CLI with its permission
prompts disabled (`--permission-mode bypassPermissions` for Claude Code, and the
equivalent elsewhere). A headless agent that stops at a prompt is useless, so
the confinement has to come from the container, not the flag. Run crew inside a
container, mount only what the job needs, and give it credentials scoped to the
job. If you want the prompts back, override the runner spec (below).

Limits worth knowing:

- An agent runs as the same user as its crew, so it can read that crew's token
  out of the environment and impersonate **that crew**. Since a crew token only
  reaches the crew endpoints, and only as itself, this buys it nothing beyond
  the job it is already running.
- Revocation takes effect on that crew's next request, so up to one poll
  interval (25s by default). A job already running on a revoked crew keeps
  running locally, but can no longer report logs or its result — cancel it from
  the operator side.
- **TODO, not done here:** agent jobs still launch the remote CLI with its
  permission prompts disabled, so the container remains the only confinement.
  The next security step is to run agents with normal permissions and route
  anything they would be denied into `ask_operator`, so you approve risky steps
  instead of pre-approving everything. Tracked in the handover notes.

## Customising how agents are launched

Agent CLIs change their flags. The launch specs are data, not code — drop a
`~/.uplink/runners.json` on the crew host to override the built-in defaults:

```json
{
  "claude": {
    "command": "claude",
    "args": ["-p", "{{prompt}}", "--output-format", "stream-json", "--verbose",
             "--permission-mode", "acceptEdits",
             "--append-system-prompt", "{{system}}",
             "--mcp-config", "{{mcp_config}}"],
    "mcp_style": "config-flag",
    "mcp_config_flag": "--mcp-config",
    "stream_json": true
  }
}
```

Placeholders: `{{prompt}}`, `{{system}}` (uplink's briefing for the agent), and
`{{mcp_config}}` (a generated file registering `uplink radio`). `mcp_style` is
`config-flag`, `codex-overrides`, or `none` when the agent is already configured.
Anything you leave out keeps its default, and you can add your own runner names —
a name uplink does not know is still selectable with `--runner` or the `runner`
job argument, it just sits after the built-in ones in the preference order.

**`ensure_args`** are added just before the prompt if their flag is not already
in `args`. This is how a runner keeps the flags it cannot work without: codex
ships `--dangerously-bypass-approvals-and-sandbox` this way, because it has no
other non-interactive approval mode and a crew agent has no human to approve
anything — an override that forgot the flag would produce a job that stalls
until it times out. Set `"ensure_args": []` to opt out deliberately.

**The briefing always arrives.** If a runner's `args` contain no `{{system}}`,
uplink folds the briefing into the prompt instead, under a `--- Your task ---`
separator. Only Claude Code has a flag for appending to the system prompt; for
codex and cursor-agent the briefing would otherwise never arrive, and the agent
would not know `ask_operator` exists.

### cursor-agent and the radio

cursor-agent ships with `mcp_style: "none"`, which means **uplink does not
register the radio for it** — it relies on whatever MCP config that host already
has. A cursor crew can therefore run jobs but cannot ask you anything, which is
the one thing that makes it crew rather than a shell.

Two ways to fix it on the crew host, neither verified here (cursor-agent is not
installed on the machine uplink was built on, so pick whichever matches your
version):

```json
{"cursor-agent": {
  "command": "cursor-agent",
  "args": ["-p", "{{prompt}}", "--output-format", "stream-json", "--force",
           "--mcp-config", "{{mcp_config}}"],
  "mcp_style": "config-flag",
  "mcp_config_flag": "--mcp-config",
  "stream_json": true
}}
```

...if your `cursor-agent` accepts an MCP config path. Otherwise pre-seed
`~/.cursor/mcp.json` on the crew host once, pointing at `uplink radio`, and
leave `mcp_style` as `none`:

```json
{"mcpServers": {"uplink": {
  "command": "/usr/local/bin/uplink",
  "args": ["radio", "--ground", "http://127.0.0.1:8765"]
}}}
```

The per-job token reaches it either way: the crew exports `UPLINK_JOB_TOKEN`
and `UPLINK_GROUND` into every agent process, and `uplink radio` reads the token
from the environment. Confirm it worked by checking that a job's log shows
`tool mcp__uplink__*` calls, or by having the agent call `report_progress`.

This is also how you plug in a CLI uplink has never heard of: give it a command,
a way to pass the prompt, and point it at `uplink radio`.

## Troubleshooting

For the operator side not connecting, see
[When it will not connect](#when-it-will-not-connect). On the crew side:

**`address already in use`** — a ground is probably already running. `uplink
shutdown` stops it; the error message says which case you are in, because
something else on that port is a different problem from a second uplink.

**`cannot reach ground`** — the tunnel is down or landed on the wrong interface.
Check with `curl -s http://127.0.0.1:8765/v1/health` on the crew host.

**`no token found`** — set `UPLINK_TOKEN` on the crew host to a crew token from
`uplink crew-token add <name>`.

**`the operator token no longer works for crew`** — exactly what it says: mint a
crew token and use that instead.

**`this token is for crew "x", not "y"`** — the `--name` does not match the token.
Use the name it was minted for, or mint one for this name.

**`runners: none found`** — no agent CLI on the crew's `PATH`, so only `exec`
jobs will work. Install one, or point a runner spec at it.

**A job hangs at `running` with no output** — read the raw transcript on the
crew host: `~/.uplink/crew/transcripts/<job_id>.jsonl`. Everything the agent did
is there, including what uplink condensed away.

**A job used the wrong CLI** — precedence is job, then crew default, then the
preference order; `list_crew` shows what that crew actually has.

**An agent never asks anything** — say so in the prompt. Agents default to
deciding for themselves; naming the decisions that are yours makes them escalate.

## Layout

```
cmd/uplink/        CLI: init, ground, shutdown, crew, capcom, radio, call, token, crew-token
internal/mcp/      MCP over stdio and streamable HTTP
internal/ground/   hub: crew registry, jobs, inbox, the tool surface
internal/crew/     worker: poll loop, job execution, log shipping
internal/bridge/   stdio ↔ HTTP pipe behind capcom and radio
internal/runner/   how each agent CLI gets launched
internal/store/    append-only event log and audit trail
test/              end-to-end tests against the real binary
```

See [TESTING.md](TESTING.md) for what is covered and the live-agent results.

## License

Copyright 2026 Alagroc

Licensed under the Apache License, Version 2.0. You may obtain a copy of the
licence in [LICENSE](LICENSE) or at
<http://www.apache.org/licenses/LICENSE-2.0>.

uplink has no third-party dependencies — it builds from the Go standard library
alone — so there is nothing to attribute and no `NOTICE` file to carry.
