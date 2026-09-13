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
  tkmax [--codex-bin PATH] [--fallback-wait 5h] [-- CODEX_OPTIONS...]
  tkmax resume [RUN_ID]
  tkmax status [RUN_ID]

Start Codex normally, then set /goal inside its TUI. tkmax resumes that goal
after a usage window resets. Manual pauses and goal completion leave the TUI
open. Exit Codex normally to stop the wrapper and its private app server.

resume/status default to the latest unfinished run in the current directory.
status with an explicit ID can also inspect completed runs.

Supported Codex options after --:
  --model/-m, --sandbox/-s, --ask-for-approval/-a, --cd/-C, --add-dir,
  --config/-c, --enable, --disable, --search, --no-alt-screen, --strict-config,
  --approve-for-me, --dangerously-bypass-approvals-and-sandbox,
  --dangerously-bypass-hook-trust

Linux and Codex CLI 0.154.0 are supported. Existing Codex login/configuration
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
	mode := "start"
	if len(args) > 0 && (args[0] == "resume" || args[0] == "status") {
		mode = args[0]
		args = args[1:]
	}
	f := flag.NewFlagSet("tkmax", flag.ContinueOnError)
	f.Usage = func() { fmt.Fprint(f.Output(), help) }
	bin := f.String("codex-bin", "codex", "Codex executable")
	fallback := f.Duration("fallback-wait", 5*time.Hour, "wait when the quota reset time is unavailable")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if *fallback <= 0 {
		return errors.New("--fallback-wait must be positive")
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
		record.CodexHome = os.Getenv("CODEX_HOME")
		if record.CodexHome != "" {
			record.CodexHome, err = filepath.Abs(record.CodexHome)
			if err != nil {
				return err
			}
		}
	} else {
		if f.NFlag() != 0 {
			return errors.New("resume/status use the saved configuration; wrapper flags are only supported on a new run")
		}
		if len(f.Args()) > 1 {
			return errors.New("resume/status accept at most one run ID")
		}
		if len(f.Args()) == 1 {
			record, err = store.Load(f.Args()[0])
		} else {
			record, err = store.Latest(cwd)
		}
		if err != nil {
			return err
		}
		if mode == "status" {
			fmt.Printf("Run: %s\nDirectory: %s\nThread: %s\nState: %s\n", record.ID, record.CWD, record.ThreadID, record.State)
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
