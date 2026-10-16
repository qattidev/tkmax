package harness

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ToolRequirement struct {
	Kind   string `json:"kind"`
	Server string `json:"server,omitempty"`
	Tool   string `json:"tool,omitempty"`
	AppID  string `json:"appId,omitempty"`
}

type toolItem struct {
	Type       string `json:"type"`
	Status     string `json:"status"`
	Success    *bool  `json:"success"`
	Server     string `json:"server"`
	Tool       string `json:"tool"`
	AppContext *struct {
		ConnectorID string `json:"connectorId"`
	} `json:"appContext"`
}

func (e *Engine) requireTool(t ToolRequirement) {
	for _, old := range e.Record.ToolRequirements {
		if old == t {
			return
		}
	}
	e.Record.ToolRequirements = append(e.Record.ToolRequirements, t)
	e.toolsReady = false
	e.save()
}

func (e *Engine) observeTool(b json.RawMessage) {
	var item toolItem
	if json.Unmarshal(b, &item) != nil || item.Status != "completed" {
		return
	}
	switch item.Type {
	case "mcpToolCall":
		e.observeMCPDependency(item)
	case "dynamicToolCall":
		if item.Success != nil && *item.Success {
			e.requireTool(ToolRequirement{Kind: "dynamic", Tool: item.Tool})
		}
	}
}

func (e *Engine) observeMCPDependency(item toolItem) {
	if item.AppContext != nil && item.AppContext.ConnectorID != "" {
		e.requireTool(ToolRequirement{Kind: "app", AppID: item.AppContext.ConnectorID})
	} else if item.Server != "" && item.Tool != "" {
		e.requireTool(ToolRequirement{Kind: "mcp", Server: item.Server, Tool: item.Tool})
	}
}

func (e *Engine) toolsFailure(method string, rpc *rpcError, detail string) {
	e.invalidate()
	e.toolsChecking, e.toolsReady = false, false
	e.Record.ManualHold = true
	e.Record.State = "needs-attention"
	if rpc != nil {
		detail = fmt.Sprintf("%s (RPC %d): %s", method, rpc.Code, rpc.Message)
	}
	e.warning(detail + "; automatic recovery disabled. Restore the service/login in Codex, then use /goal resume to recheck tools.")
}

// Tool checks run as callbacks on the same actor as user input and quota
// recovery. The selection guard prevents history/catalog replies crossing chats.
func (e *Engine) checkTools() {
	if e.toolsChecking || !e.ready || e.switching || e.Record.ThreadID == "" {
		return
	}
	e.toolsChecking, e.toolsReady = true, false
	e.invalidate()
	thread, selection := e.Record.ThreadID, e.selection
	generation := e.toolsGeneration
	valid := func() bool {
		return generation == e.toolsGeneration && thread == e.Record.ThreadID && selection == e.selection
	}
	catalog := func() {
		if !valid() {
			return
		}
		for _, requirement := range e.Record.ToolRequirements {
			if requirement.Kind == "dynamic" {
				e.toolsFailure("", nil, "Tool "+requirement.Tool+" needs its original client's tool handler; this session cannot recover unattended in the Codex TUI")
				return
			}
		}
		e.query("app/installed", map[string]any{"threadId": thread, "forceRefresh": true}, func(b json.RawMessage, rpc *rpcError) {
			if !valid() {
				return
			}
			if rpc != nil {
				e.toolsFailure("app/installed", rpc, "")
				return
			}
			var apps struct {
				Apps *[]struct {
					ID       string `json:"id"`
					Enabled  bool   `json:"enabled"`
					Callable bool   `json:"callable"`
				} `json:"apps"`
			}
			if json.Unmarshal(b, &apps) != nil || apps.Apps == nil {
				e.toolsFailure("", nil, "Invalid app/installed response")
				return
			}
			callable := map[string]bool{}
			for _, app := range *apps.Apps {
				callable[app.ID] = app.Enabled && app.Callable
			}
			for _, requirement := range e.Record.ToolRequirements {
				if requirement.Kind == "app" && !callable[requirement.AppID] {
					e.toolsFailure("", nil, "Required app "+requirement.AppID+" is unavailable or disabled in the resumed thread")
					return
				}
			}
			e.checkMCP(thread, valid)
		})
	}
	inventory := func() {
		e.query("config/read", map[string]any{"cwd": e.Record.CWD, "includeLayers": false}, func(b json.RawMessage, rpc *rpcError) {
			if !valid() {
				return
			}
			if rpc != nil {
				e.toolsFailure("config/read", rpc, "")
				return
			}
			var result struct {
				Config json.RawMessage `json:"config"`
			}
			if json.Unmarshal(b, &result) != nil || len(result.Config) == 0 || string(result.Config) == "null" {
				e.toolsFailure("", nil, "Invalid effective Codex configuration response")
				return
			}
			e.observeRequiredMCP(result.Config)
			if config, ok := e.ResumeConfig["config"].(map[string]any); ok {
				// A thread-local required server override may be absent from
				// config/read, which reports the configuration on disk.
				for key, value := range config {
					if strings.HasPrefix(key, "mcp_servers.") && strings.HasSuffix(key, ".required") && value == true {
						e.requireTool(ToolRequirement{Kind: "mcp", Server: strings.TrimSuffix(strings.TrimPrefix(key, "mcp_servers."), ".required")})
					}
				}
			}
			catalog()
		})
	}
	if e.Record.CapabilitiesKnown {
		inventory()
		return
	}
	if e.historyMode != "paginated" {
		e.query("thread/read", map[string]any{"threadId": thread, "includeTurns": true}, func(b json.RawMessage, rpc *rpcError) {
			if !valid() {
				return
			}
			if rpc != nil {
				e.toolsFailure("thread/read", rpc, "")
				return
			}
			var history struct {
				Thread *struct {
					ID    string `json:"id"`
					Turns []struct {
						Items []json.RawMessage `json:"items"`
					} `json:"turns"`
				} `json:"thread"`
			}
			if json.Unmarshal(b, &history) != nil || history.Thread == nil || history.Thread.ID != thread {
				e.toolsFailure("", nil, "Invalid thread history response")
				return
			}
			for _, turn := range history.Thread.Turns {
				for _, item := range turn.Items {
					e.observeTool(item)
				}
			}
			e.Record.CapabilitiesKnown = true
			e.save()
			inventory()
		})
		return
	}
	seen := map[string]bool{}
	var page func(string)
	page = func(cursor string) {
		params := map[string]any{"threadId": thread, "limit": 100, "sortDirection": "desc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		e.query("thread/items/list", params, func(b json.RawMessage, rpc *rpcError) {
			if !valid() {
				return
			}
			if rpc != nil {
				e.toolsFailure("thread/items/list", rpc, "")
				return
			}
			var history struct {
				Data *[]struct {
					Item json.RawMessage `json:"item"`
				} `json:"data"`
				NextCursor string `json:"nextCursor"`
			}
			if json.Unmarshal(b, &history) != nil || history.Data == nil {
				e.toolsFailure("", nil, "Invalid paginated tool history response")
				return
			}
			for _, item := range *history.Data {
				e.observeTool(item.Item)
			}
			if history.NextCursor != "" {
				if seen[history.NextCursor] {
					e.toolsFailure("", nil, "Repeated thread history cursor")
					return
				}
				seen[history.NextCursor] = true
				page(history.NextCursor)
				return
			}
			e.Record.CapabilitiesKnown = true
			e.save()
			inventory()
		})
	}
	page("")
}

func (e *Engine) checkMCP(thread string, valid func() bool) {
	servers := map[string]mcpStatus{}
	seen := map[string]bool{}
	var page func(string)
	page = func(cursor string) {
		p := map[string]any{"threadId": thread, "detail": "toolsAndAuthOnly", "limit": 100}
		if cursor != "" {
			p["cursor"] = cursor
		}
		e.query("mcpServerStatus/list", p, func(b json.RawMessage, rpc *rpcError) {
			if !valid() {
				return
			}
			if rpc != nil {
				e.toolsFailure("mcpServerStatus/list", rpc, "")
				return
			}
			var result struct {
				Data       *[]mcpStatus `json:"data"`
				NextCursor string       `json:"nextCursor"`
			}
			if json.Unmarshal(b, &result) != nil || result.Data == nil {
				e.toolsFailure("", nil, "Invalid MCP inventory response")
				return
			}
			for _, s := range *result.Data {
				servers[s.Name] = s
			}
			if result.NextCursor != "" {
				if seen[result.NextCursor] {
					e.toolsFailure("", nil, "Repeated MCP inventory cursor")
					return
				}
				seen[result.NextCursor] = true
				page(result.NextCursor)
				return
			}
			for _, requirement := range e.Record.ToolRequirements {
				if requirement.Kind != "mcp" {
					continue
				}
				s, exists := servers[requirement.Server]
				if exists && (s.RuntimeStatus == "starting" || s.RuntimeStatus == "notStarted") {
					if e.toolsDeadline.IsZero() {
						e.toolsDeadline = e.now.Add(45 * time.Second)
					}
					if e.now.Before(e.toolsDeadline) {
						e.toolsChecking = false
						e.toolsRetryAt = e.now.Add(2 * time.Second)
						return
					}
				}
				_, hasTool := s.Tools[requirement.Tool]
				if !exists || s.AuthStatus == "notLoggedIn" || s.ToolsError != nil || (s.RuntimeStatus != "" && s.RuntimeStatus != "connected") || (requirement.Tool != "" && !hasTool) {
					detail := "Required MCP service " + requirement.Server + " or tool " + requirement.Tool + " is unavailable"
					if s.AuthStatus == "notLoggedIn" || s.RuntimeStatus == "authenticationRequired" {
						detail += "; sign in to that service in Codex"
					}
					e.toolsFailure("", nil, detail)
					return
				}
			}
			e.toolsChecking, e.toolsReady = false, true
			e.toolsDeadline = time.Time{}
			e.toolsCheckAt = e.now.Add(30 * time.Second)
			e.Record.ToolsCheckedAt = e.now.UTC()
			e.save()
			// Starting a tool check invalidates any in-flight recovery or goal
			// read. Always reconcile native goal state afterward, including
			// limits reached while the wrapper was disconnected.
			e.refreshGoal()
		})
	}
	page("")
}

type mcpStatus struct {
	Name          string                     `json:"name"`
	AuthStatus    string                     `json:"authStatus"`
	RuntimeStatus string                     `json:"runtimeStatus"`
	Tools         map[string]json.RawMessage `json:"tools"`
	ToolsError    *string                    `json:"toolsError"`
}

func (e *Engine) capabilitiesChanged() {
	e.toolsGeneration++
	e.toolsChecking = false
	e.toolsReady = false
	e.toolsRetryAt = e.now.Add(2 * time.Second)
	e.invalidate()
}

func (e *Engine) observeRequiredMCP(config json.RawMessage) {
	var c struct {
		MCP map[string]struct {
			Required bool  `json:"required"`
			Enabled  *bool `json:"enabled"`
		} `json:"mcp_servers"`
	}
	if json.Unmarshal(config, &c) != nil {
		return
	}
	for name, settings := range c.MCP {
		if settings.Required && (settings.Enabled == nil || *settings.Enabled) {
			e.requireTool(ToolRequirement{Kind: "mcp", Server: strings.TrimSpace(name)})
		}
	}
}
