package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tkmax/internal/harness"
)

func navigationRecord(t *testing.T, dir, name string) *harness.Record {
	t.Helper()
	s, err := harness.DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.NewRecord(dir, "/missing/codex", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.SessionName = name
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNavigationListsNamesAndDirectoriesWithoutCodex(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var out bytes.Buffer
	if err := runNavigation([]string{"ls"}, &out); err != nil || !strings.Contains(out.String(), "No saved") {
		t.Fatalf("empty list: %s, %v", &out, err)
	}
	a := navigationRecord(t, "/repo-a", "Repair the build")
	b := navigationRecord(t, "/repo-b", "Other\nname\t\x1b")
	out.Reset()
	if err := runNavigation([]string{"ls"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{a.ID, b.ID, a.SessionName, a.CWD, b.CWD} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, &out)
		}
	}
	if strings.Count(out.String(), "\n") != 3 || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("unsafe table output: %q", out.String())
	}
	if strings.Index(out.String(), b.ID) > strings.Index(out.String(), a.ID) {
		t.Fatal("newest run not first")
	}
}

func TestOpenValidationAndPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	r := navigationRecord(t, dir, "Some task")
	var out bytes.Buffer
	if err := runNavigation([]string{"open", "--print-path", r.ID}, &out); err != nil || out.String() != dir+"\n" {
		t.Fatalf("path = %q, %v", out.String(), err)
	}
	for _, args := range [][]string{
		{"open"}, {"open", r.ID, "extra"}, {"open", "--print-path", "../bad"},
		{"open", "--print-path", strings.Repeat("f", 24)}, {"ls", "extra"},
		{"ls", "--unknown"}, {"shell-init", "fish"}, {"shell-init"},
	} {
		if err := runNavigation(args, &out); err == nil {
			t.Fatalf("accepted invalid command: %v", args)
		}
	}
	if err := runNavigation([]string{"open", r.ID}, &out); err == nil || !strings.Contains(err.Error(), "shell-init") {
		t.Fatalf("missing shell setup instructions: %v", err)
	}
	missing := navigationRecord(t, filepath.Join(dir, "removed"), "Removed repo")
	if err := runNavigation([]string{"open", "--print-path", missing.ID}, &out); err == nil {
		t.Fatal("accepted removed directory")
	}
}

func TestShellHookChangesCallingShellDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	binDir := t.TempDir()
	build := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(binDir, "tkmax"), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s, %v", out, err)
	}
	dir := filepath.Join(t.TempDir(), "repo with 'quotes' $(false) `false`")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r := navigationRecord(t, dir, "Navigate here")
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skip(err)
			}
			// Data travels through positional parameters, never through shell code.
			script := `eval "$(tkmax shell-init "$1")"
tkmax open "$2" || exit 1
[ "$PWD" = "$3" ] || exit 2
tkmax ls || exit 3
tkmax open ffffffffffffffffffffffff 2>/dev/null && exit 4
[ "$PWD" = "$3" ] || exit 5
tkmax open --print-path "$2"
`
			cmd := exec.Command(shell, "-f", "-c", script, "test", shell, r.ID, dir)
			cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), r.SessionName) || !strings.HasSuffix(string(out), dir+"\n") {
				t.Fatalf("shell navigation: %s, %v", out, err)
			}
		})
	}
}
