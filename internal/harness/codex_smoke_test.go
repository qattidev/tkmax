package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in test uses the installed binary with an isolated Codex home and
// a deliberately unreachable LOCAL provider. No account or paid model is used.
func TestInstalledCodexNativeGoalContinuation(t *testing.T) {
	if os.Getenv("TKMAX_CODEX_SMOKE") != "1" {
		t.Skip("set TKMAX_CODEX_SMOKE=1 to test the installed Codex binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckCodex(ctx, bin); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(bin, "app-server", "--stdio",
		"-c", `model_provider="stub"`,
		"-c", `model_providers.stub.name="stub"`,
		"-c", `model_providers.stub.base_url="http://127.0.0.1:9"`,
		"-c", `model_providers.stub.wire_api="responses"`,
		"-c", `model_providers.stub.requires_openai_auth=false`)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CODEX_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+dir)
	stderr, err := os.Create(filepath.Join(dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	child, err := startChild(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	defer child.stop()
	packets := make(chan message, 64)
	go func() {
		defer close(packets)
		reader := bufio.NewReader(out)
		for {
			b, err := readJSONLine(reader)
			if err != nil {
				return
			}
			m, err := decode(b)
			if err != nil {
				return
			}
			select {
			case packets <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	next := func() message {
		t.Helper()
		select {
		case m, ok := <-packets:
			if !ok {
				b, _ := os.ReadFile(stderr.Name())
				t.Fatalf("Codex exited: %s", b)
			}
			return m
		case <-ctx.Done():
			t.Fatal("timed out waiting for Codex")
			return nil
		}
	}
	send := func(v any) {
		t.Helper()
		if _, err := in.Write(append(raw(v), '\n')); err != nil {
			t.Fatal(err)
		}
	}
	starts := 0
	wait := func(id int) json.RawMessage {
		t.Helper()
		for {
			m := next()
			if str(m["method"]) == "turn/started" {
				starts++
			}
			if string(m["id"]) == string(raw(id)) {
				if m["error"] != nil {
					t.Fatalf("Codex RPC error: %s", m["error"])
				}
				return m["result"]
			}
		}
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "tkmax_smoke", "version": "0.1"}, "capabilities": map[string]any{"experimentalApi": true}}})
	wait(1)
	send(map[string]any{"method": "initialized"})
	send(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": dir, "model": "gpt-5.6-terra", "modelProvider": "stub", "approvalPolicy": "never", "sandbox": "read-only"}})
	var result struct {
		Thread threadInfo `json:"thread"`
	}
	if err := json.Unmarshal(wait(2), &result); err != nil {
		t.Fatal(err)
	}
	thread := result.Thread.ID
	if thread == "" {
		t.Fatal("missing thread ID")
	}
	send(map[string]any{"id": 3, "method": "thread/goal/set", "params": map[string]any{"threadId": thread, "objective": "Reply ready.", "status": "usageLimited", "tokenBudget": 10000}})
	var before struct {
		Goal Goal `json:"goal"`
	}
	_ = json.Unmarshal(wait(3), &before)
	if before.Goal.Status != "usageLimited" || starts != 0 {
		t.Fatal("usage-limited goal started unexpectedly")
	}
	send(map[string]any{"id": 4, "method": "thread/goal/set", "params": map[string]any{"threadId": thread, "status": "active"}})
	var after struct {
		Goal Goal `json:"goal"`
	}
	_ = json.Unmarshal(wait(4), &after)
	if after.Goal.Status != "active" || after.Goal.CreatedAt != before.Goal.CreatedAt || after.Goal.Objective != before.Goal.Objective || after.Goal.TokenBudget == nil || *after.Goal.TokenBudget != 10000 {
		t.Fatal("reactivation did not preserve native goal")
	}
	for starts == 0 {
		if str(next()["method"]) == "turn/started" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("expected one continuation, got %d", starts)
	}
	t.Log("Installed Codex resumed the same goal and started one native turn without turn/start")
}
