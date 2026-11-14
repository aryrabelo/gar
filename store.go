package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Status of a case (Run). Steps reuse these plus StepSkipped.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StepSkipped     = "skipped"
)

const firstID = 1001

type Step struct {
	Cmd             string     `json:"cmd"`
	Status          string     `json:"status"`
	ExitCode        *int       `json:"exit_code"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	DurationMs      int64      `json:"duration_ms"`
	Output          string     `json:"output"`
	OutputTruncated bool       `json:"output_truncated"`
	// Reason says why a failed step stopped when it was not its own exit ("timeout").
	Reason string `json:"reason,omitempty"`
}

type Run struct {
	ID        int        `json:"id"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	CreatedBy string     `json:"created_by"`
	Why       string     `json:"why"`
	Expect    string     `json:"expect"`
	Cwd       string     `json:"cwd"`
	Host      string     `json:"host"`
	Workspace string     `json:"workspace,omitempty"`
	RanAt     *time.Time `json:"ran_at"`
	// RunBy names who took the case: pid, workspace and Herdr terminal.
	RunBy string `json:"run_by,omitempty"`
	// RunPID is the gar run process that owns a running case (0 in old cases).
	RunPID     int        `json:"run_pid,omitempty"`
	FinishedAt *time.Time `json:"finished_at"`
	// StepTimeout caps each step (Go duration, e.g. "10m"); empty = defaultStepTimeout.
	StepTimeout string `json:"step_timeout,omitempty"`
	// Parallel cases skip the run queue and run alongside any other case.
	Parallel bool `json:"parallel,omitempty"`
	// Queued is set while a gar run waits for run.lock to start this case.
	Queued *Queued `json:"queued,omitempty"`
	Steps  []Step  `json:"steps"`
}

// Queued says which gar run waits in the run queue for a case, and behind whom.
type Queued struct {
	PID int `json:"pid"`
	// Behind is the case holding run.lock (0 when the lock does not say).
	Behind int       `json:"behind"`
	Since  time.Time `json:"since"`
}

// queuedLive returns the queue mark of a pending case whose waiting gar run
// still exists; a mark left by a killed run counts as absent.
func (r *Run) queuedLive() *Queued {
	if r.Status != StatusPending || r.Queued == nil || !runnerAlive(r.Queued.PID) {
		return nil
	}
	return r.Queued
}

func (q *Queued) behindText() string {
	if q.Behind == 0 {
		return "na fila atrás de outro gar run"
	}
	return fmt.Sprintf("na fila atrás do %d", q.Behind)
}

var (
	errNotFound = errors.New("caso não encontrado")
	// errState marks a refusal because of the case's current status (exit 4).
	errState = errors.New("estado do caso não permite")
)

var transitions = map[string][]string{
	StatusPending: {StatusRunning, StatusCancelled},
	StatusRunning: {StatusDone, StatusFailed, StatusCancelled},
}

// SetStatus moves the case along pending → running → done|failed|cancelled
// (or pending → cancelled). Anything else is refused.
func (r *Run) SetStatus(to string) error {
	for _, ok := range transitions[r.Status] {
		if ok == to {
			r.Status = to
			return nil
		}
	}
	return fmt.Errorf("caso %d: transição %s → %s não permitida: %w", r.ID, r.Status, to, errState)
}

// Terminal reports whether the case has finished (wait stops here).
func (r *Run) Terminal() bool {
	return r.Status == StatusDone || r.Status == StatusFailed || r.Status == StatusCancelled
}

// Reset puts a finished case back to pending with zeroed steps (gar run --again).
func (r *Run) Reset() error {
	if !r.Terminal() {
		return fmt.Errorf("caso %d está %s; --again só vale para caso terminado: %w", r.ID, r.Status, errState)
	}
	r.Status = StatusPending
	r.RanAt, r.FinishedAt, r.RunBy, r.RunPID = nil, nil, "", 0
	for i := range r.Steps {
		r.Steps[i] = Step{Cmd: r.Steps[i].Cmd, Status: StatusPending}
	}
	return nil
}

type Store struct{ Home string }

func DefaultHome() (string, error) {
	if h := os.Getenv("GAR_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "gar"), nil
}

func (s Store) runsDir() string    { return filepath.Join(s.Home, "runs") }
func (s Store) path(id int) string { return filepath.Join(s.runsDir(), strconv.Itoa(id)+".json") }

func (s Store) Init() error { return os.MkdirAll(s.runsDir(), 0o700) }

// flock opens (creating) name under Home and holds an exclusive flock until
// the returned func is called. flock is per open file description, so it
// serialises goroutines of one process as well as separate processes.
func (s Store) flock(name string) (*os.File, func(), error) {
	f, err := os.OpenFile(filepath.Join(s.Home, name), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// NextID hands out sequential IDs from 1001, under flock on the counter file.
func (s Store) NextID() (int, error) {
	f, unlock, err := s.flock("counter")
	if err != nil {
		return 0, err
	}
	defer unlock()
	b, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	next := firstID
	if t := strings.TrimSpace(string(b)); t != "" {
		last, err := strconv.Atoi(t)
		if err != nil {
			return 0, fmt.Errorf("counter corrompido (%q): %w", t, err)
		}
		next = last + 1
	}
	// A lost counter must never reuse an existing case.
	for {
		if _, err := os.Stat(s.path(next)); errors.Is(err, os.ErrNotExist) {
			break
		}
		next++
	}
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(next)+"\n"), 0); err != nil {
		return 0, err
	}
	return next, f.Sync()
}

// Create assigns an ID and writes the case.
func (s Store) Create(r *Run) error {
	id, err := s.NextID()
	if err != nil {
		return err
	}
	r.ID = id
	return s.Save(r)
}

// marshalRun renders a case as indented JSON without HTML escaping, so
// commands like `2>&1` stay readable.
func marshalRun(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return buf.Bytes(), err
}

// Save writes the case atomically: temp file in the same dir, fsync, rename.
func (s Store) Save(r *Run) error {
	b, err := marshalRun(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.runsDir(), fmt.Sprintf(".%d-*.tmp", r.ID))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(r.ID))
}

func (s Store) Load(id int) (*Run, error) {
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("caso %d: %w", id, errNotFound)
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("caso %d: JSON inválido: %w", id, err)
	}
	return &r, nil
}

// Update loads, mutates and saves a case under the store-wide lock, so a
// cancel and a run starting on the same case cannot both win.
func (s Store) Update(id int, fn func(*Run) error) (*Run, error) {
	_, unlock, err := s.flock("lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if err := fn(r); err != nil {
		return r, err
	}
	return r, s.Save(r)
}

// SaveLocked saves an in-memory case under the store-wide lock.
func (s Store) SaveLocked(r *Run) error {
	_, unlock, err := s.flock("lock")
	if err != nil {
		return err
	}
	defer unlock()
	return s.Save(r)
}

// List returns every case, newest first.
func (s Store) List() ([]*Run, error) {
	paths, err := filepath.Glob(filepath.Join(s.runsDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var runs []*Run
	for _, p := range paths {
		id, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil {
			continue
		}
		r, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID > runs[j].ID })
	return runs, nil
}
