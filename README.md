# claude-scheduler

Cron-scheduled Claude Code tasks behind a local web UI, with the dependencies
each task needs checked **before** it runs.

Scheduled automation that drives Claude Code is fragile for one specific
reason: the things it depends on **expire**. An AWS SSO session dies after a
few hours, an MCP connector's OAuth token lapses. A plain cron job that shells
out to `claude` will happily run against a dead credential, burn tokens, and
produce a confidently wrong answer. Nobody finds out until much later.

This service makes those dependencies first-class, checked, and visible.

## Install

```sh
make deb
sudo dpkg -i dist/claude-scheduler_0.1.0_amd64.deb
```

`postinst` figures out which user to run as (from `SUDO_USER`) and writes a
systemd drop-in with that user's `HOME`, `XDG_RUNTIME_DIR` and session bus, so
there is nothing to configure by hand:

```sh
sudo systemctl start claude-scheduler
xdg-open http://127.0.0.1:9977
```

The service must run as a real desktop user, because the `claude` CLI reads
`~/.claude/.credentials.json` and the `aws` CLI reads `~/.aws`.

The `claude` CLI is not a Debian package and therefore cannot be a dependency.
Install it separately; the service reports a clear warning until it is found,
and discovers it in `~/.local/bin` at install time.

## What it does

- **Pre-flight healthchecks.** Each task declares what it needs: AWS CLI
  profiles, MCP servers, binaries. If a dependency needs human attention the
  run is refused and recorded as `blocked`, at zero cost, rather than
  executing against a broken environment.
- **The AWS canary.** `aws sts get-caller-identity --profile X` is the first
  use case, and doubles as a canary for the whole class of expiring-credential
  problems. When it fails, the UI can drive `aws sso login --no-browser`
  itself and show you the verification code. That flow is one-way, so the
  service can complete it without anything being typed back.
- **Live streaming.** Every run's `stream-json` output is parsed, persisted,
  and pushed to the browser over SSE as a structured transcript, with
  reconnect and resume. Claude's prose is rendered as markdown, including
  headings, tables, task lists and fenced code with syntax highlighting.
  Tool results stay monospaced, because they are command output, not prose.
- **A status strip.** Each schedule shows its last five runs as coloured
  squares. Click one to open it.
- **Notifications.** Failures, blocked runs and auto-paused schedules raise a
  desktop notification over D-Bus. Slack is a webhook behind the same
  interface.

## Statuses

The list is wider than pass/fail because the distinctions drive different
responses:

| Status | Meaning |
|---|---|
| `success` | Ran, no error. |
| `failure` | Ran, finished with an error. |
| `blocked` | A dependency was unhealthy. **Never ran, nothing spent.** |
| `rate_limited` | A usage or credit limit was hit. Not a fault in the task. |
| `timeout` | Exceeded its timeout and was stopped. |
| `cancelled` | Stopped before finishing. |
| `skipped` | Suppressed by the overlap policy. |

Likewise for dependency checks: `needs_login` and `misconfigured` gate a run,
while `unavailable` and `unknown` deliberately do not. A flaky network must
never look like a dead credential, or restarts would pause every schedule.

## Tools and permissions

Two different levers, and the distinction matters:

- **Available tools** (`--tools`) restricts which built-in tools exist for the
  run. This is the only **hard** limit. A task restricted to `Read, Grep`
  genuinely has no Bash tool.
- **Pre-approved actions** (`--allowedTools`) pre-approves things that would
  otherwise prompt. It does not restrict what the model can reach for.

Nobody is present to answer a permission prompt on a scheduled run, so runs
use `--permission-prompts none`: anything that would prompt is denied
immediately rather than hanging forever. Denials land in
`permission_denials` on the execution, and **Allow these and re-run** adds
them to the task's allowlist and starts a fresh run, so the allowlist teaches
itself.

Worth knowing, because it is easy to assume otherwise: an empty allowlist is
**not** a deny-all. Claude Code auto-approves actions it judges harmless, so
`echo hello` runs even with no allow rules and `--permission-mode dontAsk`.
If a task must not be able to run commands, remove the tool rather than
merely leaving it un-allowlisted.

A per-task **bypass** switch (`--dangerously-skip-permissions`) is available,
off by default, badged in the UI wherever the task appears. With it on, a run
can take any action as your user and both fields above are ignored.

## Configuration

`/etc/claude-scheduler/config.yaml` is a conffile, so your edits survive
upgrades. Machine-specific values live in the generated drop-in at
`/etc/systemd/system/claude-scheduler.service.d/10-user.conf` instead.

Defaults bind `127.0.0.1:9977` with no auth. The API can start Claude Code
runs, which execute code as the service user, so binding beyond loopback
requires a bearer token, and the service refuses to start otherwise.

## Development

```sh
make ui        # build the SPA (needs Node >= 20.19; 22 is pinned)
make build     # static binary with the SPA embedded
make check     # gofmt, vet, tests
make run       # run against ./dev-config.yaml
make dev       # Vite dev server, proxying /api to :9977
```

The frontend lives in `web/` and builds into `internal/webui/dist`, which is
embedded with `go:embed`. `CGO_ENABLED=0` throughout, so the package ships a
single static binary and SQLite comes from `modernc.org/sqlite`.

## Notes on the claude CLI

A few behaviours this depends on, verified rather than assumed:

- `claude` is a native binary, so no Node is needed at runtime.
- `claude mcp list` **exits 0 even when servers are failed or
  unauthenticated**, and writes to stdout. The status markers are the only
  health signal; the exit code carries none.
- `~/.claude.json` holds only locally-configured MCP servers. The account-level
  `claude.ai *` connectors, usually the large majority, are resolved
  server-side, so the CLI is the only source of truth for the inventory.
- A `system/init` event carries a machine-readable `mcp_servers` array, which
  is snapshotted against every execution.
- Single NDJSON lines exceed 8 KB, so the stream is read with sized line reads
  rather than a default `bufio.Scanner`.
- Neither `--permission-prompts none` nor `--permission-mode dontAsk` is a
  deny-all; harmless commands are auto-approved even with no user settings
  loaded. `--tools` is the only hard restriction.
- A hit budget cap arrives as `subtype: error_max_budget_usd`, which is
  reported as `rate_limited` rather than a failure.
