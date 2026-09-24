# tkmax

A Go wrapper for long-running Codex goals. It launches the real Codex TUI,
waits when usage is exhausted, and resumes the same goal when quota returns.

Requires **Linux**, **Go 1.27+** to build, and **Codex CLI 0.154.0 or newer**.

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
Exit Codex with `/quit` or its normal quit keys to stop the wrapper and its private
server. `Ctrl+C` retains Codex's normal in-TUI behavior; it can interrupt a turn
before quitting. `SIGINT`, `SIGTERM`, and terminal hangup stop the wrapper.

## Resume and inspect

```sh
tkmax ls                     # all saved runs, across repositories
tkmax resume                 # latest unfinished run in this directory
tkmax resume RUN_ID          # exact saved run, from any directory
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

Resume reopens the saved Codex thread UUID, preserves the remaining wait, and
checks the current goal before acting. It never uses Codex's `--last` selection.
An explicitly paused, blocked, or budget-limited goal remains under your control;
resume it inside the TUI when appropriate. Only one wrapper may own a saved run.
Codex may display its native "Resume paused goal?" dialog when reopening a
paused goal. Choose "Leave paused" to review it without restarting work.

## Options

```sh
tkmax --codex-bin /path/to/codex --fallback-wait 5h
tkmax -- --model gpt-6-astra --sandbox workspace-write
tkmax -- --cd /path/to/project --no-alt-screen
tkmax -- -c 'model_reasoning_effort="high"'
```

Place Codex options after `--`. Run `tkmax --help` for the complete supported
list. Existing Codex login and default configuration are used, and `-c` overrides
are passed to both the TUI and server. Launch arguments and an explicitly selected
`CODEX_HOME` are retained for resume. Profiles, image arguments, managed worktrees,
arbitrary Codex subcommands, and external `--remote` endpoints are not supported
in this first version; unsupported arguments fail explicitly.

## Waiting behavior

- This is quota-based, not a five-hours-on/five-hours-off timer. Runtime alone
  never interrupts a working goal.
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
closure stops the wrapper; use explicit `tkmax resume` afterward. No daemon,
automatic login startup, or sleep inhibition is installed.

## Implementation and state

```text
real terminal → Codex TUI ↔ Go WebSocket/JSONL bridge ↔ Codex app-server
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
and credentials. A `.log` file holds app-server stderr for the latest invocation;
tkmax does not log full protocol transcripts. Run records are retained after exit.

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
locking. The WebSocket integration test requires local socket access.

The opt-in smoke test verifies native goal reactivation against the installed
Codex binary with an isolated home and an unreachable localhost model provider.
It checks that reactivation starts a turn without a second `turn/start` request;
it does not use your account or make paid model requests. Real quota exhaustion
is simulated in tests rather than consuming an actual allowance.
