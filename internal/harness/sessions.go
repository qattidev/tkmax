package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var sessionUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func IsSessionID(id string) bool { return sessionUUID.MatchString(id) }

// CanonicalCodexHome freezes the default as well as an explicit CODEX_HOME.
func CanonicalCodexHome(home string) (string, error) {
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".codex")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if os.IsNotExist(err) {
		return filepath.Clean(abs), nil
	}
	return resolved, err
}

// rpcClient is a short-lived, sequential control connection. Notifications are
// consumed, never replayed into a different client's TUI.
type rpcClient struct {
	input    io.WriteCloser
	output   io.ReadCloser
	child    *child
	packets  chan packet
	done     chan struct{}
	sequence int
}

func startRPC(cmd *exec.Cmd) (*rpcClient, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	c, err := startChild(cmd, true)
	if err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	r := &rpcClient{input: in, output: out, child: c, packets: make(chan packet, 16), done: make(chan struct{})}
	go func() {
		reader := bufio.NewReader(out)
		for {
			b, err := readJSONLine(reader)
			select {
			case r.packets <- packet{data: b, err: err}:
			case <-r.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return r, nil
}

func (r *rpcClient) Close() {
	close(r.done)
	r.input.Close()
	r.output.Close()
	r.child.stop()
}

func (r *rpcClient) Call(ctx context.Context, method string, params, result any) error {
	r.sequence++
	id := r.sequence
	if _, err := r.input.Write(append(raw(map[string]any{"id": id, "method": method, "params": params}), '\n')); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", method, ctx.Err())
		case p := <-r.packets:
			if p.err != nil {
				return fmt.Errorf("%s: Codex control connection ended: %w", method, p.err)
			}
			m, err := decode(p.data)
			if err != nil {
				return err
			}
			if string(m["id"]) != string(raw(id)) {
				continue
			}
			if b := m["error"]; len(b) != 0 && string(b) != "null" {
				var e rpcError
				if err := json.Unmarshal(b, &e); err != nil {
					return err
				}
				return fmt.Errorf("%s (RPC %d, Codex >=%s): %s", method, e.Code, CodexVersion, e.Message)
			}
			if result == nil {
				return nil
			}
			if err := json.Unmarshal(m["result"], result); err != nil {
				return fmt.Errorf("%s: invalid response: %w", method, err)
			}
			return nil
		}
	}
}

func ensureDaemon(ctx context.Context, bin, home string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server", "daemon", "start")
	cmd.Env = childEnvironment(home)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start Codex shared server (CODEX_HOME=%s): %w; run codex doctor to diagnose it", home, err)
	}
	return nil
}

func openShared(ctx context.Context, bin, home string) (*rpcClient, error) {
	if err := CheckCodex(ctx, bin); err != nil {
		return nil, err
	}
	if err := ensureDaemon(ctx, bin, home); err != nil {
		return nil, err
	}
	return connectShared(ctx, bin, home)
}

// Connecting during shutdown must never start or restart the daemon.
func connectShared(ctx context.Context, bin, home string) (*rpcClient, error) {
	cmd := exec.Command(bin, "app-server", "proxy")
	cmd.Env = childEnvironment(home)
	r, err := startRPC(cmd)
	if err != nil {
		return nil, err
	}
	var initialized struct {
		CodexHome string `json:"codexHome"`
	}
	err = r.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "tkmax", "version": "0.2"}, "capabilities": map[string]any{"experimentalApi": true}}, &initialized)
	if err == nil {
		var actual string
		actual, err = CanonicalCodexHome(initialized.CodexHome)
		if err == nil && (initialized.CodexHome == "" || actual != home) {
			err = fmt.Errorf("Codex server uses a different CODEX_HOME: %q (expected %q)", initialized.CodexHome, home)
		}
	}
	if err == nil {
		_, err = r.input.Write(append(raw(map[string]any{"method": "initialized"}), '\n'))
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

type Session struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	CWD                  string `json:"cwd"`
	ParentThreadID       string `json:"parentThreadId"`
	Ephemeral            bool   `json:"ephemeral"`
	CanAcceptDirectInput *bool  `json:"canAcceptDirectInput"`
	Status               struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
	} `json:"status"`
}

func (s Session) adoptable() error {
	if s.ID == "" {
		return errors.New("Codex returned no session UUID")
	}
	if s.Ephemeral || s.ParentThreadID != "" {
		return errors.New("only persistent root Codex sessions can be adopted")
	}
	if s.CanAcceptDirectInput != nil && !*s.CanAcceptDirectInput {
		return errors.New("session is controlled by its original client and cannot accept direct Codex TUI input")
	}
	if s.Status.Type != "notLoaded" && s.Status.Type != "idle" && s.Status.Type != "systemError" {
		return errors.New("session is active or waiting for approval/input; stop it and close its original CLI/IDE chat before resuming with tkmax")
	}
	if len(s.Status.ActiveFlags) != 0 {
		return errors.New("session has pending approval/input; resolve it in the original client first")
	}
	if !filepath.IsAbs(s.CWD) {
		return errors.New("session has no absolute working directory")
	}
	info, err := os.Stat(s.CWD)
	if err != nil {
		return fmt.Errorf("session directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("session working directory is not a directory")
	}
	return nil
}

func Sessions(ctx context.Context, bin, home, cwd string) ([]Session, error) {
	r, err := openShared(ctx, bin, home)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var sessions []Session
	cursor := ""
	seen := map[string]bool{}
	for {
		params := map[string]any{"limit": 100, "sortKey": "updated_at", "sourceKinds": []string{"cli", "vscode", "appServer"}}
		if cwd != "" {
			params["cwd"] = cwd
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []Session `json:"data"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := r.Call(ctx, "thread/list", params, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Data {
			if !s.Ephemeral && s.ParentThreadID == "" {
				sessions = append(sessions, s)
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			return sessions, nil
		}
		if seen[cursor] {
			return nil, errors.New("thread/list returned a repeated cursor")
		}
		seen[cursor] = true
	}
}

func Adopt(ctx context.Context, store Store, bin, home, id string, fallback time.Duration) (*Record, error) {
	if !IsSessionID(id) {
		return nil, fmt.Errorf("invalid Codex session UUID %q", id)
	}
	id = strings.ToLower(id)
	if r, err := store.FindThread(home, id); r != nil || err != nil {
		return r, err
	}
	lock, err := store.LockThread(home, id)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	// A concurrent adopter may have created the record before this lock.
	if r, err := store.FindThread(home, id); r != nil || err != nil {
		return r, err
	}
	rpc, err := openShared(ctx, bin, home)
	if err != nil {
		return nil, err
	}
	defer rpc.Close()
	var result struct {
		Thread Session `json:"thread"`
	}
	if err := rpc.Call(ctx, "thread/read", map[string]any{"threadId": id}, &result); err != nil {
		return nil, err
	}
	if result.Thread.ID != id {
		return nil, errors.New("Codex returned a different session UUID")
	}
	if err := result.Thread.adoptable(); err != nil {
		return nil, err
	}
	cwd, err := filepath.EvalSymlinks(result.Thread.CWD)
	if err != nil {
		return nil, err
	}
	r, err := NewRecord(cwd, bin, nil, fallback)
	if err != nil {
		return nil, err
	}
	r.ThreadID, r.SessionName, r.CodexHome, r.ServerMode = id, result.Thread.Name, home, "shared"
	if err := store.Save(r); err != nil {
		return nil, err
	}
	return r, nil
}
