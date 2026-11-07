package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tkmax/internal/harness"
)

const help = `Usage:
  tkmax [--codex-bin PATH] [--fallback-wait 5h] [--reserve-percent 5] [-- CODEX_OPTIONS...]
  tkmax resume [RUN_ID|SESSION_UUID]
  tkmax status [RUN_ID]
  tkmax ls
  tkmax sessions [--all] [--codex-bin PATH]
  tkmax open [--print-path] RUN_ID
  tkmax shell-init bash|zsh

Start Codex normally, then set /goal inside its TUI. tkmax resumes that goal
after a usage window resets. Manual pauses and goal completion leave the TUI
open. New runs use a private server; resume uses Codex's shared local server.
Exiting a resumed run pauses its active managed goal and interrupts its turn.
Other sessions and the shared server remain running.
While a goal is working, --reserve-percent (default 5) pauses it once the weekly
quota is down to that percentage, leaving the rest for other jobs; the goal then
stays paused for manual /goal resume. Set 0 to disable.

resume/status default to the latest unfinished run in the current directory.
status with an explicit ID can also inspect completed runs.
ls lists all saved runs with their Codex session names and repository directories.
sessions lists native Codex chats in this directory; --all includes other directories.
To adopt a native session, stop its turn and close its original CLI/IDE chat first.
Resume preserves quota deadlines and checks required apps/MCP tools before recovery.
To let open change your shell's directory, add eval "$(tkmax shell-init bash)"
to ~/.bashrc (use zsh and ~/.zshrc for Zsh). --print-path prints only the directory.

Supported Codex options after --:
  --model/-m, --sandbox/-s, --ask-for-approval/-a, --cd/-C, --add-dir,
  --config/-c, --enable, --disable, --search, --no-alt-screen, --strict-config,
  --approve-for-me, --dangerously-bypass-approvals-and-sandbox,
  --dangerously-bypass-hook-trust

Linux and Codex CLI 0.162.1 or newer are supported. Existing Codex login/configuration
are used. Run records live in $XDG_STATE_HOME/tkmax/runs (default:
~/.local/state/tkmax/runs). Original option values are saved privately there.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "tkmax:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "sessions":
			return runSessions(ctx, args[1:], os.Stdout)
		case "ls", "open", "shell-init":
			return runNavigation(args, os.Stdout)
		}
	}
	mode := "start"
	if len(args) > 0 && (args[0] == "resume" || args[0] == "status") {
		mode = args[0]
		args = args[1:]
	}
	f := flag.NewFlagSet("tkmax", flag.ContinueOnError)
	f.Usage = func() { fmt.Fprint(f.Output(), help) }
	bin := f.String("codex-bin", "codex", "Codex executable")
	fallback := f.Duration("fallback-wait", 5*time.Hour, "wait when the quota reset time is unavailable")
	reserve := f.Float64("reserve-percent", 5, "pause a working goal when this percent of the weekly quota remains (0 disables)")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if *fallback <= 0 {
		return errors.New("--fallback-wait must be positive")
	}
	if *reserve < 0 || *reserve > 100 {
		return errors.New("--reserve-percent must be between 0 and 100")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	store, err := harness.DefaultStore()
	if err != nil {
		return err
	}
	var record *harness.Record
	var serverArgs, tuiArgs []string
	if mode == "start" {
		var root string
		serverArgs, tuiArgs, root, err = harness.CodexOptions(f.Args(), cwd)
		if err != nil {
			return err
		}
		resolved, err := exec.LookPath(*bin)
		if err != nil {
			return err
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return err
		}
		record, err = harness.NewRecord(root, resolved, tuiArgs, *fallback)
		if err != nil {
			return err
		}
		record.ReservePercent = *reserve
		record.CodexHome, err = harness.CanonicalCodexHome("")
		if err != nil {
			return err
		}
		record.ServerMode = "private"
	} else {
		native := mode == "resume" && len(f.Args()) == 1 && harness.IsSessionID(f.Arg(0))
		if f.NFlag() != 0 && !native {
			return errors.New("resume/status use the saved configuration; wrapper flags are only supported on a new run")
		}
		if len(f.Args()) > 1 {
			return errors.New("resume/status accept at most one run ID (resume also accepts a Codex session UUID)")
		}
		if native {
			home, err := harness.CanonicalCodexHome("")
			if err != nil {
				return err
			}
			resolved, err := exec.LookPath(*bin)
			if err != nil {
				return err
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil {
				return err
			}
			record, err = harness.Adopt(ctx, store, resolved, home, f.Arg(0), *fallback)
		} else if len(f.Args()) == 1 {
			record, err = store.Load(f.Args()[0])
		} else {
			record, err = store.Latest(cwd)
		}
		if err != nil {
			return err
		}
		if mode == "status" {
			fmt.Printf("Run: %s\nSession: %s\nDirectory: %s\nThread: %s\nState: %s\n", record.ID, sessionLabel(record), record.CWD, record.ThreadID, record.State)
			if record.ServerMode != "" {
				fmt.Printf("Codex server: %s\n", record.ServerMode)
			}
			if !record.ToolsCheckedAt.IsZero() {
				fmt.Printf("Tools last checked: %s\n", record.ToolsCheckedAt.Local().Format(time.RFC1123))
			}
			if record.Goal != nil {
				fmt.Printf("Goal: %s\nGoal status: %s\n", record.Goal.Objective, record.Goal.Status)
			}
			if record.ManualHold {
				fmt.Println("Automatic recovery: paused; use /goal resume inside Codex")
			}
			if !record.NextCheck.IsZero() {
				fmt.Printf("Next check: %s\n", record.NextCheck.Local().Format(time.RFC1123))
			}
			fmt.Printf("Last update: %s\n", record.UpdatedAt.Local().Format(time.RFC1123))
			if !record.StoppedAt.IsZero() {
				fmt.Printf("Wrapper stopped: %s\n", record.StoppedAt.Local().Format(time.RFC1123))
			}
			if record.Message != "" {
				fmt.Println(record.Message)
			}
			return nil
		}
		if record.ThreadID == "" {
			return errors.New("this run has no saved Codex thread yet; start a new run")
		}
		serverArgs, tuiArgs, _, err = harness.CodexOptions(record.CodexArgs, record.CWD)
		if err != nil {
			return err
		}
	}
	return harness.Run(ctx, store, record, mode == "resume", serverArgs, tuiArgs)
}
