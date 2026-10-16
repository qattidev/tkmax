package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharedShutdownPausesOnlyManagedGoalAndInterruptsItsTurn(t *testing.T) {
	r := newRig(t).e.Record
	r.CodexHome = t.TempDir()
	r.Goal.Status = "active"
	paused := *r.Goal
	paused.Status = "paused"
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("TKMAX_RPC_CALLS", calls)
	r.CodexBin = fakeCodex(t, r.CodexHome, map[string]any{
		"thread/goal/get":   map[string]any{"goal": r.Goal},
		"thread/goal/set":   map[string]any{"goal": &paused},
		"thread/read":       map[string]any{"thread": map[string]any{"id": r.ThreadID, "historyMode": "paginated", "status": map[string]any{"type": "active"}}},
		"thread/turns/list": map[string]any{"data": []any{map[string]any{"id": "owned-turn", "status": "inProgress"}}},
		"turn/interrupt":    map[string]any{},
	})
	if err := PauseManagedGoal(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.Goal.Status != "paused" || !r.ManualHold || r.State != "paused" {
		t.Fatal("pause was not persisted")
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, `"status":"paused"`) || !strings.Contains(text, `"turnId":"owned-turn"`) || strings.Contains(text, "thread/resume") || strings.Contains(text, `"status":"active"`) {
		t.Fatalf("unsafe shutdown requests: %s", text)
	}
}

func TestSharedShutdownPreservesWaitingOrReplacedGoals(t *testing.T) {
	for _, kind := range []string{"waiting", "replaced", "completed", "pause-denied"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t).e.Record
			r.CodexHome = t.TempDir()
			r.NextCheck = time.Now().Add(3 * time.Hour)
			deadline := r.NextCheck
			current := *r.Goal
			switch kind {
			case "replaced":
				current.Status = "active"
				current.CreatedAt++
			case "completed":
				current.Status = "complete"
			case "pause-denied":
				current.Status = "active"
			}
			calls := filepath.Join(t.TempDir(), "calls.jsonl")
			t.Setenv("TKMAX_RPC_CALLS", calls)
			r.CodexBin = fakeCodex(t, r.CodexHome, map[string]any{"thread/goal/get": map[string]any{"goal": &current}})
			err := PauseManagedGoal(context.Background(), r)
			if (err != nil) != (kind == "pause-denied") {
				t.Fatalf("unexpected pause result: %v", err)
			}
			if kind == "completed" {
				if !r.NextCheck.IsZero() || r.Goal.Status != "complete" {
					t.Fatal("native goal completion was not reconciled")
				}
			} else if !r.NextCheck.Equal(deadline) {
				t.Fatal("saved wait was discarded")
			}
			b, _ := os.ReadFile(calls)
			if kind != "pause-denied" && strings.Contains(string(b), "thread/goal/set") {
				t.Fatal("waiting/replaced/completed goal was changed")
			}
		})
	}
}

func TestShutdownReconcilesTurnFinishingDuringPause(t *testing.T) {
	r := newRig(t).e.Record
	r.CodexHome = t.TempDir()
	r.Goal.Status = "active"
	paused := *r.Goal
	paused.Status = "paused"
	r.CodexBin = fakeCodex(t, r.CodexHome, map[string]any{
		"thread/goal/get": map[string]any{"goal": r.Goal}, "thread/goal/set": map[string]any{"goal": &paused},
		"thread/read":       map[string]any{"thread": map[string]any{"id": r.ThreadID, "historyMode": "paginated", "status": map[string]any{"type": "active"}}},
		"thread/read#2":     map[string]any{"thread": map[string]any{"id": r.ThreadID, "status": map[string]any{"type": "idle"}}},
		"thread/turns/list": map[string]any{"data": []any{map[string]any{"id": "finished", "status": "completed"}}},
	})
	if err := PauseManagedGoal(context.Background(), r); err != nil {
		t.Fatal("finished turn caused a false shutdown failure:", err)
	}
}
