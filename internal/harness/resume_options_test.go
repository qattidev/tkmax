package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResumeOverridesPreserveTypedThreadConfiguration(t *testing.T) {
	p, err := ResumeOverrides([]string{"--model", "saved-model", "-s", "workspace-write", "-a", "on-request", "-c", `model_reasoning_effort="high"`, "-c", `example=[1,2]`, "--enable", "apps", "--disable", "browser_use", "--search", "--add-dir", "/other", "--no-alt-screen"})
	if err != nil {
		t.Fatal(err)
	}
	config := p["config"].(map[string]any)
	if p["model"] != "saved-model" || config["model_reasoning_effort"] != "high" || config["features.apps"] != true || config["features.browser_use"] != false || config["web_search"] != "live" {
		t.Fatalf("lost saved configuration: %+v", p)
	}
	if got := string(raw(config["example"])); got != "[1,2]" {
		t.Fatal(got)
	}
	if got := string(raw(config["sandbox_workspace_write.writable_roots"])); got != `["/other"]` {
		t.Fatal(got)
	}
	for _, args := range [][]string{{"--strict-config"}, {"--dangerously-bypass-hook-trust"}, {"--unknown", "x"}, {"-c", "missing-equals"}, {"--model"}, {"--add-dir", "/x", "-c", "sandbox_workspace_write.writable_roots=[]"}} {
		if _, err := ResumeOverrides(args); err == nil {
			t.Fatalf("silently accepted unsupported saved configuration: %v", args)
		}
	}
}

func TestSharedResumeAppliesOnlyOriginalThreadOverrides(t *testing.T) {
	r := newRig(t)
	r.e.Record.ServerMode = "shared"
	r.e.ResumeConfig = map[string]any{"model": "saved", "config": map[string]any{"features.apps": true}}
	m := r.user("thread/resume", map[string]any{"threadId": "thread-1", "config": map[string]any{"other": 42}})
	var p struct {
		Model  string         `json:"model"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(m["params"], &p); err != nil {
		t.Fatal(err)
	}
	if p.Model != "saved" || p.Config["features.apps"] != true || p.Config["other"] != float64(42) {
		t.Fatalf("bad merge: %+v", p)
	}
	m = r.user("thread/resume", map[string]any{"threadId": "different"})
	if strings.Contains(string(m["params"]), "saved") {
		t.Fatal("overrides leaked to a different conversation")
	}
}

func TestSharedInitializationRejectsDifferentCodexHome(t *testing.T) {
	r := newRig(t)
	r.e.Record.ServerMode, r.e.Record.CodexHome = "shared", t.TempDir()
	m := r.user("initialize", map[string]any{"clientInfo": map[string]any{"name": "codex-tui"}})
	err := r.e.Server(raw(map[string]any{"id": m["id"], "result": map[string]any{"codexHome": t.TempDir()}}), r.now)
	if err == nil || !strings.Contains(err.Error(), "CODEX_HOME mismatch") {
		t.Fatalf("wrong home accepted: %v", err)
	}
}

func TestSelectionCannotStealOwnedThread(t *testing.T) {
	r := newRig(t)
	r.e.ClaimThread = func(id string) error { return &rpcErrorForTest{} }
	m := r.user("thread/resume", map[string]any{"threadId": "other"})
	err := r.e.Server(raw(map[string]any{"id": m["id"], "result": map[string]any{"thread": map[string]any{"id": "other"}}}), r.now)
	if err == nil || r.e.Record.ThreadID != "thread-1" {
		t.Fatal("selection stole the thread")
	}
}

type rpcErrorForTest struct{}

func (*rpcErrorForTest) Error() string { return "session already managed" }
