# Security

uplink executes commands on remote machines. That is what it is for, so the
boundaries are deliberate and documented in the [README](README.md#security).
This file is about reporting a problem with them.

## Reporting a vulnerability

Please report privately, through GitHub's private vulnerability reporting:

**<https://github.com/Alagroc/uplink/security/advisories/new>**

That keeps the report between us until there is a fix, and avoids publishing a
working exploit in an issue thread. Please do not open a public issue for
anything that looks exploitable.

Useful things to include, as far as you have them:

- which component — `ground`, `crew`, `capcom`/`radio`, or a runner
- the version (`uplink version`) and how ground was started
- what an attacker would need to begin with: no credentials, a crew token, a job
  token, or local access to one of the hosts
- the smallest sequence that shows the problem

I maintain this in my own time, so I cannot promise a response window. I will
acknowledge what I can reproduce and say plainly when I do not intend to fix
something.

## What is in scope

Anything that crosses one of the boundaries uplink claims to hold:

- a crew credential reaching an operator surface, or another crew's jobs,
  queue, logs or agent tokens
- a job credential reaching anything beyond its own job's agent channel
- the operator token leaking into a process started by a job
- a token appearing in argv, logs, the audit trail, or any file more permissive
  than `0600`
- a job escaping the crew's `--workdir` **by a path it was given** — see below
  for why `exec` itself is not a boundary
- remote code execution reachable without a valid credential

## What is not a vulnerability

These are documented design decisions, not oversights. Reports about them are
welcome as discussion, but they are not security issues:

- **Agent jobs run the remote CLI with permission prompts disabled.** A headless
  agent that stops at a prompt cannot do unattended work, so confinement is the
  container's job. Overridable in `runners.json`.
- **`exec` jobs run a real shell**, so `--workdir` is a guardrail against a
  careless path, not a sandbox. Only the operator token can submit jobs, so this
  was never a privilege boundary.
- **An agent can read its own crew's token** from the environment it inherits
  and act as that crew. It gains nothing beyond the job it is already running.
- **Ground trusts anyone holding the operator token**, by design. The token is
  the authorisation.
- **Transport encryption is SSH's job.** Ground refuses to bind a public
  interface and expects to be reached through a tunnel. Running it on an open
  interface with `UPLINK_ALLOW_PUBLIC_BIND=1` is unsupported.
