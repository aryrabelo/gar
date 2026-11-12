package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

var garBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gar-bin-")
	if err != nil {
		panic(err)
	}
	garBin = filepath.Join(dir, "gar")
	build := exec.Command("go", "build", "-o", garBin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func garEnv(home string) []string {
	return append(os.Environ(), "GAR_HOME="+home, "SHELL=/bin/sh", "GAR_FROM=teste")
}

// gar runs the binary without a TTY and returns stdout and the exit code.
func gar(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	c := exec.Command(garBin, args...)
	c.Env = garEnv(home)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String() + errb.String(), code
}

func addCase(t *testing.T, home string, steps ...string) *Run {
	t.Helper()
	out, code := gar(t, home, append([]string{"add", "--why", "teste", "--json", "--"}, steps...)...)
	if code != 0 {
		t.Fatalf("add exit %d: %s", code, out)
	}
	var r Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("add --json: %v\n%s", err, out)
	}
	return &r
}

func TestConcurrentAddsGetUniqueIDs(t *testing.T) {
	home := t.TempDir()
	ids := make(chan int, 20)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			ids <- addCase(t, home, fmt.Sprintf("echo %d", i)).ID
		})
	}
	wg.Wait()
	close(ids)
	seen := map[int]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("id %d repetido", id)
		}
		seen[id] = true
	}
	for id := 1001; id <= 1020; id++ {
		if !seen[id] {
			t.Fatalf("faltou o id %d (vistos %v)", id, seen)
		}
	}
}

func TestRunRefusesWithoutTTY(t *testing.T) {
	home := t.TempDir()
	r := addCase(t, home, "touch nao-devia-rodar")
	out, code := gar(t, home, "run", strconv.Itoa(r.ID), "--yes")
	if code != exitNoTTY {
		t.Fatalf("exit %d, quer %d: %s", code, exitNoTTY, out)
	}
	if !strings.Contains(out, "TTY") {
		t.Fatalf("mensagem sem explicação: %s", out)
	}
	got, _ := (Store{Home: home}).Load(r.ID)
	if got.Status != StatusPending {
		t.Fatalf("status %s depois da recusa", got.Status)
	}
}

func TestCancelAndWaitTimeout(t *testing.T) {
	home := t.TempDir()
	r := addCase(t, home, "true")
	id := strconv.Itoa(r.ID)
	if _, code := gar(t, home, "wait", id, "--timeout", "600ms"); code != exitTimeout {
		t.Fatalf("wait em pending: exit %d, quer %d", code, exitTimeout)
	}
	if out, code := gar(t, home, "cancel", id); code != 0 {
		t.Fatalf("cancel: %d %s", code, out)
	}
	if _, code := gar(t, home, "cancel", id); code != exitState {
		t.Fatalf("cancel repetido: exit %d, quer %d", code, exitState)
	}
	if _, code := gar(t, home, "wait", id); code != exitCancelled {
		t.Fatalf("wait em cancelled: exit %d", code)
	}
}

// The human flow: gar run in a real PTY, answer y, a passing step then a
// failing one stops the sequence, while gar wait polls in parallel.
func TestRunInPTYWithParallelWait(t *testing.T) {
	home := t.TempDir()
	r := addCase(t, home, "printf '\\033[32mpasso-ok\\033[0m\\n'", "echo falhou >&2; exit 3", "echo nunca-roda")
	id := strconv.Itoa(r.ID)

	wait := exec.Command(garBin, "wait", id, "--timeout", "30s")
	wait.Env = garEnv(home)
	var waitOut bytes.Buffer
	wait.Stdout = &waitOut
	if err := wait.Start(); err != nil {
		t.Fatal(err)
	}

	run := exec.Command(garBin, "run", id)
	run.Env = garEnv(home)
	ptmx, err := pty.StartWithSize(run, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		ready := strings.Contains(screen.String(), "[y/N]")
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sem prompt [y/N]; tela: %q", screen.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	ptmx.Write([]byte("y\r"))

	runErr := run.Wait()
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("gar run: %v; quer exit 3 (o do passo que falhou)", runErr)
	}
	err = wait.Wait()
	if !errors.As(err, &ee) || ee.ExitCode() != exitFailed {
		t.Fatalf("gar wait: %v; quer exit 1", err)
	}

	var got Run
	if err := json.Unmarshal(waitOut.Bytes(), &got); err != nil {
		t.Fatalf("JSON do wait: %v\n%s", err, waitOut.String())
	}
	if got.Status != StatusFailed || got.RanAt == nil || got.FinishedAt == nil {
		t.Fatalf("caso: status=%s ran_at=%v finished_at=%v", got.Status, got.RanAt, got.FinishedAt)
	}
	s0, s1, s2 := got.Steps[0], got.Steps[1], got.Steps[2]
	if s0.Status != StatusDone || s0.ExitCode == nil || *s0.ExitCode != 0 || s0.Output != "passo-ok\n" {
		t.Fatalf("passo 1: %+v", s0)
	}
	if s1.Status != StatusFailed || s1.ExitCode == nil || *s1.ExitCode != 3 || !strings.Contains(s1.Output, "falhou") {
		t.Fatalf("passo 2: %+v", s1)
	}
	if s2.Status != StepSkipped || s2.ExitCode != nil || s2.StartedAt != nil {
		t.Fatalf("passo 3 devia ser pulado: %+v", s2)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(screen.String(), "\nnunca-roda") {
		t.Fatal("passo 3 rodou")
	}
	if !strings.Contains(screen.String(), "passo-ok") {
		t.Fatalf("saída do passo não chegou ao terminal: %q", screen.String())
	}
}
