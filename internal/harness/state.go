package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
)

const StateVersion = 1

type Goal struct {
	ThreadID        string `json:"threadId"`
	Objective       string `json:"objective"`
	Status          string `json:"status"`
	TokenBudget     *int64 `json:"tokenBudget"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

func sameGoal(a, b *Goal) bool {
	return a != nil && b != nil && a.ThreadID == b.ThreadID && a.CreatedAt == b.CreatedAt && a.Objective == b.Objective
}

type Record struct {
	Version         int           `json:"version"`
	ID              string        `json:"id"`
	CWD             string        `json:"cwd"`
	CodexBin        string        `json:"codexBin"`
	CodexHome       string        `json:"codexHome,omitempty"`
	CodexArgs       []string      `json:"codexArgs,omitempty"`
	FallbackWait    time.Duration `json:"fallbackWait"`
	ThreadID        string        `json:"threadId,omitempty"`
	Goal            *Goal         `json:"goal,omitempty"`
	State           string        `json:"state"`
	NextCheck       time.Time     `json:"nextCheck,omitempty"`
	ProbeAfter      time.Time     `json:"probeAfter,omitempty"`
	RecoveryPending bool          `json:"recoveryPending"`
	ManualHold      bool          `json:"manualHold"`
	Message         string        `json:"message,omitempty"`
	UpdatedAt       time.Time     `json:"updatedAt"`
	StoppedAt       time.Time     `json:"stoppedAt,omitempty"`
}

type Store struct{ Dir string }

func DefaultStore() (Store, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Store{}, err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(dir) {
		return Store{}, errors.New("XDG_STATE_HOME must be absolute")
	}
	return Store{Dir: filepath.Join(dir, "tkmax", "runs")}, nil
}

func NewRecord(cwd, bin string, args []string, fallback time.Duration) (*Record, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Record{Version: StateVersion, ID: hex.EncodeToString(b), CWD: cwd, CodexBin: bin, CodexArgs: args, FallbackWait: fallback, State: "idle"}, nil
}

var validID = regexp.MustCompile(`^[a-f0-9]{24}$`)

func (s Store) path(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("invalid run ID %q", id)
	}
	return filepath.Join(s.Dir, id+".json"), nil
}

func (s Store) Save(r *Record) error {
	path, err := s.path(r.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s Store) Load(id string) (*Record, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("read run %s: %w", id, err)
	}
	if r.Version != StateVersion {
		return nil, fmt.Errorf("unsupported state version %d", r.Version)
	}
	if r.ID != id || !filepath.IsAbs(r.CWD) || r.FallbackWait <= 0 {
		return nil, errors.New("invalid saved run")
	}
	return &r, nil
}

func (s Store) Latest(cwd string) (*Record, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var records []*Record
	for _, f := range files {
		id := filepath.Base(f)
		id = id[:len(id)-5]
		r, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		if r.CWD == cwd && r.ThreadID != "" && (r.Goal == nil || r.Goal.Status != "complete") {
			records = append(records, r)
		}
	}
	if len(records) == 0 {
		return nil, errors.New("no unfinished tkmax run in this directory; specify a run ID or start tkmax")
	}
	sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt.After(records[j].UpdatedAt) })
	return records[0], nil
}

// Lock is process-scoped and automatically released even after SIGKILL.
func (s Store) Lock(id string) (*os.File, error) {
	if _, err := s.path(id); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("run %s is already open in another wrapper: %w", id, err)
	}
	return f, nil
}
