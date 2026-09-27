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

## Wiring up your CLI

`uplink capcom` is a stdio MCP server. Every CLI can launch one, so this works
identically across all three. Ground holds all the state; capcom is a thin pipe,
and a fresh one per session is fine.

**Claude Code** — `~/.claude.json`, or `.mcp.json` in a project:

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

Or in one line: `claude mcp add uplink -- uplink capcom --ground http://127.0.0.1:8765`

**Codex** — `~/.codex/config.toml`:

```toml
[mcp_servers.uplink]
command = "uplink"
args = ["capcom", "--ground", "http://127.0.0.1:8765"]
```

**Cursor** — `~/.cursor/mcp.json` or `.cursor/mcp.json`:

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

All three need `UPLINK_TOKEN` in the environment, or a readable
`~/.uplink/token`. Ground also serves MCP over streamable HTTP at
`http://127.0.0.1:8765/mcp` with a bearer token, if you prefer to skip capcom
and your client supports it.

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

Then from your laptop: *"send the migration to a implementer, and when it is
done have a reviewer check the diff."* Your local model dispatches by role, and
each bot's questions arrive in the same inbox tagged with its crew name.

Ground picks the least loaded online crew for a role, so adding capacity is just
starting another one.

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
Anything you leave out keeps its default, and you can add your own runner names.

This is also how you plug in a CLI uplink has never heard of: give it a command,
a way to pass the prompt, and point it at `uplink radio`.

## Troubleshooting

**`cannot reach ground`** — the tunnel is down or landed on the wrong interface.
Check with `curl -s http://127.0.0.1:8765/v1/health` on the crew host.

**`no token found`** — set `UPLINK_TOKEN`, or copy `~/.uplink/token` from the
laptop. Ground creates it on first run; crew never does.

**`runners: none found on PATH`** — the crew host has no agent CLI installed, so
only `exec` jobs will work. Install one, or point a runner spec at it.

**A job hangs at `running` with no output** — read the raw transcript on the
crew host: `~/.uplink/crew/transcripts/<job_id>.jsonl`. Everything the agent did
is there, including what uplink condensed away.

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
