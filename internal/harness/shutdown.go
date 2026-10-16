package harness

import (
	"context"
	"errors"
	"time"
)

// PauseManagedGoal runs only after the bridge has joined, so it cannot race
// the actor's record writes. It never resumes a thread, creates a goal, or
// touches the daemon lifecycle. A replaced goal belongs to its other client.
func PauseManagedGoal(ctx context.Context, r *Record) error {
	if r.Goal == nil || r.ThreadID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rpc, err := connectShared(ctx, r.CodexBin, r.CodexHome)
	if err != nil {
		return err
	}
	defer rpc.Close()
	var current struct {
		Goal *Goal `json:"goal"`
	}
	if err := rpc.Call(ctx, "thread/goal/get", map[string]any{"threadId": r.ThreadID}, &current); err != nil {
		return err
	}
	if !sameGoal(r.Goal, current.Goal) || (current.Goal.Status != "active" && !(r.RecoveryPending && current.Goal.Status == "usageLimited")) {
		return nil
	}
	var paused struct {
		Goal *Goal `json:"goal"`
	}
	if err := rpc.Call(ctx, "thread/goal/set", map[string]any{"threadId": r.ThreadID, "status": "paused"}, &paused); err != nil {
		return err
	}
	if !sameGoal(current.Goal, paused.Goal) || paused.Goal.Status != "paused" {
		return errors.New("Codex did not confirm the managed goal was paused")
	}
	r.Goal, r.State, r.ManualHold, r.RecoveryPending = paused.Goal, "paused", true, false
	r.NextCheck, r.ProbeAfter = time.Time{}, time.Time{}
	var thread struct {
		Thread struct {
			ID          string `json:"id"`
			HistoryMode string `json:"historyMode"`
			Status      struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns []shutdownTurn `json:"turns"`
		} `json:"thread"`
	}
	if err := rpc.Call(ctx, "thread/read", map[string]any{"threadId": r.ThreadID}, &thread); err != nil {
		return err
	}
	if thread.Thread.ID != r.ThreadID {
		return errors.New("Codex returned a different thread during shutdown")
	}
	if thread.Thread.Status.Type != "active" {
		return nil
	}
	var turns []shutdownTurn
	if thread.Thread.HistoryMode == "paginated" {
		var page struct {
			Data []shutdownTurn `json:"data"`
		}
		if err := rpc.Call(ctx, "thread/turns/list", map[string]any{"threadId": r.ThreadID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &page); err != nil {
			return err
		}
		turns = page.Data
	} else {
		if err := rpc.Call(ctx, "thread/read", map[string]any{"threadId": r.ThreadID, "includeTurns": true}, &thread); err != nil {
			return err
		}
		turns = thread.Thread.Turns
	}
	for _, turn := range turns {
		if turn.Status == "inProgress" && turn.ID != "" {
			return rpc.Call(ctx, "turn/interrupt", map[string]any{"threadId": r.ThreadID, "turnId": turn.ID}, nil)
		}
	}
	return errors.New("goal paused, but its active turn could not be identified for interruption")
}

type shutdownTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
