package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	out, code := gar(t, home, "add", "--why", "teste", "--timeout", "1s", "--json", "--", "sleep 60", "echo nunca-roda")
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

// waitStatus polls until case id reaches status.
func waitStatus(t *testing.T, home string, id int, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for showCase(t, home, id).Status != status {
		if time.Now().After(deadline) {
			t.Fatalf("caso %d não chegou a %s", id, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func addParallel(t *testing.T, home string, steps ...string) *Run {
	t.Helper()
	out, code := gar(t, home, append([]string{"add", "--why", "teste", "--parallel", "--json", "--"}, steps...)...)
	if code != 0 {
		t.Fatalf("add exit %d: %s", code, out)
	}
	var r Run
	if err := json.Unmarshal([]byte(out), &r); err != nil || !r.Parallel {
		t.Fatalf("add --parallel: %v %+v", err, r)
	}
	return &r
}

// A second sequential run waits in the queue, naming the case ahead, and runs after it.
func TestSequentialRunQueues(t *testing.T) {
	home := t.TempDir()
	busy := addCase(t, home, "sleep 2")
	next := addCase(t, home, "echo segundo-rodou")

	first, _ := startRun(t, home, strconv.Itoa(busy.ID), "--yes")
	waitStatus(t, home, busy.ID, StatusRunning)

	second, screen := startRun(t, home, strconv.Itoa(next.ID), "--yes")
	want := "na fila atrás do caso " + strconv.Itoa(busy.ID) + " (há "
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(screen(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("tela sem %q: %q", want, screen())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := showCase(t, home, next.ID); got.Status != StatusPending {
		t.Fatalf("caso na fila está %s com o outro rodando", got.Status)
	}
	if c := exitOf(t, first.Wait()); c != 0 {
		t.Fatalf("primeiro run exit %d", c)
	}
	if c := exitOf(t, second.Wait()); c != 0 || !strings.Contains(screen(), "\nsegundo-rodou") {
		t.Fatalf("segundo run exit %d; tela: %q", c, screen())
	}
	a, b := showCase(t, home, busy.ID), showCase(t, home, next.ID)
	if b.Status != StatusDone || b.RanAt == nil || a.FinishedAt == nil || b.RanAt.Before(*a.FinishedAt) {
		t.Fatalf("segundo devia rodar depois do primeiro: A fim %v, B %s início %v", a.FinishedAt, b.Status, b.RanAt)
	}
}

// A --parallel case skips the queue and finishes while a sequential one still runs.
func TestParallelRunSkipsQueue(t *testing.T) {
	home := t.TempDir()
	busy := addCase(t, home, "sleep 5")
	par := addParallel(t, home, "echo par-ok")

	first, _ := startRun(t, home, strconv.Itoa(busy.ID), "--yes")
	waitStatus(t, home, busy.ID, StatusRunning)

	second, screen := startRun(t, home, strconv.Itoa(par.ID), "--yes")
	if c := exitOf(t, second.Wait()); c != 0 || strings.Contains(screen(), "na fila") ||
		!strings.Contains(screen(), "\npar-ok") {
		t.Fatalf("paralelo exit %d; tela: %q", c, screen())
	}
	if got := showCase(t, home, busy.ID).Status; got != StatusRunning {
		t.Fatalf("o sequencial devia seguir running, está %s", got)
	}
	ls, _ := gar(t, home, "ls")
	want := regexp.MustCompile(`(?m)^` + strconv.Itoa(busy.ID) + `\s+running\s+pid \d+ · ws-teste\s`)
	if !want.MatchString(ls) || !regexp.MustCompile(`(?m)^`+strconv.Itoa(par.ID)+`\s+done\s+-\s`).MatchString(ls) {
		t.Fatalf("ls não mostra quem roda: %s", ls)
	}
	if out, _ := gar(t, home, "show", strconv.Itoa(par.ID)); !strings.Contains(out, "paralelo: roda junto") {
		t.Fatalf("show não diz que é paralelo: %s", out)
	}
	if c := exitOf(t, first.Wait()); c != 0 {
		t.Fatalf("primeiro run exit %d", c)
	}
}

// An open picker holds no lock: another gar run <id> runs right away.
func TestPickerHoldsNothing(t *testing.T) {
	home := t.TempDir()
	addCase(t, home, "echo no-seletor")
	other := addCase(t, home, "echo outro-rodou")

	picker, pickScreen := startRun(t, home)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(pickScreen(), "Enter roda") {
		if time.Now().After(deadline) {
			t.Fatalf("sem seletor; tela: %q", pickScreen())
		}
		time.Sleep(20 * time.Millisecond)
	}
	run, screen := startRun(t, home, strconv.Itoa(other.ID), "--yes")
	if c := exitOf(t, run.Wait()); c != 0 || strings.Contains(screen(), "na fila") {
		t.Fatalf("run com seletor aberto: exit %d; tela: %q", c, screen())
	}
	picker.Process.Kill()
	picker.Wait()
}

// Bare gar wait waits for this workspace's running cases; none running is exit 4.
func TestBareWaitWaitsRunning(t *testing.T) {
	home := t.TempDir()
	if out, code := gar(t, home, "wait"); code != exitState {
		t.Fatalf("wait sem running: exit %d, quer %d: %s", code, exitState, out)
	}
	// Running in another workspace: must not be waited for.
	elsewhere := addIn(t, home, "outro", "sleep 30")
	far, _ := startRun(t, home, strconv.Itoa(elsewhere), "--yes")
	t.Cleanup(func() { far.Process.Kill(); far.Wait() })
	waitStatus(t, home, elsewhere, StatusRunning)

	mine := addParallel(t, home, "sleep 1", "echo fim-do-caso")
	run, _ := startRun(t, home, strconv.Itoa(mine.ID), "--yes")
	waitStatus(t, home, mine.ID, StatusRunning)

	out, code := gar(t, home, "wait", "--timeout", "20s")
	if c := exitOf(t, run.Wait()); c != 0 {
		t.Fatalf("run exit %d", c)
	}
	var r Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("wait devia imprimir um objeto: %v\n%s", err, out)
	}
	if code != 0 || r.ID != mine.ID || r.Status != StatusDone || !strings.Contains(r.Steps[1].Output, "fim-do-caso") {
		t.Fatalf("wait exit %d, caso %d %s", code, r.ID, r.Status)
	}
}

// A live run can't be cancelled; once its gar run dies (SIGKILL), cancel clears the orphan.
func TestCancelOrphanRunning(t *testing.T) {
	home := t.TempDir()
	r := addCase(t, home, "sleep 30", "echo nunca")
	run, _ := startRun(t, home, strconv.Itoa(r.ID), "--yes")
	waitStatus(t, home, r.ID, StatusRunning)
	if out, code := gar(t, home, "cancel", strconv.Itoa(r.ID)); code != exitState {
		t.Fatalf("cancel com run vivo: exit %d: %s", code, out)
	}
	run.Process.Kill()
	run.Wait()
	if out, code := gar(t, home, "cancel", strconv.Itoa(r.ID)); code != 0 {
		t.Fatalf("cancel do órfão: exit %d: %s", code, out)
	}
	got := showCase(t, home, r.ID)
	if got.Status != StatusCancelled || got.Steps[0].Reason != "orphan" || got.Steps[1].Status != StepSkipped {
		t.Fatalf("órfão: %+v", got)
	}
}

// Closing the terminal (SIGHUP) marks the case cancelled instead of leaving it running.
func TestHangupCancelsCase(t *testing.T) {
	home := t.TempDir()
	r := addCase(t, home, "sleep 30")
	run, _ := startRun(t, home, strconv.Itoa(r.ID), "--yes")
	waitStatus(t, home, r.ID, StatusRunning)
	run.Process.Signal(syscall.SIGHUP)
	if c := exitOf(t, run.Wait()); c != 128+int(syscall.SIGHUP) {
		t.Fatalf("exit %d", c)
	}
	if got := showCase(t, home, r.ID); got.Status != StatusCancelled {
		t.Fatalf("caso %s depois do SIGHUP", got.Status)
	}
}
