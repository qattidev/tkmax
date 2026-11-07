# tkmax

A Go wrapper for long-running Codex goals. It launches the real Codex TUI,
waits when usage is exhausted, and resumes the same goal when quota returns.

Requires **Linux**, **Go 1.27+** to build, and **Codex CLI 0.162.1 or newer**.

## Build and run

```sh
make build
./tkmax
```

Alternatively, install the command into your Go bin directory:

```sh
make install
tkmax
```

Installation uses `GOBIN`, or `$(go env GOPATH)/bin` when `GOBIN` is unset;
ensure that directory is on your `PATH`. Override it with
`make install GOBIN=/absolute/path/to/bin`.

Run `make help` for all targets. `make run ARGS="--help"` builds and runs the
local binary with arguments. Use `make deps` to download dependencies,
`make fmt` to format Go sources, and `make clean` to remove local build output.
If your checkout has inaccessible Git metadata, use
`make build GOFLAGS=-buildvcs=false` (also supported by `make install`).
Without Make, use `go build -o tkmax ./cmd/tkmax` or `go install ./cmd/tkmax`.

Discuss the task in Codex, then enter `/goal` and describe the outcome and
completion criteria. Codex's normal goal mode continues working. When the goal
becomes usage-limited, tkmax schedules recovery and displays its next check time.
The terminal stays interactive throughout the wait.

You can steer the task normally, answer approvals, or use `/goal pause` and
`/goal resume`. Completion stops automatic recovery and leaves the result visible.
Exit Codex with `/quit` or its normal quit keys to stop the wrapper. A new run owns
a private server; a resumed run uses Codex's shared server. On exit from a resumed
run, tkmax pauses its active managed goal and interrupts that goal's current turn,
then disconnects. Other sessions and the shared server keep running. `Ctrl+C`
retains Codex's normal in-TUI behavior; it can interrupt a turn
before quitting. `SIGINT`, `SIGTERM`, and terminal hangup stop the wrapper.

## Resume and inspect

```sh
tkmax ls                     # all saved runs, across repositories
tkmax resume                 # latest unfinished run in this directory
tkmax resume RUN_ID          # exact saved run, from any directory
tkmax sessions               # native Codex sessions in this directory
tkmax sessions --all         # native sessions across directories
tkmax resume SESSION_UUID    # adopt a session originally started in Codex
tkmax status RUN_ID          # useful from another terminal during a wait
```

`tkmax ls` shows the run ID, Codex session name, last observed state, and full
repository/working directory, newest update first. Completed and stopped runs
are included. Session names follow Codex's title and rename notifications;
older records and unnamed sessions fall back to the Codex thread UUID until a
name is observed on resume. Runs that have not started a session are labelled
accordingly. The run ID stays stable when a session is renamed.

To navigate to a run's directory in your current shell, enable the shell hook:

```sh
eval "$(tkmax shell-init zsh)"  # add to ~/.zshrc; use bash and ~/.bashrc for Bash
tkmax open RUN_ID
```

The hook is necessary because an executable cannot change its parent shell's
directory. `open` only changes directory; it does not resume Codex. Without the
hook, use `cd -- "$(tkmax open --print-path RUN_ID)"`. Missing runs or removed
directories produce an error. `--print-path` is also available for scripts.

The run ID is printed at startup and shutdown. `tkmax status` without an ID
selects the latest unfinished run in the current directory. Use an explicit ID
to inspect a completed run. Status is the last persisted observation; an abrupt
kill can leave it stale. A graceful shutdown also records its stop time.

Resume reopens the saved Codex thread UUID through Codex's local background
server, starting that server if needed. tkmax never restarts or stops the shared
server and never falls back to a private server during resume. It preserves the
remaining wait and checks the current goal and tools before automatic recovery.
It never uses Codex's `--last` selection.

To adopt a native session, stop any active turn and close its original CLI/IDE
chat, then use its UUID from `tkmax sessions`. tkmax uses that session's saved
directory and creates a run record. Repeating adoption reuses the same run and
deadline. Active sessions, pending approvals/input, ephemeral sessions, and
subagent threads cannot be adopted. Idle status alone does not prove another
client has closed; using multiple clients on the managed chat is unsupported.

Resume uses the saved Codex home, including the default home resolved for new
records. Older records without a saved home use the current `CODEX_HOME`, or
`~/.codex`. Launch overrides are applied to the resumed thread without changing
global daemon configuration. Unsupported saved overrides fail explicitly.

An explicitly paused, blocked, or budget-limited goal remains under your control;
resume it inside the TUI when appropriate. Only one wrapper may own a saved run.
Ownership also covers the same native thread referenced by different run records.
Codex may display its native "Resume paused goal?" dialog when reopening a
paused goal. Choose "Leave paused" to review it without restarting work.
A goal paused by tkmax on exit stays paused until you use `/goal resume`. A
usage-limited goal retains its deadline when you exit while waiting.

If a required app or MCP tool is unavailable after resume, `tkmax status` reports
`needs-attention` and identifies the service. Restore its login/configuration in
Codex, then use `/goal resume` to recheck it. Tools supplied by an original
client's custom handler require that client; the Codex TUI cannot execute those
handlers. Automatic recovery stays disabled while those handlers are unavailable.

## Options

```sh
tkmax --codex-bin /path/to/codex --fallback-wait 5h
tkmax --reserve-percent 5 -- --model gpt-6-astra --sandbox workspace-write
tkmax -- --cd /path/to/project --no-alt-screen
tkmax -- -c 'model_reasoning_effort="high"'
```

Place Codex options after `--`. Run `tkmax --help` for the complete supported
list. `--reserve-percent` (default `5`) pauses a working goal when only that
percentage of the weekly quota remains, leaving the rest for other jobs; set `0`
to disable. Existing Codex login and default configuration are used, and `-c` overrides
are passed to both the TUI and private server on new runs. Launch arguments and the
`CODEX_HOME` are retained for resume. Profiles, image arguments, managed worktrees,
arbitrary Codex subcommands, and external `--remote` endpoints are not supported
in this first version; unsupported arguments fail explicitly.
Saved `--strict-config`, `--dangerously-bypass-hook-trust`, and `--add-dir`
invocations cannot be resumed on the shared server and produce an explicit error.
The last restriction avoids replacing inherited directory grants with a partial
list. These options remain available for new private-server runs.

## Waiting behavior

- This is quota-based, not a five-hours-on/five-hours-off timer. Runtime alone
  never interrupts a working goal.
- While a goal is working, tkmax checks the weekly quota at each turn boundary
  and pauses the goal once usage reaches the `--reserve-percent` threshold. The
  goal then stays paused for manual `/goal resume`; overshoot is bounded to one
  in-flight turn.
- Exhausted windows supply the next check time, with a 30-second reset buffer.
  Longer limits, including weekly limits, can extend the wait.
- Missing reset information uses `--fallback-wait`, defaulting to five hours.
- At the deadline, tkmax rechecks goal, thread, approvals, and account allowance.
  An explicit quota denial prevents recovery. If allowance is unavailable, a
  scheduled attempt can probe recovery; failure causes another wait.
- Manual pauses, interrupted turns, pending approvals, user input, changed goals,
  and completion take precedence over a scheduled retry. Switching conversations
  stops tkmax recovery for the previous foreground conversation.
- Ordinary turn continuation belongs to Codex. tkmax only reactivates usage-limited
  native goals; it does not send repeated “continue” prompts or change models.
- A lost recovery response is reconciled from native goal state before another
  attempt. Remaining uncertainty causes a fallback wait instead of immediate replay.

The machine and shell must remain available to run work. After machine sleep,
the next tick compares the persisted deadline with the current time. Terminal
closure stops the wrapper; use explicit `tkmax resume` afterward. Shared-server
shutdown has a bounded attempt to pause the managed goal and interrupt its turn.
An unconfirmed pause is reported and saved as `needs-attention`. A forced kill or
power loss cannot perform this cleanup, and the shared server may keep working.
tkmax installs no additional daemon, automatic login startup, or sleep inhibition.

## Implementation and state

```text
real terminal → Codex TUI ↔ Go WebSocket/JSONL bridge ↔ private app-server (new)
                                                   ↔ proxy → shared daemon (resume)
                                  │
                          quota recovery scheduler
```

The bridge uses a private Unix socket and keeps all protocol events, including
approvals, on the TUI's connection. Request IDs are remapped independently in
both directions. The scheduler observes structured goal states and calls
`thread/goal/set` with `status: active` when recovery is eligible. Native goal
continuation supplies the next turn. No terminal scraping or keystroke injection
is used. See the [Codex app-server documentation](https://learn.chatgpt.com/docs/app-server).

Run records and server diagnostics are under `$XDG_STATE_HOME/tkmax/runs`, or
`~/.local/state/tkmax/runs`. Records use atomic writes and owner-only file
permissions. They include the goal text, launch option values, thread UUID, Codex
session name, working directory,
deadline, and pending recovery intent. Codex retains its own conversation history
and credentials. A `.log` file holds private-server or proxy stderr for the latest invocation;
it also records RPC method/error codes, thread IDs, and runtime identifiers.
tkmax does not log full protocol transcripts or credentials. Run records include
identifiers of observed tool dependencies and remain readable across this update.
They are retained after exit.

## Tests

```sh
make check                   # race-enabled tests and go vet
make test                    # race-enabled tests only
make vet                     # static checks only
make smoke                   # opt-in test against the installed Codex binary
```

The default suite uses fake time for quota cycles, fallback waits, weekly limits,
manual-pause races, lost responses, and resume deadlines. It also checks protocol
routing, large messages, WebSocket transport, process-group shutdown, and state
locking. Additional fixtures cover native adoption, missing apps, MCP authentication
and startup, history paging, and shared-goal shutdown. The WebSocket integration
test requires local socket access.

The opt-in smoke tests use isolated homes and local model providers. One verifies
that native goal reactivation starts a turn without a second `turn/start` request.
Another executes a stub MCP tool, restarts the server, reopens the same thread
through the tkmax bridge, and executes the tool again. They do not use your account
or make paid model requests. Local socket access is required. Real quota exhaustion
is simulated rather than consuming an actual allowance.
