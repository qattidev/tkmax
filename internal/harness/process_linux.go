//go:build linux

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/coder/websocket"
)

const CodexVersion = "0.154.0"

func CheckCodex(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("check Codex executable: %w", err)
	}
	if strings.TrimSpace(string(out)) != "codex-cli "+CodexVersion {
		return fmt.Errorf("unsupported Codex version %q; this build supports codex-cli %s", strings.TrimSpace(string(out)), CodexVersion)
	}
	return nil
}

func terminalState() (*syscall.Termios, error) {
	var term syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&term)))
	if errno != 0 {
		return nil, errors.New("tkmax requires an interactive terminal on stdin")
	}
	var output syscall.Termios
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&output)))
	if errno != 0 {
		return nil, errors.New("tkmax requires an interactive terminal on stdout")
	}
	return &term, nil
}

func restoreTerminal(term *syscall.Termios) {
	syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(term)))
}

type child struct {
	cmd   *exec.Cmd
	done  chan struct{}
	err   error
	group bool
}

func startChild(cmd *exec.Cmd, group bool) (*child, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: group, Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{cmd: cmd, done: make(chan struct{}), group: group}
	go func() { c.err = cmd.Wait(); close(c.done) }()
	return c, nil
}

func (c *child) signal(sig syscall.Signal) {
	if c.group {
		_ = syscall.Kill(-c.cmd.Process.Pid, sig)
	} else {
		_ = c.cmd.Process.Signal(sig)
	}
}

func (c *child) stop() {
	// A server can exit with shell descendants still alive in its group.
	c.signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		c.signal(syscall.SIGKILL)
		<-c.done
	}
	if c.group {
		c.signal(syscall.SIGKILL)
	}
}

// Run owns only its private app server, socket, and TUI. The shared Codex
// daemon and other sessions are never stopped or selected by --last.
func Run(ctx context.Context, store Store, r *Record, resume bool, serverArgs, tuiArgs []string) (runErr error) {
	// Linux parent-death signals belong to the creating OS thread. Keep that
	// thread alive for the entire child lifetime, even while Go reschedules.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	term, err := terminalState()
	if err != nil {
		return err
	}
	defer restoreTerminal(term)
	if err := CheckCodex(ctx, r.CodexBin); err != nil {
		return err
	}
	lock, err := store.Lock(r.ID)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Reload under the lock so an earlier owner cannot race the resume read.
	if resume {
		fresh, err := store.Load(r.ID)
		if err != nil {
			return err
		}
		*r = *fresh
	}
	r.StoppedAt = time.Time{}
	if err := store.Save(r); err != nil {
		return err
	}
	defer func() {
		r.StoppedAt = time.Now().UTC()
		if err := store.Save(r); err != nil && runErr == nil {
			runErr = err
		}
		restoreTerminal(term)
		if runErr != nil {
			fmt.Fprint(os.Stderr, "\x1b[?1049l\x1b[?2004l\x1b[?1004l\x1b[<u\x1b[>4;0m\x1b[0m\x1b[?25h")
		}
		if r.ThreadID != "" {
			fmt.Fprintf(os.Stderr, "tkmax stopped its private Codex server. Resume: tkmax resume %s\n", r.ID)
		}
	}()
	dir, err := os.MkdirTemp("", "tkmax-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(store.Dir, r.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(r.CodexBin, append([]string{"app-server", "--stdio"}, serverArgs...)...)
	cmd.Dir = r.CWD
	cmd.Env = childEnvironment(r.CodexHome)
	cmd.Stderr = log
	input, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer output.Close()
	server, err := startChild(cmd, true)
	if err != nil {
		return fmt.Errorf("start Codex app-server: %w", err)
	}
	defer server.stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bridgeDone := make(chan error, 1)
	var mu sync.Mutex
	var connection *websocket.Conn
	connected := false
	httpServer := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			http.Error(w, "wrapper stopping", http.StatusServiceUnavailable)
			return
		}
		if connected {
			mu.Unlock()
			http.Error(w, "one TUI connection per run", http.StatusConflict)
			return
		}
		if req.Header.Get("Origin") != "" {
			mu.Unlock()
			http.Error(w, "browser connections are not supported", http.StatusForbidden)
			return
		}
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			mu.Unlock()
			return
		}
		connected = true
		connection = ws
		mu.Unlock()
		defer ws.CloseNow()
		bridgeDone <- Bridge(ctx, ws, input, output, r, store.Save)
	})
	go func() { _ = httpServer.Serve(listener) }()
	defer httpServer.Close()
	defer func() {
		cancel()
		_ = httpServer.Close()
		mu.Lock()
		ws := connection
		started := connected
		mu.Unlock()
		if ws != nil {
			_ = ws.CloseNow()
		}
		// Pipe closure unblocks bridge I/O before the record can be saved again.
		_ = input.Close()
		_ = output.Close()
		if started {
			<-bridgeDone
		}
	}()
	args := []string{"--remote", "unix://" + socket}
	if resume {
		args = append(args, "resume", r.ThreadID)
	}
	args = append(args, tuiArgs...)
	tuiCmd := exec.Command(r.CodexBin, args...)
	tuiCmd.Dir = r.CWD
	tuiCmd.Env = childEnvironment(r.CodexHome)
	tuiCmd.Stdin = os.Stdin
	tuiCmd.Stdout = os.Stdout
	tuiCmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "tkmax run %s — set /goal in Codex; inspect with tkmax status %s\n", r.ID, r.ID)
	tui, err := startChild(tuiCmd, false)
	if err != nil {
		return fmt.Errorf("start Codex TUI: %w", err)
	}
	defer tui.stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tui.done:
		return tui.err
	case <-server.done:
		return fmt.Errorf("Codex app-server exited (%v); see %s", server.err, log.Name())
	case err := <-bridgeDone:
		// Preserve the result for the deferred join. TUI disconnect commonly
		// precedes its normal process exit by a few milliseconds.
		bridgeDone <- err
		select {
		case <-tui.done:
			return tui.err
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("Codex app-server disconnected; see %s", log.Name())
			}
			return err
		}
	}
}

func childEnvironment(home string) []string {
	env := os.Environ()
	if home == "" {
		return env
	}
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, "CODEX_HOME=") {
			result = append(result, entry)
		}
	}
	return append(result, "CODEX_HOME="+home)
}
