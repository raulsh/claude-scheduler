-- claude-scheduler schema. Applied in full on an empty database; guarded by
-- PRAGMA user_version so re-running is a no-op.

CREATE TABLE IF NOT EXISTS tasks (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT    NOT NULL UNIQUE,
    description        TEXT    NOT NULL DEFAULT '',
    cron_expr          TEXT    NOT NULL,
    timezone           TEXT    NOT NULL DEFAULT 'Local',
    prompt             TEXT    NOT NULL,
    model              TEXT    NOT NULL DEFAULT '',
    cwd                TEXT    NOT NULL DEFAULT '',
    timeout_seconds    INTEGER NOT NULL DEFAULT 0,
    max_budget_usd     REAL    NOT NULL DEFAULT 0,
    allowed_tools      TEXT    NOT NULL DEFAULT '[]',   -- JSON array of permission rules
    -- tools restricts which built-in tools exist at all, which is a hard
    -- limit rather than a permission rule. Empty means the CLI default set.
    tools              TEXT    NOT NULL DEFAULT '[]',
    bypass_permissions INTEGER NOT NULL DEFAULT 0,
    overlap_policy     TEXT    NOT NULL DEFAULT 'skip'      CHECK (overlap_policy IN ('skip','queue','parallel')),
    gating_policy      TEXT    NOT NULL DEFAULT 'fail_fast' CHECK (gating_policy  IN ('fail_fast','skip','pause_schedule')),
    enabled            INTEGER NOT NULL DEFAULT 1,
    paused             INTEGER NOT NULL DEFAULT 0,
    paused_reason      TEXT    NOT NULL DEFAULT '',
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- A task's declared dependencies. These are exactly the pre-flight checks.
CREATE TABLE IF NOT EXISTS task_requirements (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id  INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind     TEXT    NOT NULL CHECK (kind IN ('aws_profile','mcp_server','binary')),
    target   TEXT    NOT NULL,
    required INTEGER NOT NULL DEFAULT 1,
    UNIQUE (task_id, kind, target)
);

CREATE TABLE IF NOT EXISTS executions (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id               INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
    trigger               TEXT    NOT NULL DEFAULT 'cron' CHECK (trigger IN ('cron','manual','retry')),
    status                TEXT    NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending','running','success','failure',
                                                'blocked','rate_limited','timeout','cancelled','skipped')),
    queued_at             TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    started_at            TEXT,
    finished_at           TEXT,
    claude_session_id     TEXT    NOT NULL DEFAULT '',
    claude_version        TEXT    NOT NULL DEFAULT '',
    model                 TEXT    NOT NULL DEFAULT '',
    exit_code             INTEGER,
    is_error              INTEGER,
    result_subtype        TEXT    NOT NULL DEFAULT '',
    terminal_reason       TEXT    NOT NULL DEFAULT '',
    num_turns             INTEGER NOT NULL DEFAULT 0,
    duration_ms           INTEGER NOT NULL DEFAULT 0,
    duration_api_ms       INTEGER NOT NULL DEFAULT 0,
    total_cost_usd        REAL    NOT NULL DEFAULT 0,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    permission_denials    TEXT    NOT NULL DEFAULT '[]',  -- JSON array
    mcp_snapshot          TEXT    NOT NULL DEFAULT '[]',  -- JSON, from system/init
    preflight_outcome     TEXT    NOT NULL DEFAULT 'ok'   CHECK (preflight_outcome IN ('ok','blocked','skipped')),
    result_text           TEXT    NOT NULL DEFAULT '',
    error_message         TEXT    NOT NULL DEFAULT '',
    transcript_path       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_executions_task_queued ON executions(task_id, queued_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_executions_status      ON executions(status);
CREATE INDEX IF NOT EXISTS idx_executions_queued      ON executions(queued_at DESC, id DESC);

-- Parsed NDJSON transcript. Raw stdout is also tee'd to a file on disk so a
-- pathological run cannot bloat the database.
CREATE TABLE IF NOT EXISTS execution_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    execution_id INTEGER NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    ts           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    type         TEXT    NOT NULL,
    subtype      TEXT    NOT NULL DEFAULT '',
    payload      TEXT    NOT NULL,
    UNIQUE (execution_id, seq)
);

CREATE TABLE IF NOT EXISTS check_results (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    execution_id INTEGER REFERENCES executions(id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL,
    target       TEXT    NOT NULL,
    state        TEXT    NOT NULL CHECK (state IN ('ok','needs_login','misconfigured','unavailable','unknown')),
    detail       TEXT    NOT NULL DEFAULT '',
    latency_ms   INTEGER NOT NULL DEFAULT 0,
    checked_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_check_results_target ON check_results(kind, target, checked_at DESC);
CREATE INDEX IF NOT EXISTS idx_check_results_exec   ON check_results(execution_id);

CREATE TABLE IF NOT EXISTS notifications (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type   TEXT    NOT NULL,
    channel      TEXT    NOT NULL,
    task_id      INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
    execution_id INTEGER REFERENCES executions(id) ON DELETE SET NULL,
    status       TEXT    NOT NULL DEFAULT 'sent',
    detail       TEXT    NOT NULL DEFAULT '',
    sent_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_notifications_sent ON notifications(sent_at DESC);

-- Scratch space for runs, so a task can carry state between executions
-- without inventing a file convention of its own.
--
-- Values are BLOBs because they are loaded from files and piped through the
-- CLI unchanged: a TEXT column would impose UTF-8 on data that is sometimes
-- a gzip blob or a binary export, and would corrupt it silently.
CREATE TABLE IF NOT EXISTS kv (
    key        TEXT PRIMARY KEY,
    value      BLOB NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    -- NULL means the value never expires, which is the common case.
    expires_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_kv_expires ON kv (expires_at) WHERE expires_at IS NOT NULL;
