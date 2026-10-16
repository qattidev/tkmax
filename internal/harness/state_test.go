package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTripAndExclusiveLock(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "runs")}
	r, err := NewRecord("/tmp", "/usr/bin/codex", []string{"--model", "test"}, 5*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.ThreadID = "specific-thread"
	r.SessionName = "Fix quota recovery"
	r.Goal = &Goal{ThreadID: r.ThreadID, Objective: "Finish", Status: "usageLimited", CreatedAt: 1}
	r.NextCheck = time.Now().Add(4 * time.Hour).UTC()
	r.RecoveryPending = true
	lock, err := s.Lock(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.Lock(r.ID); err == nil {
		duplicate.Close()
		t.Fatal("second owner acquired lock")
	}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.NextCheck.Equal(r.NextCheck) || loaded.ThreadID != r.ThreadID || loaded.SessionName != r.SessionName || !loaded.RecoveryPending {
		t.Fatal("resume state lost")
	}
	path, _ := s.path(r.ID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v", info.Mode())
	}
	lock.Close()
	lock, err = s.Lock(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if err := s.Save(loaded); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(s.Dir, ".state-*"))
	if len(files) != 0 {
		t.Fatal("temporary state left behind")
	}
}

func TestListIncludesEveryRepositoryAndCompletedRuns(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "runs")}
	if records, err := s.List(); err != nil || len(records) != 0 {
		t.Fatalf("empty store: %v, %v", records, err)
	}
	var want []*Record
	for i, dir := range []string{"/project-a", "/project-b", "/project-a"} {
		r, _ := NewRecord(dir, "codex", nil, time.Hour)
		if i == 1 {
			r.SessionName = "Completed task"
			r.Goal = &Goal{Status: "complete"}
			r.StoppedAt = time.Now()
		}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
		want = append(want, r)
	}
	got, err := s.List()
	if err != nil || len(got) != len(want) {
		t.Fatalf("list: %v, %v", got, err)
	}
	for i, r := range got {
		if r.ID != want[len(want)-1-i].ID {
			t.Fatalf("wrong order: %v", got)
		}
	}
}

func TestLatestUsesDirectoryAndSkipsCompletedRuns(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for i, dir := range []string{"/project-a", "/project-b", "/project-a"} {
		r, _ := NewRecord(dir, "codex", nil, time.Hour)
		r.ThreadID = "thread"
		r.Goal = &Goal{Status: "active"}
		if i == 2 {
			r.Goal.Status = "complete"
		}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Latest("/project-a")
	if err != nil {
		t.Fatal(err)
	}
	if r.Goal.Status != "active" || r.CWD != "/project-a" {
		t.Fatal("wrong run selected")
	}
	if _, err := s.Latest("/other"); err == nil {
		t.Fatal("selected unrelated run")
	}
	if _, err := s.Load("../../outside"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestStateVersionAndCorruptionAreNotSilentlyIgnored(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	id := strings.Repeat("b", 24)
	path, _ := s.path(id)
	for _, body := range []string{`{`, `{"version":99}`, `{"version":1,"id":"wrong"}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load(id); err == nil {
			t.Fatalf("accepted corrupt state %s", body)
		}
	}
}

func TestPersistedDeadlineSurvivesReconnect(t *testing.T) {
	r := newRig(t)
	r.e.Record.NextCheck = r.now.Add(3 * time.Hour)
	resume := r.user("thread/resume", map[string]any{"threadId": "thread-1"})
	r.reply(resume, map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}})
	r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
	r.tick(2 * time.Hour)
	if len(r.server) != 0 {
		t.Fatal("saved wait shortened")
	}
	r.tick(2 * time.Hour)
	r.preflight(Quota{Allowed: ptr(true)})
	r.request("thread/goal/set")
}

func TestLegacyRecordsAndCapabilityMetadataRemainCompatible(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	r, err := NewRecord(t.TempDir(), "codex", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.Load(r.ID)
	if err != nil || legacy.CapabilitiesKnown {
		t.Fatalf("legacy record: %+v, %v", legacy, err)
	}
	r.CodexHome = t.TempDir()
	r.ServerMode = "shared"
	r.CapabilitiesKnown = true
	r.ToolRequirements = []ToolRequirement{{Kind: "app", AppID: "github"}, {Kind: "mcp", Server: "local", Tool: "lookup"}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(r.ID)
	if err != nil || len(loaded.ToolRequirements) != 2 || !loaded.CapabilitiesKnown {
		t.Fatalf("lost capability metadata: %+v, %v", loaded, err)
	}
	r.ToolRequirements = []ToolRequirement{{Kind: "unknown"}}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(r.ID); err == nil {
		t.Fatal("corrupt dependency snapshot was silently accepted")
	}
}

func TestSessionOwnershipCanonicalizesSymlinkedHomes(t *testing.T) {
	home := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: t.TempDir()}
	lock, err := s.LockThread(home, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := s.LockThread(alias, testSessionID); err == nil {
		other.Close()
		t.Fatal("symlink alias bypassed native session ownership")
	}
	got, err := CanonicalCodexHome(filepath.Join(alias, "not-created", "yet"))
	if err != nil || got != filepath.Join(home, "not-created", "yet") {
		t.Fatalf("uncreated home resolved incorrectly: %s, %v", got, err)
	}
}
