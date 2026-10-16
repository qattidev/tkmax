package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestMCPStubProcess(t *testing.T) {
	if os.Getenv("TKMAX_MCP_STUB") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var m message
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			os.Exit(2)
		}
		if _, ok := m["id"]; !ok {
			continue
		}
		var result any
		switch str(m["method"]) {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "tkmax-stub", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "ping", "description": "Return pong", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}, "annotations": map[string]any{"readOnlyHint": true}}}}
		case "tools/call":
			f, err := os.OpenFile(os.Getenv("TKMAX_MCP_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(3)
			}
			_, _ = f.WriteString("pong\n")
			f.Close()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}, "isError": false}
		default:
			result = map[string]any{}
		}
		fmt.Println(string(raw(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": result})))
	}
	os.Exit(0)
}

// This exercises a real tool call on each side of a server restart. Both the
// model endpoint and MCP service are local stubs in an isolated Codex home.
func TestInstalledCodexToolsSurviveBridgeReconnect(t *testing.T) {
	if os.Getenv("TKMAX_CODEX_SMOKE") != "1" {
		t.Skip("set TKMAX_CODEX_SMOKE=1 to test the installed Codex binary")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckCodex(ctx, bin); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	calls := filepath.Join(home, "mcp-calls")
	t.Setenv("TKMAX_MCP_STUB", "1")
	t.Setenv("TKMAX_MCP_CALLS", calls)
	modelErrors := make(chan string, 8)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var input struct {
			Tools []struct {
				Type  string            `json:"type"`
				Name  string            `json:"name"`
				Tools []json.RawMessage `json:"tools"`
			} `json:"tools"`
			Input []struct {
				Type   string            `json:"type"`
				Tools  []json.RawMessage `json:"tools"`
				CallID string            `json:"call_id"`
				Output json.RawMessage   `json:"output"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &input); err != nil {
			select {
			case modelErrors <- fmt.Sprintf("invalid stub model request %s (encoding %s): %v", req.URL.Path, req.Header.Get("Content-Encoding"), err):
			default:
			}
			http.Error(w, "invalid request", 500)
			return
		}
		tool := ""
		for _, v := range input.Tools {
			if strings.HasSuffix(v.Name, "stub__ping") {
				tool = v.Name
			}
		}
		codeMode := false
		for _, entry := range input.Input {
			if entry.Type == "additional_tools" {
				for _, definition := range entry.Tools {
					var ns struct {
						Name  string `json:"name"`
						Tools []struct {
							Name string `json:"name"`
						} `json:"tools"`
					}
					_ = json.Unmarshal(definition, &ns)
					if ns.Name == "functions" {
						for _, v := range ns.Tools {
							if v.Name == "exec" {
								codeMode = true
							}
						}
					}
				}
			}
		}
		if codeMode {
			tool = "mcp__stub__ping"
		}
		if tool == "" {
			select {
			case modelErrors <- "stub MCP tool/Code Mode executor was not advertised":
			default:
			}
			http.Error(w, "stub tool missing", 500)
			return
		}
		marker := "first"
		if strings.Contains(string(body), "second-marker") {
			marker = "second"
		}
		callID := "tkmax_ping_" + marker
		complete := strings.Contains(string(body), `"call_id":"`+callID+`"`) || strings.Contains(string(body), `"call_id": "`+callID+`"`)
		for _, entry := range input.Input {
			if entry.CallID == callID && strings.HasSuffix(entry.Type, "tool_call_output") && !strings.Contains(string(entry.Output), "pong") {
				select {
				case modelErrors <- "stub tool execution failed: " + string(entry.Output):
				default:
				}
			}
		}
		// The first request has no tool call with this ID. The subsequent
		// request contains Codex's function_call_output for the stub result.
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(kind string, p map[string]any) {
			p["type"] = kind
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw(p))
		}
		emit("response.created", map[string]any{"response": map[string]any{"id": "resp_" + marker, "status": "in_progress"}})
		var item map[string]any
		if !complete {
			item = map[string]any{"id": "fc_" + marker, "type": "function_call", "name": tool, "call_id": callID, "arguments": "{}", "status": "completed"}
			if codeMode {
				item = map[string]any{"id": "ctc_" + marker, "type": "custom_tool_call", "namespace": "functions", "name": "exec", "call_id": callID, "input": "text(await tools.mcp__stub__ping({}));", "status": "completed"}
			}
		} else {
			item = map[string]any{"id": "msg_" + marker, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "pong", "annotations": []any{}}}}
		}
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
		emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		emit("response.completed", map[string]any{"response": map[string]any{"id": "resp_" + marker, "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110}}})
	}))
	defer model.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`model_provider="stub"
[model_providers.stub]
name="stub"
base_url=%s
wire_api="responses"
requires_openai_auth=false
[features]
apps=false
enable_request_compression=false
[mcp_servers.stub]
command=%s
args=["-test.run=^TestMCPStubProcess$"]
required=true
[mcp_servers.stub.env]
TKMAX_MCP_STUB="1"
TKMAX_MCP_CALLS=%s
`, strconv.Quote(model.URL), strconv.Quote(exe), strconv.Quote(calls))
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"app-server", "--stdio", "-c", `model_provider="stub"`, "-c", `model_providers.stub.name="stub"`, "-c", "model_providers.stub.base_url=" + strconv.Quote(model.URL), "-c", `model_providers.stub.wire_api="responses"`, "-c", `model_providers.stub.requires_openai_auth=false`, "-c", `features.apps=false`, "-c", `features.enable_request_compression=false`, "-c", "mcp_servers.stub.command=" + strconv.Quote(exe), "-c", `mcp_servers.stub.args=["-test.run=^TestMCPStubProcess$"]`}
	args = append(args, "-c", `mcp_servers.stub.required=true`, "-c", `mcp_servers.stub.env.TKMAX_MCP_STUB="1"`, "-c", "mcp_servers.stub.env.TKMAX_MCP_CALLS="+strconv.Quote(calls))
	newCmd := func() *exec.Cmd {
		cmd := exec.Command(bin, args...)
		cmd.Dir = home
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "CODEX_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "CODEX_HOME="+home)
		log, err := os.OpenFile(filepath.Join(home, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { log.Close() })
		cmd.Stderr = log
		return cmd
	}
	initialize := map[string]any{"clientInfo": map[string]any{"name": "tkmax_smoke", "version": "0.2"}, "capabilities": map[string]any{"experimentalApi": true}}
	first, err := startRPC(newCmd())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if first != nil {
			first.Close()
		}
	}()
	if err := first.Call(ctx, "initialize", initialize, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = first.input.Write(append(raw(map[string]any{"method": "initialized"}), '\n'))
	var started struct {
		Thread threadInfo `json:"thread"`
	}
	if err := first.Call(ctx, "thread/start", map[string]any{"cwd": home, "model": "gpt-6.1-sol", "modelProvider": "stub", "approvalPolicy": "never", "sandbox": "read-only"}, &started); err != nil {
		t.Fatal(err)
	}
	thread := started.Thread.ID
	if err := first.Call(ctx, "turn/start", map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": "Call ping once. first-marker"}}}, nil); err != nil {
		t.Fatal(err)
	}
	waitTurn := func(call func(string, any, any) error, target int) {
		t.Helper()
		for {
			select {
			case err := <-modelErrors:
				t.Fatal(err)
			case <-ctx.Done():
				b, _ := os.ReadFile(filepath.Join(home, "server.log"))
				t.Fatalf("timed out waiting for stub tool: %s", b)
			default:
			}
			b, _ := os.ReadFile(calls)
			var state struct {
				Thread struct {
					Status struct {
						Type string `json:"type"`
					} `json:"status"`
				} `json:"thread"`
			}
			if err := call("thread/read", map[string]any{"threadId": thread}, &state); err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(b), "pong\n") >= target && state.Thread.Status.Type == "idle" {
				var turns struct {
					Data []shutdownTurn `json:"data"`
				}
				if err := call("thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &turns); err != nil {
					t.Fatal(err)
				}
				if len(turns.Data) == 0 || turns.Data[0].Status != "completed" {
					t.Fatalf("stub tool ran but the native turn failed: %+v", turns.Data)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitTurn(func(method string, p, result any) error { return first.Call(ctx, method, p, result) }, 1)
	first.Close()
	first = nil
	// Reopen the exact stored thread through the WebSocket/JSONL bridge.
	daemon := exec.CommandContext(ctx, bin, "app-server", "daemon", "start")
	daemon.Env = newCmd().Env
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(stopCtx, bin, "app-server", "daemon", "stop")
		cmd.Env = daemon.Env
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("stop isolated daemon: %s: %v", b, err)
		}
	})
	if b, err := daemon.CombinedOutput(); err != nil {
		t.Fatalf("start isolated daemon: %s: %v", b, err)
	}
	cmd := exec.Command(bin, "app-server", "proxy")
	cmd.Env = daemon.Env
	cmd.Dir = home
	proxy, err := startProxy(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	in, out := proxy.Input, proxy.Output
	bridgeCtx, stop := context.WithCancel(ctx)
	defer stop()
	store := Store{Dir: filepath.Join(home, "runs")}
	record, err := NewRecord(home, bin, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	record.ThreadID, record.CodexHome, record.ServerMode = thread, home, "shared"
	done := make(chan error, 1)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			done <- err
			return
		}
		defer ws.CloseNow()
		done <- Bridge(bridgeCtx, ws, in, out, record, store.Save, BridgeOptions{ResumeConfig: map[string]any{"model": "gpt-6.1-sol", "sandbox": "read-only", "approvalPolicy": "never", "config": map[string]any{"features.apps": false}}})
	}))
	defer bridge.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(bridge.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop()
		ws.CloseNow()
		in.Close()
		out.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("bridge failed to stop")
		}
	}()
	id := 0
	call := func(method string, p, result any) error {
		id++
		if err := ws.Write(ctx, websocket.MessageText, raw(map[string]any{"id": id, "method": method, "params": p})); err != nil {
			return err
		}
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return err
			}
			var m message
			if json.Unmarshal(b, &m) != nil {
				return fmt.Errorf("invalid bridge reply")
			}
			if string(m["id"]) != string(raw(id)) {
				continue
			}
			if b, ok := m["error"]; ok {
				return fmt.Errorf("%s: %s", method, b)
			}
			if result != nil {
				return json.Unmarshal(m["result"], result)
			}
			return nil
		}
	}
	if err := call("initialize", initialize, nil); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, raw(map[string]any{"method": "initialized"})); err != nil {
		t.Fatal(err)
	}
	if err := call("thread/resume", map[string]any{"threadId": thread}, nil); err != nil {
		t.Fatal(err)
	}
	for {
		var state any
		if err := call("thread/read", map[string]any{"threadId": thread}, &state); err != nil {
			t.Fatal(err)
		}
		saved, err := store.Load(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State == "needs-attention" {
			t.Fatal("resumed tool preflight failed:", saved.Message)
		}
		if saved.CapabilitiesKnown && !saved.ToolsCheckedAt.IsZero() {
			found := false
			for _, required := range saved.ToolRequirements {
				if required.Kind == "mcp" && required.Server == "stub" {
					found = true
				}
			}
			if !found {
				t.Fatal("resumed tool dependency was not recorded")
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := call("turn/start", map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": "Call ping once. second-marker"}}}, nil); err != nil {
		t.Fatal(err)
	}
	waitTurn(call, 2)
	var active struct {
		Goal *Goal `json:"goal"`
	}
	if err := call("thread/goal/set", map[string]any{"threadId": thread, "objective": "Exercise managed shutdown", "status": "active"}, &active); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Goal = active.Goal
	if err := PauseManagedGoal(ctx, snapshot); err != nil {
		t.Fatal("pause on the real shared daemon:", err)
	}
	var paused struct {
		Goal *Goal `json:"goal"`
	}
	if err := call("thread/goal/get", map[string]any{"threadId": thread}, &paused); err != nil {
		t.Fatal(err)
	}
	if paused.Goal == nil || paused.Goal.Status != "paused" {
		t.Fatal("managed shutdown did not pause the native goal")
	}
	t.Log("The same persisted thread passed tool preflight and executed the local MCP tool through tkmax's bridge and the shared-daemon proxy after restart")
}
