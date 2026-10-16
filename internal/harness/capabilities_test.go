package harness

import (
	"strings"
	"testing"
	"time"
)

func sharedRig(t *testing.T) *rig {
	r := newRig(t)
	r.e.Record.ServerMode = "shared"
	r.e.Record.CapabilitiesKnown = true
	return r
}

func toolInventory(r *rig, apps, mcp any) {
	r.t.Helper()
	r.reply(r.request("config/read"), map[string]any{"config": map[string]any{}})
	r.reply(r.request("app/installed"), map[string]any{"apps": apps})
	if !r.e.Record.ManualHold {
		r.reply(r.request("mcpServerStatus/list"), map[string]any{"data": mcp})
		if r.e.toolsReady {
			r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
		}
	}
}

func TestToolPreflightReconcilesGoalLimitedWhileDisconnected(t *testing.T) {
	r := sharedRig(t)
	r.e.Record.Goal.Status = "active"
	r.tick(0)
	r.reply(r.request("config/read"), map[string]any{"config": map[string]any{}})
	r.reply(r.request("app/installed"), map[string]any{"apps": []any{}})
	r.reply(r.request("mcpServerStatus/list"), map[string]any{"data": []any{}})
	limited := *r.e.Record.Goal
	limited.Status = "usageLimited"
	r.reply(r.request("thread/goal/get"), map[string]any{"goal": &limited})
	r.tick(0)
	r.preflight(Quota{Allowed: ptr(true)})
	r.request("thread/goal/set")
}

func TestSharedRecoveryRequiresUsableToolsAndRechecksAtDeadline(t *testing.T) {
	r := sharedRig(t)
	r.e.Record.ToolRequirements = []ToolRequirement{{Kind: "app", AppID: "github"}, {Kind: "mcp", Server: "local", Tool: "read"}}
	apps := []any{map[string]any{"id": "github", "enabled": true, "callable": true}}
	mcp := []any{map[string]any{"name": "local", "runtimeStatus": "connected", "authStatus": "unsupported", "tools": map[string]any{"read": map[string]any{}}}}
	r.tick(0)
	toolInventory(r, apps, mcp)
	if !r.e.toolsReady || r.e.Record.ManualHold {
		t.Fatal("usable tools blocked recovery")
	}
	r.tick(0)
	reset := r.now.Add(time.Hour).Unix()
	r.preflight(Quota{Allowed: ptr(false), Bucket: QuotaBucket{Primary: &Window{UsedPercent: 100, ResetsAt: &reset}}})
	r.tick(time.Hour + 31*time.Second)
	// A previously healthy connector was removed while tkmax waited.
	toolInventory(r, []any{}, mcp)
	if !r.e.Record.ManualHold || r.e.Record.State != "needs-attention" || !strings.Contains(r.e.Record.Message, "github") {
		t.Fatal("missing app did not stop automatic recovery")
	}
	if len(r.server) != 0 {
		t.Fatal("recovery continued after a missing tool")
	}
}

func TestMCPAuthenticationStartupAndRequiredServers(t *testing.T) {
	for _, state := range []string{"authenticationRequired", "failed", "disabled", "missing", "toolsError", "missing-tool"} {
		t.Run(state, func(t *testing.T) {
			r := sharedRig(t)
			r.e.Record.ToolRequirements = []ToolRequirement{{Kind: "mcp", Server: "service", Tool: "lookup"}}
			s := map[string]any{"name": "service", "runtimeStatus": state, "tools": map[string]any{"lookup": map[string]any{}}}
			if state == "toolsError" {
				s["runtimeStatus"], s["toolsError"] = "connected", "failed to discover tools"
			}
			if state == "missing-tool" {
				s["runtimeStatus"], s["tools"] = "connected", map[string]any{}
			}
			servers := []any{s}
			if state == "missing" {
				servers = []any{}
			}
			r.tick(0)
			toolInventory(r, []any{}, servers)
			if !r.e.Record.ManualHold || r.e.Record.State != "needs-attention" {
				t.Fatalf("unsafe MCP state accepted: %s", state)
			}
		})
	}
	r := sharedRig(t)
	r.tick(0)
	r.reply(r.request("config/read"), map[string]any{"config": map[string]any{"mcp_servers": map[string]any{"required-service": map[string]any{"required": true}}}})
	r.reply(r.request("app/installed"), map[string]any{"apps": []any{}})
	r.reply(r.request("mcpServerStatus/list"), map[string]any{"data": []any{}})
	if !r.e.Record.ManualHold || !strings.Contains(r.e.Record.Message, "required-service") {
		t.Fatal("required server ignored")
	}
}

func TestMCPStartupWaitIsBounded(t *testing.T) {
	r := sharedRig(t)
	r.e.Record.ToolRequirements = []ToolRequirement{{Kind: "mcp", Server: "service", Tool: "lookup"}}
	starting := []any{map[string]any{"name": "service", "runtimeStatus": "starting"}}
	r.tick(0)
	toolInventory(r, []any{}, starting)
	if r.e.Record.ManualHold || r.e.toolsReady {
		t.Fatal("startup was not allowed to settle")
	}
	r.tick(time.Second)
	if len(r.server) != 0 {
		t.Fatal("startup polled too soon")
	}
	r.tick(45 * time.Second)
	toolInventory(r, []any{}, starting)
	if !r.e.Record.ManualHold {
		t.Fatal("startup wait is unbounded")
	}
}

func TestAdoptedHistoryFindsAppAndClientOwnedTools(t *testing.T) {
	for _, kind := range []string{"app", "dynamic", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			r := sharedRig(t)
			r.e.Record.CapabilitiesKnown = false
			r.e.historyMode = "paginated"
			item := map[string]any{"type": "mcpToolCall", "status": "completed", "server": "codex_apps", "tool": "lookup", "appContext": map[string]any{"connectorId": "github"}}
			if kind == "dynamic" {
				item = map[string]any{"type": "dynamicToolCall", "status": "completed", "tool": "original-client-tool", "success": true}
			}
			if kind == "legacy" {
				r.e.historyMode = "legacy"
			}
			r.tick(0)
			if kind == "legacy" {
				r.reply(r.request("thread/read"), map[string]any{"thread": map[string]any{"id": "thread-1", "turns": []any{map[string]any{"items": []any{item}}}}})
			} else {
				r.reply(r.request("thread/items/list"), map[string]any{"data": []any{map[string]any{"item": item}}})
			}
			r.reply(r.request("config/read"), map[string]any{"config": map[string]any{}})
			if kind != "dynamic" {
				r.reply(r.request("app/installed"), map[string]any{"apps": []any{}})
			}
			if !r.e.Record.CapabilitiesKnown || len(r.e.Record.ToolRequirements) != 1 || !r.e.Record.ManualHold {
				t.Fatal("history dependency lost")
			}
		})
	}
}

func TestToolHistoryRepliesCannotCrossSelections(t *testing.T) {
	r := sharedRig(t)
	r.e.Record.CapabilitiesKnown = false
	r.e.historyMode = "paginated"
	r.tick(0)
	history := r.request("thread/items/list")
	// Selecting another conversation invalidates this pending history callback.
	r.user("thread/resume", map[string]any{"threadId": "other"})
	r.reply(history, map[string]any{"data": []any{map[string]any{"item": map[string]any{"type": "dynamicToolCall", "status": "completed", "tool": "stale", "success": true}}}})
	if len(r.e.Record.ToolRequirements) != 0 || r.e.Record.ManualHold {
		t.Fatal("stale history changed the selected session")
	}
}

func TestFailedSelectionCannotRecoverSavedGoal(t *testing.T) {
	r := sharedRig(t)
	r.e.toolsReady = true
	m := r.user("thread/resume", map[string]any{"threadId": "thread-1"})
	if err := r.e.Server(raw(map[string]any{"id": m["id"], "error": map[string]any{"code": -1, "message": "resume failed"}}), r.now); err != nil {
		t.Fatal(err)
	}
	r.tick(24 * time.Hour)
	if len(r.server) != 0 {
		t.Fatal("failed resume still scheduled recovery")
	}
}

func TestRecoverableMCPCallErrorRechecksServiceWithoutManualHold(t *testing.T) {
	r := sharedRig(t)
	r.notify("item/completed", map[string]any{"threadId": "thread-1", "item": map[string]any{"type": "mcpToolCall", "status": "failed", "server": "service", "tool": "lookup", "error": map[string]any{"message": "invalid arguments"}}})
	if r.e.Record.ManualHold {
		t.Fatal("an ordinary tool argument error requires human recovery")
	}
	r.tick(3 * time.Second)
	toolInventory(r, []any{}, []any{map[string]any{"name": "service", "runtimeStatus": "connected", "tools": map[string]any{"lookup": map[string]any{}}}})
	if !r.e.toolsReady || r.e.Record.ManualHold {
		t.Fatal("healthy service failed the recheck")
	}
	r.tick(0)
	r.preflight(Quota{Allowed: ptr(true)})
	r.request("thread/goal/set")
}

func TestMCPStartupChangeInvalidatesPendingInventory(t *testing.T) {
	r := sharedRig(t)
	r.tick(0)
	old := r.request("config/read")
	r.notify("mcpServer/startupStatus/updated", map[string]any{"threadId": "thread-1"})
	r.reply(old, map[string]any{"config": map[string]any{}})
	if len(r.server) != 0 || r.e.toolsReady {
		t.Fatal("stale inventory advanced after an MCP runtime change")
	}
	r.tick(3 * time.Second)
	toolInventory(r, []any{}, []any{})
	if !r.e.toolsReady {
		t.Fatal("fresh runtime inventory did not complete")
	}
}
