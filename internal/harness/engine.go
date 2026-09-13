package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Engine is an actor: every method and callback runs on the bridge's event loop.
// No timer goroutine can race a manual pause or an arriving goal update.
type Engine struct {
	Record           *Record
	ToServer         func([]byte) error
	ToTUI            func([]byte) error
	Save             func(*Record) error
	now              time.Time
	ready            bool
	sequence         uint64
	epoch            uint64
	selection        uint64
	switching        bool
	pending          map[string]request
	serverRequests   map[string]json.RawMessage
	answeredRequests map[string]json.RawMessage
	optOut           map[string]bool
	busy             bool
	status           string
	model            string
	retry            time.Duration
	pollAt           time.Time
	err              error
}

type request struct {
	original  json.RawMessage
	method    string
	thread    string
	selection uint64
	manual    bool
	callback  func(json.RawMessage, *rpcError)
	deadline  time.Time
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type message map[string]json.RawMessage

func raw(v any) json.RawMessage    { b, _ := json.Marshal(v); return b }
func str(v json.RawMessage) string { var s string; _ = json.Unmarshal(v, &s); return s }
func decode(b []byte) (message, error) {
	var m message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("invalid Codex protocol message: %w", err)
	}
	if m == nil {
		return nil, errors.New("empty Codex protocol message")
	}
	return m, nil
}

func NewEngine(r *Record, server, tui func([]byte) error, save func(*Record) error) *Engine {
	return &Engine{Record: r, ToServer: server, ToTUI: tui, Save: save, pending: make(map[string]request), serverRequests: make(map[string]json.RawMessage), answeredRequests: make(map[string]json.RawMessage), optOut: make(map[string]bool), status: "unknown", retry: 5 * time.Second}
}

func (e *Engine) send(f func([]byte) error, m message) {
	if e.err != nil {
		return
	}
	b, err := json.Marshal(m)
	if err == nil {
		err = f(b)
	}
	if err != nil {
		e.err = err
	}
}

func (e *Engine) save() {
	if e.err == nil {
		e.err = e.Save(e.Record)
	}
}

func (e *Engine) id(prefix string) string {
	e.sequence++
	return fmt.Sprintf("tkmax:%s:%d", prefix, e.sequence)
}

func (e *Engine) warning(text string) {
	e.Record.Message = text
	e.send(e.ToTUI, message{"method": raw("warning"), "params": raw(map[string]any{"threadId": e.Record.ThreadID, "message": "tkmax: " + text})})
	e.save()
}

func (e *Engine) invalidate() { e.epoch++; e.busy = false }

func (e *Engine) TUI(b []byte, now time.Time) error {
	e.now = now
	m, err := decode(b)
	if err != nil {
		return err
	}
	method := str(m["method"])
	if method == "" {
		// Responses to approval and tool requests use the independent server ID map.
		id := str(m["id"])
		if original, ok := e.serverRequests[id]; ok {
			m["id"] = original
			delete(e.serverRequests, id)
			e.answeredRequests[id] = original
		}
		e.send(e.ToServer, m)
		return e.err
	}
	if method == "initialized" {
		e.ready = true
	}
	if method == "initialize" {
		// Observe required events even if this TUI opted out; keep its original
		// filtering on the downstream connection.
		var p message
		_ = json.Unmarshal(m["params"], &p)
		var caps message
		_ = json.Unmarshal(p["capabilities"], &caps)
		var methods []string
		_ = json.Unmarshal(caps["optOutNotificationMethods"], &methods)
		for _, method := range methods {
			e.optOut[method] = true
		}
		if len(methods) > 0 {
			keep := make([]string, 0, len(methods))
			for _, name := range methods {
				if !observedMethod(name) {
					keep = append(keep, name)
				}
			}
			caps["optOutNotificationMethods"] = raw(keep)
			p["capabilities"] = raw(caps)
			m["params"] = raw(p)
		}
	}
	if id, ok := m["id"]; ok {
		var p struct {
			ThreadID string `json:"threadId"`
			Status   string `json:"status"`
		}
		_ = json.Unmarshal(m["params"], &p)
		r := request{original: id, method: method, thread: p.ThreadID}
		if method == "thread/start" || method == "thread/resume" || method == "thread/fork" {
			e.selection++
			e.switching = true
			e.invalidate()
			r.selection = e.selection
		}
		if p.ThreadID == e.Record.ThreadID && p.ThreadID != "" && isUserAction(method) {
			r.manual = true
			e.invalidate()
			if method == "turn/interrupt" || method == "thread/goal/clear" || (method == "thread/goal/set" && p.Status != "" && p.Status != "active") {
				e.Record.ManualHold = true
			}
			if method == "turn/start" || (method == "thread/goal/set" && p.Status == "active") {
				e.Record.ManualHold = false
			}
			e.save()
		}
		key := e.id("tui")
		e.pending[key] = r
		m["id"] = raw(key)
	}
	e.send(e.ToServer, m)
	return e.err
}

func isUserAction(method string) bool {
	switch method {
	case "thread/goal/set", "thread/goal/clear", "turn/start", "turn/steer", "turn/interrupt", "thread/queue/add", "thread/queue/start", "thread/settings/update", "turn/settings/update", "thread/archive", "thread/delete":
		return true
	}
	return false
}

func observedMethod(method string) bool {
	switch method {
	case "thread/name/updated", "thread/goal/updated", "thread/goal/cleared", "thread/status/changed", "thread/settings/updated", "turn/started", "turn/completed", "error", "thread/closed", "thread/archived", "thread/deleted", "serverRequest/resolved":
		return true
	}
	return false
}

func (e *Engine) Server(b []byte, now time.Time) error {
	e.now = now
	m, err := decode(b)
	if err != nil {
		return err
	}
	method := str(m["method"])
	if method == "" {
		key := str(m["id"])
		r, ok := e.pending[key]
		if !ok {
			return nil
		} // A late response to an expired internal request.
		delete(e.pending, key)
		var rpcErr *rpcError
		_ = json.Unmarshal(m["error"], &rpcErr)
		if r.callback != nil {
			r.callback(m["result"], rpcErr)
			return e.err
		}
		if r.selection != 0 && r.selection == e.selection {
			e.switching = false
			if rpcErr == nil {
				var result struct {
					Thread threadInfo `json:"thread"`
				}
				if json.Unmarshal(m["result"], &result) == nil && result.Thread.ID != "" && result.Thread.ParentThreadID == "" {
					e.selectThread(result.Thread)
				} else {
					e.Record.ManualHold = true
					e.save()
				}
			}
		}
		m["id"] = r.original
		e.send(e.ToTUI, m)
		if r.manual && r.thread == e.Record.ThreadID {
			e.refreshGoal()
		}
		return e.err
	}
	if id, ok := m["id"]; ok {
		key := e.id("server")
		e.serverRequests[key] = id
		m["id"] = raw(key)
		e.send(e.ToTUI, m)
		return e.err
	}
	if method == "serverRequest/resolved" {
		var p message
		if json.Unmarshal(m["params"], &p) == nil {
			for _, requests := range []map[string]json.RawMessage{e.serverRequests, e.answeredRequests} {
				for key, id := range requests {
					if string(id) == string(p["requestId"]) {
						p["requestId"] = raw(key)
						m["params"] = raw(p)
						delete(requests, key)
						break
					}
				}
			}
		}
	}
	e.observe(method, m["params"])
	if !e.optOut[method] {
		e.send(e.ToTUI, m)
	}
	return e.err
}

type threadInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	CWD            string `json:"cwd"`
	ParentThreadID string `json:"parentThreadId"`
	Model          string `json:"model"`
	Status         struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
	} `json:"status"`
}

func (e *Engine) selectThread(t threadInfo) {
	e.invalidate()
	if t.ID != e.Record.ThreadID {
		e.Record.Goal = nil
		e.Record.NextCheck = time.Time{}
		e.Record.ProbeAfter = time.Time{}
		e.Record.RecoveryPending = false
		e.Record.ManualHold = false
	}
	e.Record.ThreadID = t.ID
	e.Record.SessionName = t.Name
	if t.CWD != "" {
		e.Record.CWD = t.CWD
	}
	e.status = t.Status.Type
	e.model = t.Model
	e.Record.State = "observing"
	e.Record.Message = ""
	e.save()
	e.refreshGoal()
}

func (e *Engine) observe(method string, params json.RawMessage) {
	var p struct {
		ThreadID   string `json:"threadId"`
		ThreadName string `json:"threadName"`
		Goal       *Goal  `json:"goal"`
		Status     struct {
			Type string `json:"type"`
		} `json:"status"`
		Turn struct {
			Status string     `json:"status"`
			Error  *turnError `json:"error"`
		} `json:"turn"`
		Error     *turnError `json:"error"`
		WillRetry bool       `json:"willRetry"`
		Settings  struct {
			Model string `json:"model"`
		} `json:"settings"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	if p.ThreadID == "" || p.ThreadID != e.Record.ThreadID || e.switching {
		return
	}
	switch method {
	case "thread/name/updated":
		e.Record.SessionName = p.ThreadName
		e.save()
	case "thread/goal/updated":
		e.applyGoal(p.Goal)
	case "thread/goal/cleared":
		e.applyGoal(nil)
	case "thread/status/changed":
		e.status = p.Status.Type
	case "thread/settings/updated":
		e.model = p.Settings.Model
		e.invalidate()
	case "turn/started":
		e.status = "active"
		e.invalidate()
	case "turn/completed":
		e.status = "idle"
		if p.Turn.Status == "interrupted" {
			e.Record.ManualHold = true
			e.invalidate()
			e.save()
		}
		if p.Turn.Error != nil {
			e.turnError(p.Turn.Error)
		}
		e.refreshGoal()
	case "error":
		if !p.WillRetry && p.Error != nil {
			e.turnError(p.Error)
		}
	case "thread/closed", "thread/archived", "thread/deleted":
		e.invalidate()
		e.Record.ManualHold = true
		e.Record.State = "inactive"
		e.save()
	}
}

type turnError struct {
	Message string          `json:"message"`
	Code    json.RawMessage `json:"codexErrorInfo"`
}

func (e *Engine) turnError(err *turnError) {
	switch str(err.Code) {
	case "usageLimitExceeded":
		// The persisted native goal is authoritative. A failed ordinary prompt
		// must never create a goal or trigger a synthetic continuation.
		e.refreshGoal()
	case "rateLimitExceeded", "serverOverloaded", "internalServerError":
		// Codex owns retries for ordinary transport/request-rate failures.
	default:
		e.invalidate()
		e.Record.ManualHold = true
		e.Record.State = "needs-attention"
		e.warning(err.Message)
	}
}

func (e *Engine) applyGoal(g *Goal) {
	old := e.Record.Goal
	keepAttention := e.Record.ManualHold && e.Record.State == "needs-attention" && sameGoal(old, g)
	recovered := sameGoal(old, g) && old.Status == "usageLimited" && g.Status == "active" && e.Record.RecoveryPending
	if g != nil && !sameGoal(old, g) {
		e.Record.ManualHold = false
	}
	if !sameGoal(old, g) || (old != nil && g != nil && old.Status != g.Status) {
		e.invalidate()
		e.Record.NextCheck = time.Time{}
		e.Record.ProbeAfter = time.Time{}
		// An observed post-attempt state resolves an uncertain recovery.
		e.Record.RecoveryPending = false
	}
	e.Record.Goal = g
	if !keepAttention && (g == nil || g.Status != "usageLimited") {
		e.Record.Message = ""
	}
	if g == nil {
		e.Record.State = "idle"
		e.Record.NextCheck = time.Time{}
	} else {
		switch g.Status {
		case "active":
			e.Record.State = "working"
			e.Record.NextCheck = time.Time{}
			e.Record.RecoveryPending = false
		case "usageLimited":
			e.Record.State = "waiting"
		case "paused", "blocked", "budgetLimited", "complete":
			e.Record.State = g.Status
			e.Record.NextCheck = time.Time{}
		default:
			e.Record.State = "needs-attention"
			e.Record.ManualHold = true
		}
	}
	if keepAttention {
		e.Record.State = "needs-attention"
	}
	e.save()
	if recovered {
		e.warning("Quota recovery accepted; continuing the existing goal.")
	}
}

func (e *Engine) query(method string, params any, cb func(json.RawMessage, *rpcError)) {
	if e.err != nil {
		return
	}
	id := e.id("internal")
	e.pending[id] = request{method: method, callback: cb, deadline: e.now.Add(15 * time.Second)}
	e.send(e.ToServer, message{"id": raw(id), "method": raw(method), "params": raw(params)})
}

func (e *Engine) refreshGoal() {
	if !e.ready || e.Record.ThreadID == "" || e.busy || e.switching {
		return
	}
	e.busy = true
	e.pollAt = e.now.Add(30 * time.Second)
	epoch, thread := e.epoch, e.Record.ThreadID
	e.query("thread/goal/get", map[string]any{"threadId": thread}, func(b json.RawMessage, err *rpcError) {
		if epoch != e.epoch || thread != e.Record.ThreadID {
			return
		}
		e.busy = false
		if err != nil {
			e.queryFailure(err)
			return
		}
		var p struct {
			Goal *Goal `json:"goal"`
		}
		if json.Unmarshal(b, &p) != nil {
			e.failProtocol("invalid thread/goal/get response")
			return
		}
		e.applyGoal(p.Goal)
	})
}

func (e *Engine) Tick(now time.Time) error {
	e.now = now
	var expired []string
	for id, r := range e.pending {
		if r.callback != nil && !now.Before(r.deadline) {
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		r := e.pending[id]
		delete(e.pending, id)
		r.callback(nil, &rpcError{Code: -1, Message: "Codex request timed out; reconciling before retry"})
	}
	if !e.ready || e.busy || e.switching || e.Record.ThreadID == "" || e.Record.ManualHold {
		return e.err
	}
	for _, p := range e.pending {
		if p.manual {
			return e.err
		}
	}
	if len(e.serverRequests) != 0 || e.status == "active" {
		return e.err
	}
	if e.Record.Goal == nil {
		if !now.Before(e.pollAt) {
			e.refreshGoal()
		}
		return e.err
	}
	if e.Record.Goal.Status != "usageLimited" {
		return e.err
	}
	if !e.Record.NextCheck.IsZero() && now.Before(e.Record.NextCheck) {
		return e.err
	}
	e.recover()
	return e.err
}

func (e *Engine) recover() {
	e.busy = true
	epoch, thread := e.epoch, e.Record.ThreadID
	goal := *e.Record.Goal
	// Metadata retries must never shorten the fallback before an unverified
	// model attempt. Persist that boundary separately from the polling timer.
	if e.Record.ProbeAfter.IsZero() {
		if e.Record.NextCheck.IsZero() {
			e.Record.ProbeAfter = e.now.Add(e.Record.FallbackWait)
		} else {
			e.Record.ProbeAfter = e.Record.NextCheck
		}
		e.save()
	}
	probeDue := !e.now.Before(e.Record.ProbeAfter)
	valid := func() bool {
		return epoch == e.epoch && thread == e.Record.ThreadID && !e.Record.ManualHold && sameGoal(&goal, e.Record.Goal)
	}
	e.query("thread/goal/get", map[string]any{"threadId": thread}, func(b json.RawMessage, err *rpcError) {
		if !valid() {
			return
		}
		if err != nil {
			e.busy = false
			e.queryFailure(err)
			return
		}
		var p struct {
			Goal *Goal `json:"goal"`
		}
		if json.Unmarshal(b, &p) != nil {
			e.failProtocol("invalid goal response")
			return
		}
		if !sameGoal(&goal, p.Goal) || p.Goal.Status != "usageLimited" {
			e.busy = false
			e.applyGoal(p.Goal)
			return
		}
		if e.Record.RecoveryPending {
			// A previously dispatched request may have succeeded and exhausted
			// quota again. Never immediately replay it on a lost response/restart.
			e.Record.RecoveryPending = false
			e.Record.NextCheck = e.now.Add(e.Record.FallbackWait)
			e.Record.ProbeAfter = e.Record.NextCheck
			e.busy = false
			e.warning("Recovery reconciled; goal remains usage-limited. Next check: " + e.Record.NextCheck.Local().Format(time.RFC1123))
			return
		}
		e.query("thread/read", map[string]any{"threadId": thread}, func(b json.RawMessage, err *rpcError) {
			if !valid() {
				return
			}
			if err != nil {
				e.busy = false
				e.queryFailure(err)
				return
			}
			var p struct {
				Thread threadInfo `json:"thread"`
			}
			if json.Unmarshal(b, &p) != nil || p.Thread.ID != thread {
				e.failProtocol("invalid thread response")
				return
			}
			e.status = p.Thread.Status.Type
			e.model = p.Thread.Model
			if !recoverableThreadStatus(e.status) || len(p.Thread.Status.ActiveFlags) != 0 || len(e.serverRequests) != 0 {
				e.busy = false
				e.Record.NextCheck = e.now.Add(time.Minute)
				e.save()
				return
			}
			e.query("account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true}, func(b json.RawMessage, err *rpcError) {
				if !valid() {
					return
				}
				e.busy = false
				if err != nil {
					e.queryFailure(err)
					return
				}
				var q Quota
				if json.Unmarshal(b, &q) != nil {
					e.failProtocol("invalid quota response")
					return
				}
				e.retry = 5 * time.Second
				// A definitive server denial always wins. Null permits only a
				// scheduled probe; reset timestamps themselves never grant access.
				if !hasFutureBlock(q, e.model, e.now) && ((q.Allowed != nil && *q.Allowed) || (q.Allowed == nil && probeDue)) {
					e.resumeGoal()
				} else {
					e.Record.NextCheck = nextQuotaCheck(q, e.model, e.now, e.Record.FallbackWait)
					e.Record.ProbeAfter = e.Record.NextCheck
					e.warning("Usage limit reached. Next check: " + e.Record.NextCheck.Local().Format(time.RFC1123) + ". Use /goal pause to cancel automatic recovery.")
				}
			})
		})
	})
}

func (e *Engine) resumeGoal() {
	if !recoverableThreadStatus(e.status) || len(e.serverRequests) != 0 {
		return
	}
	e.busy = true
	e.Record.RecoveryPending = true
	e.Record.NextCheck = e.now.Add(time.Minute)
	e.save() // Write intent before the side effect.
	if e.err != nil {
		return
	}
	epoch, thread := e.epoch, e.Record.ThreadID
	e.query("thread/goal/set", map[string]any{"threadId": thread, "status": "active"}, func(b json.RawMessage, err *rpcError) {
		if epoch != e.epoch || thread != e.Record.ThreadID {
			return
		}
		e.busy = false
		if err != nil {
			e.queryFailure(err)
			return
		}
		var p struct {
			Goal *Goal `json:"goal"`
		}
		if json.Unmarshal(b, &p) != nil || p.Goal == nil {
			e.failProtocol("invalid goal recovery response")
			return
		}
		e.Record.RecoveryPending = false
		e.applyGoal(p.Goal)
		if p.Goal.Status == "active" {
			e.warning("Quota recovery accepted; continuing the existing goal.")
		}
	})
}

// A quota failure can leave a quiescent thread in systemError. The freshly
// checked usageLimited goal is what authorizes this specific recovery.
func recoverableThreadStatus(status string) bool { return status == "idle" || status == "systemError" }

func (e *Engine) failProtocol(msg string) {
	e.busy = false
	e.Record.ManualHold = true
	e.Record.State = "needs-attention"
	e.warning(msg + "; automatic recovery disabled. This build supports Codex 0.154.0.")
}

func (e *Engine) queryFailure(err *rpcError) {
	msg := strings.ToLower(err.Message)
	if err.Code == -32601 || err.Code == -32602 || strings.Contains(msg, "unauthoriz") || strings.Contains(msg, "authenticat") || strings.Contains(msg, "login") {
		e.Record.ManualHold = true
		e.Record.State = "needs-attention"
		e.warning(err.Message)
		return
	}
	e.Record.NextCheck = e.now.Add(e.retry)
	e.retry *= 2
	if e.retry > time.Minute {
		e.retry = time.Minute
	}
	if e.Record.Message != err.Message {
		e.warning(err.Message)
	} else {
		e.save()
	}
}
