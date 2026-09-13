package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"tkmax/internal/harness"
)

// This function runs in the caller's shell, where cd can change its directory.
// Only the path is captured; stored names and paths are never evaluated as code.
const shellHook = `tkmax() {
    if [ "$#" -eq 2 ] && [ "$1" = open ] && [ "$2" != --help ] && [ "$2" != -h ]; then
        local tkmax_dir
        tkmax_dir=$(command tkmax open --print-path "$2") || return $?
        builtin cd -- "$tkmax_dir"
    else
        command tkmax "$@"
    fi
}
`

func runNavigation(args []string, out io.Writer) error {
	mode := args[0]
	f := flag.NewFlagSet("tkmax "+mode, flag.ContinueOnError)
	f.SetOutput(out)
	f.Usage = func() { fmt.Fprint(out, help) }
	var printPath bool
	if mode == "open" {
		f.BoolVar(&printPath, "print-path", false, "print the saved working directory")
	}
	if err := f.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if mode == "shell-init" {
		if f.NArg() != 1 || (f.Arg(0) != "bash" && f.Arg(0) != "zsh") {
			return errors.New("usage: tkmax shell-init bash|zsh")
		}
		_, err := fmt.Fprint(out, shellHook)
		return err
	}
	if mode == "ls" && f.NArg() != 0 {
		return errors.New("usage: tkmax ls")
	}
	if mode == "open" && f.NArg() != 1 {
		return errors.New("usage: tkmax open [--print-path] RUN_ID")
	}
	store, err := harness.DefaultStore()
	if err != nil {
		return err
	}
	if mode == "ls" {
		records, err := store.List()
		if err != nil {
			return err
		}
		if len(records) == 0 {
			_, err = fmt.Fprintln(out, "No saved tkmax runs.")
			return err
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "RUN_ID\tSESSION\tSTATE\tREPOSITORY / DIRECTORY")
		for _, r := range records {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.ID, displayText(sessionLabel(r)), displayText(r.State), displayText(r.CWD))
		}
		return w.Flush()
	}
	r, err := store.Load(f.Arg(0))
	if err != nil {
		return err
	}
	info, err := os.Stat(r.CWD)
	if err != nil {
		return fmt.Errorf("open run directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("run directory %q is no longer a directory", r.CWD)
	}
	if !printPath {
		return errors.New("open needs shell integration to change directories: run eval \"$(tkmax shell-init bash)\" (use zsh for Zsh), then retry; or use cd -- \"$(tkmax open --print-path RUN_ID)\"")
	}
	_, err = fmt.Fprintln(out, r.CWD)
	return err
}

func sessionLabel(r *harness.Record) string {
	if r.SessionName != "" {
		return r.SessionName
	}
	if r.ThreadID != "" {
		return r.ThreadID
	}
	return "(session not started)"
}

// Keep names and paths from injecting terminal controls or extra table rows.
func displayText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
