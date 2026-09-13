package harness

import (
	"reflect"
	"testing"
)

func TestCodexOptions(t *testing.T) {
	dir := t.TempDir()
	server, tui, root, err := CodexOptions([]string{"-c", `model_reasoning_effort="high"`, "--model=example", "--search", "--no-alt-screen", "--cd", dir}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if root != dir {
		t.Fatalf("root %q", root)
	}
	if !reflect.DeepEqual(server, []string{"-c", `model_reasoning_effort="high"`, "-c", `web_search="live"`}) {
		t.Fatalf("server args: %v", server)
	}
	if !reflect.DeepEqual(tui, []string{"-c", `model_reasoning_effort="high"`, "--model", "example", "--search", "--no-alt-screen"}) {
		t.Fatalf("TUI args: %v", tui)
	}
	for _, args := range [][]string{{"--remote", "unix:///tmp/other"}, {"exec"}, {"--profile", "other"}, {"--model"}, {"--worktree"}, {"hello"}} {
		if _, _, _, err := CodexOptions(args, dir); err == nil {
			t.Fatalf("accepted unsupported arguments %v", args)
		}
	}
}
