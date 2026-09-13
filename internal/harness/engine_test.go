package harness

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type rig struct {
	t           *testing.T
	e           *Engine
	now         time.Time
	server, tui []message
	saves       []Record
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, now: time.Unix(1800000000, 0)}
	record := &Record{ID: strings.Repeat("a", 24), Version: 1, ThreadID: "thread-1", CWD: "/tmp", FallbackWait: 5 * time.Hour, Goal: &Goal{ThreadID: "thread-1", Objective: "Finish the task", Status: "usageLimited", CreatedAt: 1}}
	r.e = NewEngine(record, func(b []byte) error { m, err := decode(b); r.server = append(r.server, m); return err }, func(b []byte) error { m, err := decode(b); r.tui = append(r.tui, m); return err }, func(s *Record) error {
		var copy Record
		_ = json.Unmarshal(raw(s), &copy)
		r.saves = append(r.saves, copy)
		return nil
	})
	r.e.ready = true
	r.e.status = "idle"
	return r
}

func (r *rig) tick(d time.Duration) {
	r.t.Helper()
	r.now = r.now.Add(d)
	if err := r.e.Tick(r.now); err != nil {
		r.t.Fatal(err)
	}
}
func (r *rig) request(method string) message {
	r.t.Helper()
	if len(r.server) == 0 {
		r.t.Fatalf("expected %s, no request", method)
	}
	m := r.server[0]
	r.server = r.server[1:]
	if str(m["method"]) != method {
		r.t.Fatalf("expected %s, got %s", method, m["method"])
	}
	return m
}
func (r *rig) reply(m message, v any) {
	r.t.Helper()
	if err := r.e.Server(raw(map[string]any{"id": m["id"], "result": v}), r.now); err != nil {
		r.t.Fatal(err)
	}
}
func (r *rig) notify(method string, p any) {
	r.t.Helper()
	if err := r.e.Server(raw(map[string]any{"method": method, "params": p}), r.now); err != nil {
		r.t.Fatal(err)
	}
}
func (r *rig) user(method string, p any) message {
	r.t.Helper()
	if err := r.e.TUI(raw(map[string]any{"id": 7, "method": method, "params": p}), r.now); err != nil {
		r.t.Fatal(err)
	}
	return r.request(method)
}
func (r *rig) goal(status string) *Goal { g := *r.e.Record.Goal; g.Status = status; return &g }
func (r *rig) goalEvent(status string) {
	r.notify("thread/goal/updated", map[string]any{"threadId": "thread-1", "goal": r.goal(status)})
}
func (r *rig) preflight(q Quota) {
	r.t.Helper()
	r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
	r.reply(r.request("thread/read"), map[string]any{"thread": map[string]any{"id": "thread-1", "model": "model-a", "status": map[string]any{"type": "idle"}}})
	r.reply(r.request("account/rateLimits/read"), q)
}
func ptr[T any](v T) *T { return &v }

func TestQuotaCyclesAndCompletion(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		reset := r.now.Add(2 * time.Hour).Unix()
		r.tick(0)
		r.preflight(Quota{Allowed: ptr(false), Bucket: QuotaBucket{Primary: &Window{UsedPercent: 100, ResetsAt: &reset}}})
		if want := time.Unix(reset, 0).Add(30 * time.Second); !r.e.Record.NextCheck.Equal(want) {
			t.Fatalf("deadline = %v, want %v", r.e.Record.NextCheck, want)
		}
		r.tick(2 * time.Hour)
		if len(r.server) != 0 {
			t.Fatal("retried before reset buffer")
		}
		r.tick(31 * time.Second)
		r.preflight(Quota{Allowed: ptr(true)})
		m := r.request("thread/goal/set")
		var p message
		_ = json.Unmarshal(m["params"], &p)
		if len(p) != 2 || str(p["status"]) != "active" || str(p["threadId"]) != "thread-1" {
			t.Fatalf("recovery replaced goal/budget: %s", m["params"])
		}
		if !r.saves[len(r.saves)-1].RecoveryPending {
			t.Fatal("recovery not persisted before dispatch")
		}
		r.reply(m, map[string]any{"goal": r.goal("active")})
		r.tick(time.Hour)
		if len(r.server) != 0 {
			t.Fatal("wrapper duplicated native goal continuation")
		}
		r.goalEvent("usageLimited")
	}
	r.goalEvent("complete")
	r.tick(365 * 24 * time.Hour)
	if len(r.server) != 0 || r.e.Record.State != "complete" {
		t.Fatal("completed goal resumed")
	}
}

func TestFallbackAndDefinitiveDenial(t *testing.T) {
	r := newRig(t)
	r.tick(0)
	r.preflight(Quota{})
	if !r.e.Record.NextCheck.Equal(r.now.Add(5 * time.Hour)) {
		t.Fatal("missing fallback deadline")
	}
	r.tick(5 * time.Hour)
	r.preflight(Quota{Allowed: ptr(false)})
	if len(r.server) != 0 {
		t.Fatal("explicit quota denial ignored")
	}
	r.tick(5 * time.Hour)
	r.preflight(Quota{})
	r.request("thread/goal/set")
}

func TestUserPauseWinsEveryPreflightStage(t *testing.T) {
	for stage := 0; stage < 3; stage++ {
		t.Run(string(rune('A'+stage)), func(t *testing.T) {
			r := newRig(t)
			r.e.Record.NextCheck = r.now
			r.tick(0)
			m := r.request("thread/goal/get")
			if stage > 0 {
				r.reply(m, map[string]any{"goal": r.e.Record.Goal})
				m = r.request("thread/read")
			}
			if stage > 1 {
				r.reply(m, map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}})
				m = r.request("account/rateLimits/read")
			}
			pause := r.user("thread/goal/set", map[string]any{"threadId": "thread-1", "status": "paused"})
			// Reply from an older recovery must not undo the user's newer intent.
			switch stage {
			case 0:
				r.reply(m, map[string]any{"goal": r.e.Record.Goal})
			case 1:
				r.reply(m, map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}})
			case 2:
				r.reply(m, Quota{Allowed: ptr(true)})
			}
			if len(r.server) != 0 {
				t.Fatal("stale preflight dispatched a request after user pause")
			}
			r.goalEvent("paused")
			r.reply(pause, map[string]any{"goal": r.e.Record.Goal})
			r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
			r.tick(24 * time.Hour)
			if len(r.server) != 0 {
				t.Fatal("paused goal resumed")
			}
		})
	}
}

func TestGoalChangedOrClearedDuringRecovery(t *testing.T) {
	for _, kind := range []string{"clear", "replace", "complete", "blocked", "budgetLimited"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t)
			r.tick(0)
			pending := r.request("thread/goal/get")
			old := *r.e.Record.Goal
			switch kind {
			case "clear":
				r.notify("thread/goal/cleared", map[string]any{"threadId": "thread-1"})
			case "replace":
				g := r.goal("active")
				g.Objective = "Different task"
				r.notify("thread/goal/updated", map[string]any{"threadId": "thread-1", "goal": g})
			default:
				r.goalEvent(kind)
			}
			r.reply(pending, map[string]any{"goal": &old})
			if len(r.server) != 0 {
				t.Fatal("stale goal continued")
			}
		})
	}
}

func TestRecoveryTimeoutIsReconciledNotReplayed(t *testing.T) {
	r := newRig(t)
	r.e.Record.NextCheck = r.now
	r.tick(0)
	r.preflight(Quota{Allowed: ptr(true)})
	lost := r.request("thread/goal/set")
	r.tick(16 * time.Second)
	if !r.e.Record.RecoveryPending {
		t.Fatal("forgot uncertain recovery")
	}
	r.tick(time.Minute)
	r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
	if len(r.server) != 0 || r.e.Record.RecoveryPending {
		t.Fatal("uncertain mutation replayed instead of reconciled")
	}
	if !r.e.Record.NextCheck.Equal(r.now.Add(5 * time.Hour)) {
		t.Fatal("missing reconciliation cooldown")
	}
	r.reply(lost, map[string]any{"goal": r.goal("active")})
	if r.e.Record.Goal.Status != "usageLimited" {
		t.Fatal("late response overwritten authoritative read")
	}
}

func TestApprovalsAndUserWorkPreventRecovery(t *testing.T) {
	for _, kind := range []string{"approval", "turn", "input"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t)
			switch kind {
			case "approval":
				_ = r.e.Server([]byte(`{"id":7,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1"}}`), r.now)
			case "turn":
				r.notify("turn/started", map[string]any{"threadId": "thread-1"})
			case "input":
				r.notify("thread/status/changed", map[string]any{"threadId": "thread-1", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnUserInput"}}})
			}
			r.tick(5 * time.Hour)
			if len(r.server) != 0 {
				t.Fatal("recovery interfered with active user work")
			}
		})
	}
}

func TestRequestIDsAndUnknownFields(t *testing.T) {
	r := newRig(t)
	if err := r.e.TUI([]byte(`{"id":7,"method":"future/method","params":{"newField":"value"},"extension":true}`), r.now); err != nil {
		t.Fatal(err)
	}
	out := r.request("future/method")
	if string(out["extension"]) != "true" || str(out["id"]) == "" {
		t.Fatal("extension lost or id not remapped")
	}
	_ = r.e.Server([]byte(`{"id":7,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1"}}`), r.now)
	approval := r.tui[len(r.tui)-1]
	if string(approval["id"]) == string(out["id"]) {
		t.Fatal("ID namespaces collided")
	}
	r.reply(out, map[string]any{"newResponseField": 42})
	if string(r.tui[len(r.tui)-1]["id"]) != "7" {
		t.Fatal("TUI id not restored")
	}
	if err := r.e.TUI(raw(map[string]any{"id": approval["id"], "result": map[string]any{"decision": "accept"}}), r.now); err != nil {
		t.Fatal(err)
	}
	if string(r.server[len(r.server)-1]["id"]) != "7" {
		t.Fatal("approval id not restored")
	}
}

func TestResolvedServerRequestRemapsID(t *testing.T) {
	r := newRig(t)
	_ = r.e.Server([]byte(`{"id":"server-id","method":"approval","params":{}}`), r.now)
	id := r.tui[0]["id"]
	r.notify("serverRequest/resolved", map[string]any{"threadId": "thread-1", "requestId": "server-id"})
	var p message
	_ = json.Unmarshal(r.tui[1]["params"], &p)
	if string(p["requestId"]) != string(id) || len(r.e.serverRequests) != 0 {
		t.Fatal("resolved approval id not translated")
	}
}

func TestSubagentAndLateSelectionCannotSwitchGoal(t *testing.T) {
	r := newRig(t)
	r.notify("thread/started", map[string]any{"thread": map[string]any{"id": "child", "parentThreadId": "thread-1"}})
	r.notify("thread/goal/updated", map[string]any{"threadId": "child", "goal": &Goal{ThreadID: "child", Status: "complete"}})
	if r.e.Record.ThreadID != "thread-1" || r.e.Record.Goal.Status != "usageLimited" {
		t.Fatal("child selected")
	}
	a := r.user("thread/resume", map[string]any{"threadId": "thread-a"})
	b := r.user("thread/resume", map[string]any{"threadId": "thread-b"})
	r.reply(b, map[string]any{"thread": map[string]any{"id": "thread-b", "status": map[string]any{"type": "idle"}}})
	r.reply(a, map[string]any{"thread": map[string]any{"id": "thread-a", "status": map[string]any{"type": "idle"}}})
	if r.e.Record.ThreadID != "thread-b" {
		t.Fatal("late selection won")
	}
}

func TestSaveFailurePreventsRecoveryMutation(t *testing.T) {
	r := newRig(t)
	r.e.Record.NextCheck = r.now
	r.tick(0)
	r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
	r.reply(r.request("thread/read"), map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}})
	m := r.request("account/rateLimits/read")
	r.e.Save = func(*Record) error { return errors.New("disk full") }
	err := r.e.Server(raw(map[string]any{"id": m["id"], "result": Quota{Allowed: ptr(true)}}), r.now)
	if err == nil || len(r.server) != 0 {
		t.Fatal("recovery dispatched without durable intent")
	}
}

func TestMetadataRetryDoesNotShortenFallback(t *testing.T) {
	r := newRig(t)
	r.tick(0)
	r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
	r.reply(r.request("thread/read"), map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}})
	m := r.request("account/rateLimits/read")
	if err := r.e.Server(raw(map[string]any{"id": m["id"], "error": map[string]any{"code": -1, "message": "connection failed"}}), r.now); err != nil {
		t.Fatal(err)
	}
	r.tick(time.Minute)
	r.preflight(Quota{})
	if len(r.server) != 0 {
		t.Fatal("metadata retry triggered a premature model attempt")
	}
	if r.e.Record.ProbeAfter.Before(r.now.Add(4 * time.Hour)) {
		t.Fatal("fallback shortened")
	}
}

func TestAnsweredApprovalResolutionRetainsMapping(t *testing.T) {
	r := newRig(t)
	_ = r.e.Server([]byte(`{"id":12,"method":"approval","params":{}}`), r.now)
	id := r.tui[0]["id"]
	if err := r.e.TUI(raw(map[string]any{"id": id, "result": map[string]any{"decision": "accept"}}), r.now); err != nil {
		t.Fatal(err)
	}
	if len(r.e.serverRequests) != 0 {
		t.Fatal("answered approval still blocks recovery")
	}
	r.notify("serverRequest/resolved", map[string]any{"threadId": "thread-1", "requestId": 12})
	var p message
	_ = json.Unmarshal(r.tui[1]["params"], &p)
	if string(p["requestId"]) != string(id) || len(r.e.answeredRequests) != 0 {
		t.Fatal("answered approval resolution lost")
	}
}

func TestNotificationOptOutPreservedForTUI(t *testing.T) {
	r := newRig(t)
	if err := r.e.TUI([]byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"test"},"capabilities":{"optOutNotificationMethods":["thread/goal/updated","unrelated/event"]}}}`), r.now); err != nil {
		t.Fatal(err)
	}
	m := r.request("initialize")
	if !strings.Contains(string(m["params"]), "unrelated/event") || strings.Contains(string(m["params"]), "thread/goal/updated") {
		t.Fatal("wrong upstream notification filter")
	}
	r.goalEvent("complete")
	if r.e.Record.State != "complete" || len(r.tui) != 0 {
		t.Fatal("goal event not observed privately")
	}
}

func TestUsageLimitedSystemErrorCanRecover(t *testing.T) {
	r := newRig(t)
	r.e.status = "systemError"
	r.tick(0)
	r.reply(r.request("thread/goal/get"), map[string]any{"goal": r.e.Record.Goal})
	r.reply(r.request("thread/read"), map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "systemError"}}})
	r.reply(r.request("account/rateLimits/read"), Quota{Allowed: ptr(true)})
	r.request("thread/goal/set")
}

func TestAuthenticationFailureStaysVisible(t *testing.T) {
	r := newRig(t)
	r.tick(0)
	m := r.request("thread/goal/get")
	if err := r.e.Server(raw(map[string]any{"id": m["id"], "error": map[string]any{"code": 401, "message": "authentication required"}}), r.now); err != nil {
		t.Fatal(err)
	}
	r.goalEvent("usageLimited")
	r.tick(24 * time.Hour)
	if !r.e.Record.ManualHold || r.e.Record.State != "needs-attention" || r.e.Record.Message != "authentication required" || len(r.server) != 0 {
		t.Fatal("authentication failure hidden or retried as quota")
	}
}
