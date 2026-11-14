package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// startRun launches `gar run <args>` in a real PTY; screen() returns what it printed.
func startRun(t *testing.T, home string, args ...string) (*exec.Cmd, func() string) {
	t.Helper()
	run := exec.Command(garBin, append([]string{"run"}, args...)...)
	run.Env = garEnv(home)
	ptmx, err := pty.StartWithSize(run, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ptmx.Close() })
	var mu sync.Mutex
	var screen bytes.Buffer
	go func() {
		b := make([]byte, 1024)
		for {
			n, err := ptmx.Read(b)
			mu.Lock()
			screen.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return run, func() string { mu.Lock(); defer mu.Unlock(); return screen.String() }
}

func exitOf(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0
}

func showCase(t *testing.T, home string, id int) Run {
	t.Helper()
	out, code := gar(t, home, "show", strconv.Itoa(id), "--json")
	if code != 0 {
		t.Fatalf("show exit %d: %s", code, out)
	}
	var r Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("show --json: %v\n%s", err, out)
	}
	return r
}

// A hung step fails with reason "timeout", the case stops and gar wait returns.
func TestStepTimeoutFailsCase(t *testing.T) {
	home := t.TempDir()
	out, code := gar(t, home, "add", "--timeout", "1s", "--json", "--", "sleep 60", "echo nunca-roda")
	if code != 0 {
		t.Fatalf("add exit %d: %s", code, out)
	}
	var r Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.StepTimeout != "1s" {
		t.Fatalf("step_timeout %q, quer 1s", r.StepTimeout)
	}

	wait := exec.Command(garBin, "wait", strconv.Itoa(r.ID), "--timeout", "30s")
	wait.Env = garEnv(home)
	if err := wait.Start(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	run, screen := startRun(t, home, strconv.Itoa(r.ID), "--yes")
	if c := exitOf(t, run.Wait()); c != exitTimeout {
		t.Fatalf("gar run exit %d, quer %d; tela: %q", c, exitTimeout, screen())
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("run levou %s; o timeout de 1s não parou o passo", d)
	}
	if c := exitOf(t, wait.Wait()); c != exitFailed {
		t.Fatalf("gar wait exit %d, quer %d", c, exitFailed)
	}
	got := showCase(t, home, r.ID)
	s0, s1 := got.Steps[0], got.Steps[1]
	if got.Status != StatusFailed || s0.Status != StatusFailed || s0.Reason != "timeout" ||
		s0.ExitCode == nil || *s0.ExitCode != exitTimeout || !strings.Contains(s0.Output, "timeout") {
		t.Fatalf("caso %s, passo 1: %+v", got.Status, s0)
	}
	if s1.Status != StepSkipped || strings.Contains(screen(), "\nnunca-roda") {
		t.Fatalf("passo 2 devia ser pulado: %+v", s1)
	}
}

// A second gar run while one is active refuses at once, naming the case ahead.
func TestConcurrentRunRefuses(t *testing.T) {
	home := t.TempDir()
	busy := addCase(t, home, "sleep 3")
	next := addCase(t, home, "echo segundo-rodou")

	first, _ := startRun(t, home, strconv.Itoa(busy.ID), "--yes")
	deadline := time.Now().Add(10 * time.Second)
	for showCase(t, home, busy.ID).Status != StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("primeiro run não começou")
		}
		time.Sleep(20 * time.Millisecond)
	}

	second, screen := startRun(t, home, strconv.Itoa(next.ID), "--yes")
	if c := exitOf(t, second.Wait()); c != exitState {
		t.Fatalf("segundo run exit %d, quer %d; tela: %q", c, exitState, screen())
	}
	want := "outro gar run está ativo (caso " + strconv.Itoa(busy.ID) + ", há "
	if !strings.Contains(screen(), want) {
		t.Fatalf("tela sem %q: %q", want, screen())
	}
	if got := showCase(t, home, next.ID); got.Status != StatusPending {
		t.Fatalf("segundo caso %s, devia seguir pending", got.Status)
	}
	if c := exitOf(t, first.Wait()); c != 0 {
		t.Fatalf("primeiro run exit %d", c)
	}
	// Lock released: the queued case runs now.
	again, scr := startRun(t, home, strconv.Itoa(next.ID), "--yes")
	if c := exitOf(t, again.Wait()); c != 0 || !strings.Contains(scr(), "segundo-rodou") {
		t.Fatalf("depois do primeiro, exit %d; tela: %q", c, scr())
	}
}
