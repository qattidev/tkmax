package harness

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSessionID = "01a12a6e-b9d8-7270-bb1b-4b4c86fa42a9"

// Runs only in an isolated subprocess selected by fakeCodex. No sockets,
// authentication, model requests, or real daemon state are involved.
func TestCodexRPCProcess(t *testing.T) {
	if os.Getenv("TKMAX_RPC_HELPER") != "1" {
		return
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal([]byte(os.Getenv("TKMAX_RPC_FIXTURES")), &fixtures); err != nil {
		os.Exit(2)
	}
	listener := &stdioListener{conn: &stdioConn{Reader: os.Stdin, Writer: os.Stdout}, done: make(chan struct{})}
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer listener.Close()
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		counts := map[string]int{}
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m message
			if json.Unmarshal(b, &m) != nil {
				os.Exit(3)
			}
			method := str(m["method"])
			counts[method]++
			if path := os.Getenv("TKMAX_RPC_CALLS"); path != "" {
				if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
					_, _ = f.Write(append(raw(m), '\n'))
					f.Close()
				}
			}
			if _, ok := m["id"]; !ok {
				continue
			}
			result, ok := fixtures[method]
			if numbered, exists := fixtures[method+"#"+strconv.Itoa(counts[method])]; exists {
				result, ok = numbered, true
			}
			if !ok {
				_ = ws.Write(context.Background(), websocket.MessageText, raw(map[string]any{"id": m["id"], "error": map[string]any{"code": -32601, "message": "unsupported method"}}))
				continue
			}
			_ = ws.Write(context.Background(), websocket.MessageText, raw(map[string]any{"id": m["id"], "result": result}))
		}
	}))
	os.Exit(0)
}

type stdioListener struct {
	conn net.Conn
	used bool
	done chan struct{}
	once sync.Once
}

func (l *stdioListener) Accept() (net.Conn, error) {
	if !l.used {
		l.used = true
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *stdioListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *stdioListener) Addr() net.Addr { return stdioAddr("fixture") }

func fakeCodex(t *testing.T, home string, fixtures map[string]any) string {
	t.Helper()
	fixtures["initialize"] = map[string]any{"codexHome": home}
	t.Setenv("TKMAX_RPC_HELPER", "1")
	t.Setenv("TKMAX_RPC_FIXTURES", string(raw(fixtures)))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TKMAX_HELPER_BIN", exe)
	path := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
case "$*" in
  --version) echo 'codex-cli 0.162.1';;
  'app-server daemon start') exit 0;;
  'app-server proxy') exec "$TKMAX_HELPER_BIN" -test.run='^TestCodexRPCProcess$';;
  *) exit 10;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAdoptNativeSessionAndReuseRecord(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	store := Store{Dir: t.TempDir()}
	bin := fakeCodex(t, home, map[string]any{"thread/read": map[string]any{"thread": map[string]any{"id": testSessionID, "name": "Fix tools", "cwd": cwd, "status": map[string]any{"type": "notLoaded"}}}})
	r, err := Adopt(context.Background(), store, bin, home, testSessionID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r.CWD != cwd || r.ThreadID != testSessionID || r.SessionName != "Fix tools" || r.ServerMode != "shared" {
		t.Fatalf("wrong record: %+v", r)
	}
	r.NextCheck = time.Now().Add(3 * time.Hour)
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	again, err := Adopt(context.Background(), store, "/does/not/exist", home, testSessionID, time.Minute)
	if err != nil || again.ID != r.ID || !again.NextCheck.Equal(r.NextCheck) {
		t.Fatalf("adoption lost saved configuration/deadline: %+v, %v", again, err)
	}
	if records, _ := store.List(); len(records) != 1 {
		t.Fatal("duplicate run created")
	}
}

func TestAdoptionRejectsUnsafeOrMissingSessions(t *testing.T) {
	for _, kind := range []string{"active", "approval", "ephemeral", "child", "client-owned", "missing-directory", "different-id", "missing"} {
		t.Run(kind, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			thread := map[string]any{"id": testSessionID, "cwd": cwd, "status": map[string]any{"type": "idle"}}
			switch kind {
			case "active":
				thread["status"] = map[string]any{"type": "active"}
			case "approval":
				thread["status"] = map[string]any{"type": "idle", "activeFlags": []string{"waitingOnApproval"}}
			case "ephemeral":
				thread["ephemeral"] = true
			case "child":
				thread["parentThreadId"] = "parent"
			case "client-owned":
				thread["canAcceptDirectInput"] = false
			case "missing-directory":
				thread["cwd"] = filepath.Join(cwd, "missing")
			case "different-id":
				thread["id"] = "wrong"
			}
			fixtures := map[string]any{"thread/read": map[string]any{"thread": thread}}
			if kind == "missing" {
				delete(fixtures, "thread/read")
			}
			bin := fakeCodex(t, home, fixtures)
			store := Store{Dir: t.TempDir()}
			if _, err := Adopt(context.Background(), store, bin, home, testSessionID, time.Hour); err == nil {
				t.Fatal("unsafe adoption accepted")
			}
			if records, _ := store.List(); len(records) != 0 {
				t.Fatal("failed adoption left a run record")
			}
		})
	}
}

func TestNativeSessionListingAndPaginationGuard(t *testing.T) {
	home := t.TempDir()
	fixtures := map[string]any{"thread/list": map[string]any{"data": []map[string]any{{"id": testSessionID, "cwd": "/repo", "name": "Task"}, {"id": "child", "parentThreadId": testSessionID}, {"id": "ephemeral", "ephemeral": true}}}}
	bin := fakeCodex(t, home, fixtures)
	sessions, err := Sessions(context.Background(), bin, home, "/repo")
	if err != nil || len(sessions) != 1 || sessions[0].ID != testSessionID {
		t.Fatalf("sessions: %+v, %v", sessions, err)
	}
	fixtures["thread/list"] = map[string]any{"nextCursor": "repeated"}
	bin = fakeCodex(t, home, fixtures)
	if _, err := Sessions(context.Background(), bin, home, ""); err == nil || !strings.Contains(err.Error(), "repeated cursor") {
		t.Fatalf("pagination guard: %v", err)
	}
}

func TestNativeSessionLockCoversDifferentRuns(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	home := t.TempDir()
	lock, err := store.LockThread(home, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := store.LockThread(home, testSessionID); err == nil {
		other.Close()
		t.Fatal("duplicate session lock acquired")
	}
	other, err := store.LockThread(t.TempDir(), testSessionID)
	if err != nil {
		t.Fatal("unrelated Codex home blocked:", err)
	}
	other.Close()
}
