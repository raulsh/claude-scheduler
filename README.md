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

Download the `.deb` from the [latest release](https://github.com/raulsh/claude-scheduler/releases/latest)
(`amd64` and `arm64` are published):

```sh
sudo dpkg -i claude-scheduler_*_amd64.deb
```

Or build it yourself:

```sh
make deb
sudo dpkg -i dist/claude-scheduler_0.2.0_amd64.deb
```

`postinst` figures out which user to run as (from `SUDO_USER`) and writes a
systemd drop-in with that user's `HOME`, `XDG_RUNTIME_DIR` and session bus, so
there is nothing to configure by hand:

```sh
sudo systemctl start claude-scheduler
claude-scheduler task list
```

The service listens on a Unix socket, not a port, so the CLI works immediately
and nothing is exposed to the network. A browser cannot open a socket, so the
web UI needs a loopback port, which is a second unit shipped disabled:

```sh
sudo systemctl enable --now claude-scheduler-proxy
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

A task that uses the [key/value store](#keyvalue-store) needs `Bash` in its
tool set and `Bash(claude-scheduler:*)` in its allowlist. This is the most
likely reason a KV-using task looks broken: without the rule the call is
denied, and the task carries no state forward while otherwise succeeding.

## CLI

Every command talks to the running service over its socket, using the same API
the web UI does, so a change made here reloads the schedule exactly as one made
in the browser would. Each command checks the socket is actually serving first
and says so plainly when it is not.

```sh
claude-scheduler status                       # is it up, and what does it think
claude-scheduler task list
claude-scheduler task show <id|name>          # id or name, either works
claude-scheduler task run <id|name> --follow  # trigger, then stream the transcript
claude-scheduler task pause <id|name> --reason "why"
claude-scheduler task resume <id|name>
claude-scheduler task preflight <id|name>     # check dependencies without running
claude-scheduler exec list --task <id|name> --limit 20
claude-scheduler exec logs <id> --follow
claude-scheduler exec cancel <id>
claude-scheduler health --kind aws_profile --force
```

`--json` on any command emits the daemon's own JSON rather than a table, which
is the same payload the HTTP API returns. Exit codes are meaningful, because
tasks script against this: `0` success, `1` failure, `2` a usage mistake, `3`
the daemon is unreachable, `4` not found.

The socket is found from `--socket`, then `$CLAUDE_SCHEDULER_SOCKET`, then
`server.socket` in the config if that file is readable, then the built-in
default. A config you cannot read is not an error, so an unprivileged user
still gets a working CLI.

### Key/value store

Somewhere for a task to keep state between runs, so carrying a cursor or
yesterday's report forward does not mean inventing a file convention and
getting its permissions right by hand.

```sh
claude-scheduler kv set report/cursor 8891        # a literal value
claude-scheduler kv set job/state --file state.json
cat state.json | claude-scheduler kv set job/state  # or piped
claude-scheduler kv set scratch/x tmp --ttl 24h   # expires; 7d works too
claude-scheduler kv get job/state                 # raw bytes, byte for byte
claude-scheduler kv list --prefix job/
claude-scheduler kv del job/state
```

Keys are one flat namespace, so prefixes like `report/` are the convention for
grouping. Values are opaque bytes up to 1 MiB: what goes in comes back out
identical, which is what makes `--file` and piping trustworthy. `kv get` adds
no trailing newline, and writes nothing on a miss, so both of these work:

```sh
cursor=$(claude-scheduler kv get report/cursor) || cursor=0
claude-scheduler kv get job/blob > restored.bin
```

**Using it from inside a task.** Every execution gets `CLAUDE_SCHEDULER_SOCKET`
and `CLAUDE_SCHEDULER_EXECUTION_ID` in its environment, so a prompt can just
call the CLI with no flags. Two things have to be true for that to work, and
both are per-task settings covered in [Tools and permissions](#tools-and-permissions):

- `Bash` must be in the task's tool set, since nothing else can run a command.
- the allowlist needs a rule for it, `Bash(claude-scheduler:*)`. Without one the
  run is denied; the denial is reported as the exact rule to add.

A prompt does not have to explain any of this. Every run whose tool set can
execute a command gets the store described to it in an appended system prompt
(`--append-system-prompt`), covering the commands, the flat key namespace,
TTLs and the exit code on a miss. The description is conditional on purpose:
it says to use the store only when the task's own prompt calls for state that
crosses runs, whether that is explicit ("save the cursor", "remember what you
sent") or implied ("report only what changed since last time", "continue where
the last run stopped"), and to ignore it entirely otherwise. So a task that
needs a cursor can simply say so, while a task that needs nothing carried
forward behaves as though the store were not there. The text lives in
[`internal/claude/prompt.go`](internal/claude/prompt.go); a run with no `Bash`
tool is not given it at all, since it could not act on it.

## Configuration

`/etc/claude-scheduler/config.yaml` is a conffile, so your edits survive
upgrades. Machine-specific values live in the generated drop-in at
`/etc/systemd/system/claude-scheduler.service.d/10-user.conf` instead.

The API is served on `/run/claude-scheduler/scheduler.sock`, and the socket's
file permissions are the access control. There is no token and no listening
port: the API can start Claude Code runs, which execute code as the service
user, so who may connect is a question for the filesystem. `server.socket_mode`
defaults to `0660`, which admits the service user and its group; `0600`
restricts it to that user alone, and a mode granting write to others is
rejected outright.

`claude-scheduler proxy` forwards a loopback TCP port into that socket for the
web UI. It performs no authentication, so it refuses to bind anywhere but
loopback, and it is worth being clear that a port is weaker than the socket it
fronts: every local user can reach a loopback port, while the socket is limited
to one user and one group.

## Development

```sh
make ui        # build the SPA (needs Node >= 20.19; 22 is pinned)
make build     # static binary with the SPA embedded
make check     # gofmt, vet, tests
make run       # run the daemon against ./dev-config.yaml, on a socket
make proxy     # expose that socket on :9977, for the browser and Vite
make dev       # Vite dev server, proxying /api to :9977
make snapshot  # build the release artifacts locally, publishing nothing
```

The frontend lives in `web/` and builds into `internal/webui/dist`, which is
embedded with `go:embed`. `CGO_ENABLED=0` throughout, so the package ships a
single static binary and SQLite comes from `modernc.org/sqlite`.

## Releasing

Pushing a `v*` tag runs [GoReleaser](.goreleaser.yaml) in CI, which builds both
architectures, packages the `.deb`, and attaches the artifacts to the GitHub
release for that tag:

```sh
git tag -a v0.2.0 -m 'v0.2.0' && git push origin v0.2.0
```

Every pull request builds the same artifacts with `--snapshot` and uploads the
`.deb`, so packaging breakage shows up before the tag exists.

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

## License

[0BSD](LICENSE). Use it for anything, no attribution required.
