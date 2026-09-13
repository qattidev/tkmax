package harness

import (
	"fmt"
	"path/filepath"
	"strings"
)

// CodexOptions is intentionally an allowlist. A CLI upgrade must not silently
// redirect the wrapper to a shared/remote server or start a different command.
func CodexOptions(args []string, cwd string) (server, tui []string, root string, err error) {
	root = cwd
	values := map[string]bool{"-c": true, "--config": true, "--enable": true, "--disable": true, "-m": true, "--model": true, "-s": true, "--sandbox": true, "-a": true, "--ask-for-approval": true, "-C": true, "--cd": true, "--add-dir": true}
	flags := map[string]bool{"--search": true, "--no-alt-screen": true, "--strict-config": true, "--approve-for-me": true, "--dangerously-bypass-approvals-and-sandbox": true, "--dangerously-bypass-hook-trust": true}
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if values[name] {
			if !inline {
				i++
				if i >= len(args) {
					return nil, nil, "", fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			if value == "" {
				return nil, nil, "", fmt.Errorf("%s requires a nonempty value", name)
			}
			if name == "--cd" || name == "-C" {
				if filepath.IsAbs(value) {
					root = value
				} else {
					root = filepath.Join(cwd, value)
				}
				continue // Both child processes inherit this resolved working root.
			}
			if name == "--add-dir" && !filepath.IsAbs(value) {
				value = filepath.Join(cwd, value)
			}
			tui = append(tui, name, value)
			if name == "-c" || name == "--config" || name == "--enable" || name == "--disable" {
				server = append(server, name, value)
			}
			continue
		}
		if flags[name] && !inline {
			tui = append(tui, name)
			if name == "--strict-config" {
				server = append(server, name)
			}
			if name == "--search" {
				server = append(server, "-c", `web_search="live"`)
			}
			continue
		}
		return nil, nil, "", fmt.Errorf("unsupported Codex option %q; use tkmax --help for supported options (transport, profiles, images, worktrees and subcommands are managed separately or unsupported)", args[i])
	}
	root, err = filepath.EvalSymlinks(root)
	return
}
