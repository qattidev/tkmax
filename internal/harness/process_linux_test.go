//go:build linux

package harness

import (
	"bufio"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCodexVersionSupported(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"0.154.0", true},
		{"0.154.1", true},
		{"0.155.0", true},
		{"0.1000.0", true},
		{"1.0.0", true},
		{"0.153.99", false},
		{"0.9.0", false},
		{"", false},
		{"0.154", false},
		{"1.0.invalid", false},
		{"0.154.0-alpha.1", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			if got := codexVersionSupported(tc.version); got != tc.want {
				t.Fatalf("codexVersionSupported(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

func TestChildStopTerminatesOwnedProcessGroup(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd := exec.Command("/bin/sh", "-c", `trap 'kill "$kid" 2>/dev/null; wait "$kid"; exit' TERM; sleep 60 & kid=$!; echo "$kid"; wait`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child, err := startChild(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		child.stop()
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		child.stop()
		t.Fatal(err)
	}
	child.stop()
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("descendant %d survived shutdown: %v", pid, err)
	}
}

func TestChildStopEscalatesWhenTerminationIgnored(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; echo ready; while :; do sleep 60; done`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child, err := startChild(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		child.stop()
		t.Fatal(err)
	}
	started := time.Now()
	child.stop()
	if time.Since(started) > 5*time.Second {
		t.Fatal("shutdown exceeded grace period")
	}
	if child.err == nil {
		t.Fatal("expected forced process termination")
	}
}
