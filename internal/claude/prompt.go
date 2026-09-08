package claude

// KVBasePrompt describes the key/value store to a scheduled run.
//
// The store is the one thing a run cannot discover for itself: the socket is
// in its environment, but nothing tells it what the socket is for, so until
// now every task that wanted to carry state had to re-explain the CLI in its
// own prompt. Appending this to the system prompt says it once, in the same
// words, for every run.
//
// It is written as an offer with a condition attached rather than as an
// instruction. A run whose prompt needs nothing carried forward has to
// behave exactly as it did before this text existed, so the condition comes
// first and everything after it is reference material for the runs that
// actually meet it.
//
// No backticks: the value is a raw string literal, and markdown quoting
// inside it would mean splicing the literal apart around every command name
// for a formatting the model does not need.
const KVBasePrompt = `# Scheduler key/value store

You are a scheduled task running under claude-scheduler. The scheduler keeps
a key/value store for state that has to outlive this run, reachable with the
"claude-scheduler kv" command. Its socket is already in this run's
environment, so the command needs no flags and no configuration.

Use the store only when this run's prompt calls for state that crosses runs,
whether it asks outright ("save the cursor", "remember what you sent") or
implies it ("report only what changed since last time", "skip anything
already handled", "continue where the last run stopped"). If the prompt
needs nothing carried forward, ignore the store completely: do not read it,
do not write it, and do not mention it in your output.

    claude-scheduler kv get <key>          # the value on stdout, byte for byte
    claude-scheduler kv set <key> <value>  # also --file <path>, or piped stdin
    claude-scheduler kv set <key> <value> --ttl 24h   # 30m, 24h and 7d all work
    claude-scheduler kv list --prefix <p>  # keys and sizes, never values
    claude-scheduler kv del <key>          # unset a key

Keys are one flat namespace, so prefix yours with something that names this
task, as in "daily-report/cursor". Values are opaque bytes up to 1 MiB and
come back exactly as they went in. Reading a key that does not exist writes
nothing and exits 4, which makes a default easy to express:

    cursor=$(claude-scheduler kv get daily-report/cursor) || cursor=0

A first run that finds nothing stored is normal, not a failure. Write state
only once the work it describes has actually succeeded, since the next run
will trust it, and put a TTL on anything that is scratch space so a later
run cannot read something stale.`
