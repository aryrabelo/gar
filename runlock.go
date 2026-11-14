package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const defaultStepTimeout = 10 * time.Minute

// stepTimeout is the per-step limit; cases recorded before the field existed get the default.
func (r *Run) stepTimeout() time.Duration {
	if d, err := time.ParseDuration(r.StepTimeout); err == nil && d > 0 {
		return d
	}
	return defaultStepTimeout
}

// runLock is held for the whole life of one gar run, so a second run says who
// is ahead of it instead of piling up silently. The kernel drops the flock if
// the process dies, so a crash never leaves the queue stuck.
type runLock struct {
	f     *os.File
	owner runOwner
}

type runOwner struct {
	PID     int       `json:"pid"`
	Case    int       `json:"case"`
	Started time.Time `json:"started"`
}

func acquireRunLock(st Store) (*runLock, error) {
	f, err := os.OpenFile(filepath.Join(st.Home, "run.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		b, _ := io.ReadAll(f)
		return nil, busyErr(b)
	}
	l := &runLock{f: f, owner: runOwner{PID: os.Getpid(), Started: time.Now()}}
	return l, l.write()
}

func busyErr(b []byte) error {
	var o runOwner
	if json.Unmarshal(b, &o) != nil {
		return fmt.Errorf("outro gar run está ativo; não rodei: %w", errState)
	}
	what := "escolhendo caso"
	if o.Case != 0 {
		what = fmt.Sprintf("caso %d", o.Case)
	}
	return fmt.Errorf("outro gar run está ativo (%s, há %s, pid %d); espere ele terminar e rode de novo: %w",
		what, time.Since(o.Started).Round(time.Second), o.PID, errState)
}

// claim records which case this run took.
func (l *runLock) claim(id int) error {
	l.owner.Case = id
	return l.write()
}

func (l *runLock) write() error {
	b, err := json.Marshal(l.owner)
	if err != nil {
		return err
	}
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err = l.f.WriteAt(b, 0)
	return err
}

func (l *runLock) release() {
	l.f.Truncate(0)
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
