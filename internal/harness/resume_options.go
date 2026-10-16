package harness

import (
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ResumeOverrides translates saved CLI arguments into thread-local RPC inputs.
// Never send these arguments to daemon start/proxy: that cannot configure an
// already running server and could change other sessions on a later start.
func ResumeOverrides(args []string) (map[string]any, error) {
	p := map[string]any{}
	config := map[string]any{}
	var dirs []string
	for i := 0; i < len(args); i++ {
		name := args[i]
		switch name {
		case "--no-alt-screen":
		case "--strict-config", "--dangerously-bypass-hook-trust":
			return nil, fmt.Errorf("saved option %s cannot be applied to a shared Codex server; resume directly in Codex or start a new tkmax run without it", name)
		case "--search":
			config["web_search"] = "live"
		case "--approve-for-me":
			p["sandbox"], p["approvalPolicy"], p["approvalsReviewer"] = "workspace-write", "on-request", "auto_review"
		case "--dangerously-bypass-approvals-and-sandbox":
			p["sandbox"], p["approvalPolicy"] = "danger-full-access", "never"
		default:
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("saved option %s requires a value", name)
			}
			value := args[i]
			switch name {
			case "--model", "-m":
				p["model"] = value
			case "--sandbox", "-s":
				p["sandbox"] = value
			case "--ask-for-approval", "-a":
				p["approvalPolicy"] = value
			case "--add-dir":
				dirs = append(dirs, value)
			case "--enable", "--disable":
				config["features."+value] = name == "--enable"
			case "-c", "--config":
				key, text, ok := strings.Cut(value, "=")
				if !ok || strings.TrimSpace(key) == "" {
					return nil, fmt.Errorf("invalid saved Codex config override %q", value)
				}
				var parsed map[string]any
				var v any = text
				if err := toml.Unmarshal([]byte("value = "+text), &parsed); err == nil {
					v = parsed["value"]
				}
				config[strings.TrimSpace(key)] = v
			default:
				return nil, fmt.Errorf("cannot preserve saved option %s on shared-server resume", name)
			}
		}
	}
	if len(dirs) != 0 {
		if _, exists := config["sandbox_workspace_write.writable_roots"]; exists {
			return nil, fmt.Errorf("cannot safely combine --add-dir with a writable_roots config override on resume")
		}
		config["sandbox_workspace_write.writable_roots"] = dirs
	}
	if len(config) != 0 {
		p["config"] = config
	}
	return p, nil
}
