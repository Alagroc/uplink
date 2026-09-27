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
# 1. mission control. Writes ~/.uplink/token on first run.
uplink ground

# 2. a worker, in another shell, pointed at a directory it may work in
export UPLINK_TOKEN=$(uplink token)
uplink crew --name devbox --role builder --workdir ~/projects/myapp

# 3. check it from the shell, without an LLM in the loop
uplink call list_crew
uplink call submit_job '{"kind":"exec","crew":"devbox","command":"uname -a"}'
uplink call job_logs '{"job_id":"job_..."}'
```

Then point your AI CLI at it — see [Wiring up your CLI](#wiring-up-your-cli).

## The real setup: laptop → EC2 devbox → container

Your laptop runs ground. The container runs the agent. SSH carries the
connection, so **nothing listens on a public interface** and the devbox needs no
inbound ports beyond SSH.

### 1. On the laptop

```sh
uplink ground                      # listens on 127.0.0.1:8765
uplink token                       # copy this; the crew needs it
```

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
export UPLINK_TOKEN=<the token from step 1>
uplink crew --name devbox --role builder --workdir /work/myapp
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

`uplink` must be on `PATH`; use an absolute path if it is not. Changing a server
definition needs a CLI restart, though ground itself can be restarted freely
without touching any of this.

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
| `job_status` | One job, or all active jobs |
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
uplink crew --name impl-1  --role implementer --workdir /work/myapp
uplink crew --name review-1 --role reviewer   --workdir /work/myapp
uplink crew --name docs-1   --role documenter --workdir /work/myapp
```

Then from your laptop: *"send the migration to an implementer, and when it is
done have a reviewer check the diff."* Your local model dispatches by role, and
each bot's questions arrive in the same inbox tagged with its crew name.

Ground picks the least loaded online crew for a role, so adding capacity is just
starting another one. Add `--runner` to put different models behind different
roles — see [Which agent CLI does a crew run?](#which-agent-cli-does-a-crew-run).

## One notification caveat, stated plainly

MCP is pull-only: ground **cannot** interrupt your local session to announce
that a question is waiting. Your model only sees the inbox when it looks.

So the human is the notification channel. Ground rings the terminal bell, and
`--notify` runs any command you like when a question arrives:

```sh
uplink ground --notify 'notify-send "uplink: $UPLINK_CREW needs you" "$UPLINK_QUESTION"'
# macOS:
uplink ground --notify 'osascript -e "display notification \"$UPLINK_QUESTION\" with title \"uplink\""'
```

`UPLINK_CREW`, `UPLINK_JOB`, `UPLINK_QUESTION`, `UPLINK_QUESTION_ID` and
`UPLINK_URGENCY` are in the command's environment. You glance over and tell your
local session to check the inbox.

## Security

uplink executes commands on remote machines. That is the feature, so the
boundaries are deliberate:

- **Loopback only.** Ground refuses to bind a public interface. Reach it through
  an SSH tunnel; the encryption and authentication are SSH's job.
- **A token that is never in argv.** `UPLINK_TOKEN` or a `0600` token file,
  never a flag — `ps` is readable by every user on the box. Compared in constant
  time. Where a runner can only be configured on the command line (Codex), the
  token goes to a `0600` file and only its path appears in argv.
- **Separate credentials per direction.** The operator token opens `/mcp`. Each
  job gets its own token, minted at dispatch, that opens only `/mcp/agent` and
  only for that job, and is retired when the job ends. The operator token is
  stripped from the environment of every process a job starts, so an agent
  cannot simply read it and dispatch jobs of its own.
- **A workdir default, not a sandbox.** A crew refuses any job whose `workdir`
  resolves outside `--workdir`, symlinks included. Treat that as a guardrail
  against a careless path, not a boundary: an `exec` job runs a real shell, so
  `cd /` defeats it trivially, and an agent job can do the same. Only the
  operator token can submit jobs, so this was never a privilege boundary.
- **An audit trail.** Every job, command, question, answer and message is
  appended to `~/.uplink/ground/events.jsonl`, owner-readable only.

**The one loud caveat:** agent jobs launch the remote CLI with its permission
prompts disabled (`--permission-mode bypassPermissions` for Claude Code, and the
equivalent elsewhere). A headless agent that stops at a prompt is useless, so
the confinement has to come from the container, not the flag. Run crew inside a
container, mount only what the job needs, and give it credentials scoped to the
job. If you want the prompts back, override the runner spec (below).

Two limits worth knowing:

- Crew authenticate with the same token as the operator, so a machine you give
  crew access to can also act as an operator.
- Stripping `UPLINK_TOKEN` from child processes closes the easy path, not the
  class: an agent runs as the same user as the crew and can still read
  `~/.uplink/token` if that file exists on the crew host. On a devbox, prefer
  passing the token through the environment only (`export UPLINK_TOKEN=...`)
  rather than copying the token file over.

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

This is also how you plug in a CLI uplink has never heard of: give it a command,
a way to pass the prompt, and point it at `uplink radio`.

## Troubleshooting

For the operator side not connecting, see
[When it will not connect](#when-it-will-not-connect). On the crew side:

**`cannot reach ground`** — the tunnel is down or landed on the wrong interface.
Check with `curl -s http://127.0.0.1:8765/v1/health` on the crew host.

**`no token found`** — set `UPLINK_TOKEN` on the crew host. Ground creates the
token on first run; crew never does.

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
cmd/uplink/        CLI: ground, crew, capcom, radio, call, token
internal/mcp/      MCP over stdio and streamable HTTP
internal/ground/   hub: crew registry, jobs, inbox, the tool surface
internal/crew/     worker: poll loop, job execution, log shipping
internal/bridge/   stdio ↔ HTTP pipe behind capcom and radio
internal/runner/   how each agent CLI gets launched
internal/store/    append-only event log and audit trail
test/              end-to-end tests against the real binary
```

See [TESTING.md](TESTING.md) for what is covered and the live-agent results.
