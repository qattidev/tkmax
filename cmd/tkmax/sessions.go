package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"tkmax/internal/harness"
)

func runSessions(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("tkmax sessions", flag.ContinueOnError)
	f.SetOutput(out)
	all := f.Bool("all", false, "include other working directories")
	bin := f.String("codex-bin", "codex", "Codex executable")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: tkmax sessions [--all] [--codex-bin PATH]")
	}
	home, err := harness.CanonicalCodexHome("")
	if err != nil {
		return err
	}
	cwd := ""
	if !*all {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
		cwd, err = filepath.EvalSymlinks(cwd)
		if err != nil {
			return err
		}
	}
	sessions, err := harness.Sessions(ctx, *bin, home, cwd)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		_, err = fmt.Fprintln(out, "No Codex sessions found.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION_UUID\tSESSION\tSTATE\tREPOSITORY / DIRECTORY")
	for _, s := range sessions {
		name := s.Name
		if name == "" {
			name = s.ID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", displayText(s.ID), displayText(name), displayText(s.Status.Type), displayText(s.CWD))
	}
	return w.Flush()
}
