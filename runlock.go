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

// runLock serializes sequential cases: it is taken only after the human chose
// and confirmed a case, and held while that case runs. A second sequential run
// says who is ahead of it and waits. The kernel drops the flock if the process
// dies, so a crash never leaves the queue stuck.
type runLock struct {
	f *os.File
}

type runOwner struct {
	PID     int       `json:"pid"`
	Case    int       `json:"case"`
	Started time.Time `json:"started"`
}

// queueRunLock takes the run lock for case id, blocking behind the current
// holder (after printing who it is) until it is free.
func queueRunLock(st Store, id int) (*runLock, error) {
	f, err := os.OpenFile(filepath.Join(st.Home, "run.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		b, _ := io.ReadAll(f)
		fmt.Fprintln(os.Stderr, queueLine(b))
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, err
		}
	}
	l := &runLock{f: f}
	b, err := json.Marshal(runOwner{PID: os.Getpid(), Case: id, Started: time.Now()})
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.WriteAt(b, 0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gar: gravando run.lock:", err)
	}
	return l, nil
}

func queueLine(b []byte) string {
	var o runOwner
	if json.Unmarshal(b, &o) != nil || o.Case == 0 {
		return "gar: na fila atrás de outro gar run; espera ele terminar (Ctrl-C desiste)"
	}
	return fmt.Sprintf("gar: na fila atrás do caso %d (há %s, pid %d); espera ele terminar (Ctrl-C desiste)",
		o.Case, time.Since(o.Started).Round(time.Second), o.PID)
}

func (l *runLock) release() {
	l.f.Truncate(0)
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
