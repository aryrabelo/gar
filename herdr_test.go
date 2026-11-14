package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeHerdr puts herdr-app and hc on PATH that append their args to <name>.log in dir.
func fakeHerdr(t *testing.T) (dir string, env []string) {
	t.Helper()
	dir = t.TempDir()
	for _, name := range []string{"herdr-app", "hc"} {
		script := "#!/bin/sh\necho \"$*\" >> " + filepath.Join(dir, name+".log") + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "HC_TERMINAL_ID=t-teste"}
}

func readLog(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name+".log"))
	return string(b)
}

func waitFor(t *testing.T, what string, cond func() bool, screen func() string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("sem %s; tela: %q", what, screen())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// In the Herdr dialog, Esc hides it and the picker keeps waiting; Enter then
// runs the case and labels the hc terminal with it.
func TestPickerEscHidesHerdrDialog(t *testing.T) {
	home := t.TempDir()
	dir, env := fakeHerdr(t)
	id := addCase(t, home, "echo rodou").ID

	run, ptmx, screen := startRunEnv(t, home, append(env, "GAR_HERDR_DIALOG=1"))
	waitFor(t, "seletor", func() bool { return strings.Contains(screen(), "Esc esconde · q sai") }, screen)
	ptmx.Write([]byte("\x1b"))
	waitFor(t, "overlay.close", func() bool { return readLog(dir, "herdr-app") == "do overlay.close\n" }, screen)

	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("seletor saiu no Esc: %v; tela: %q", err, screen())
	case <-time.After(300 * time.Millisecond):
	}
	if got := readLog(dir, "hc"); got != "" {
		t.Fatalf("hc antes do Enter: %q", got)
	}

	ptmx.Write([]byte("\r"))
	select {
	case err := <-done:
		if code := exitOf(t, err); code != 0 {
			t.Fatalf("gar run exit %d; tela: %q", code, screen())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("gar run não terminou; tela: %q", screen())
	}
	if want := "label t-teste gar.case=" + strconv.Itoa(id) + "\n"; readLog(dir, "hc") != want {
		t.Fatalf("hc recebeu %q, quer %q", readLog(dir, "hc"), want)
	}
	if r := showCase(t, home, id); r.Status != StatusDone {
		t.Fatalf("caso %s, quer done", r.Status)
	}
}

// Without GAR_HERDR_DIALOG=1, Esc gives up as before and touches neither tool.
func TestPickerEscExitsOutsideHerdrDialog(t *testing.T) {
	home := t.TempDir()
	dir, env := fakeHerdr(t)
	id := addCase(t, home, "echo nunca").ID

	run, ptmx, screen := startRunEnv(t, home, append(env, "GAR_HERDR_DIALOG="))
	waitFor(t, "seletor", func() bool { return strings.Contains(screen(), "Enter roda · Esc sai") }, screen)
	ptmx.Write([]byte("\x1b"))
	if code := exitOf(t, run.Wait()); code != exitCancelled {
		t.Fatalf("exit %d, quer %d; tela: %q", code, exitCancelled, screen())
	}
	if got := readLog(dir, "herdr-app") + readLog(dir, "hc"); got != "" {
		t.Fatalf("ferramentas chamadas sem GAR_HERDR_DIALOG: %q", got)
	}
	if r := showCase(t, home, id); r.Status != StatusPending {
		t.Fatalf("caso %s, quer pending", r.Status)
	}
}
