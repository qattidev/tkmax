package harness

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

func verifyResumeOverrides(b json.RawMessage, expected map[string]any) error {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(b, &result); err != nil {
		return err
	}
	for _, key := range []string{"model", "approvalPolicy", "approvalsReviewer"} {
		if value, ok := expected[key]; ok && string(result[key]) != string(raw(value)) {
			return fmt.Errorf("Codex did not preserve saved %s on resume", key)
		}
	}
	if value, ok := expected["sandbox"].(string); ok {
		var policy struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(result["sandbox"], &policy)
		want := map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}[value]
		if want == "" || policy.Type != want {
			return fmt.Errorf("Codex did not preserve the saved sandbox on resume")
		}
	}
	return nil
}

// ResumeOverrides translates saved CLI arguments into thread-local RPC inputs.
// Never send these arguments to daemon start/proxy: that cannot configure an
// already running server and could change other sessions on a later start.
func ResumeOverrides(args []string) (map[string]any, error) {
	p := map[string]any{}
	config := map[string]any{}
	for i := 0; i < len(args); i++ {
		name := args[i]
		switch name {
		case "--no-alt-screen":
		case "--strict-config", "--dangerously-bypass-hook-trust", "--add-dir":
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
	if len(config) != 0 {
		p["config"] = config
	}
	return p, nil
}
