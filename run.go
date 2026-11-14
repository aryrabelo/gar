package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// runner executes one case in the human's terminal. mu guards rec and the
// live child so the signal handler and the step loop never interleave.
type runner struct {
	st       Store
	mu       sync.Mutex
	rec      *Run
	finished bool
	child    *exec.Cmd
	ptmx     *os.File
	buf      *tailBuffer
	step     int
	started  time.Time
	stdinFd  int
	rawState *term.State
}

func cmdRun(st Store, args []string) int {
	fs := newFlags("run")
	yes := fs.Bool("yes", false, "não pergunta [Y/n] (o TTY continua obrigatório)")
	again := fs.Bool("again", false, "roda de novo um caso já terminado (zera os passos)")
	pos, err := parseArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil || len(pos) > 1 {
		fmt.Fprintln(os.Stderr, "gar run: informe no máximo um id")
		return exitUsage
	}
	id := 0
	if len(pos) == 1 {
		if id, err = strconv.Atoi(pos[0]); err != nil {
			fmt.Fprintf(os.Stderr, "gar run: id inválido %q\n", pos[0])
			return exitUsage
		}
	}
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprint(os.Stderr, "gar run: stdin e stdout precisam ser um terminal (TTY).\n"+
			"Este comando é para o humano rodar no terminal dele: ! gar run\n"+
			"Agente: use gar wait <id> para esperar o resultado.\n")
		return exitNoTTY
	}
	picked := false
	if id == 0 {
		runs, err := st.List()
		if err != nil {
			return fail(exitFailed, err)
		}
		ws := detectWorkspace()
		cases := casesIn(runs, ws, StatusPending)
		if len(cases) == 0 {
			where := "nenhum workspace detectado"
			if ws != "" {
				where = ws
			}
			return fail(exitState, fmt.Errorf("nenhum caso pendente (%s); veja gar ls --status pending", where))
		}
		r, err := pickCase(cases, ws)
		if err != nil {
			return fail(exitFailed, err)
		}
		if r == nil {
			fmt.Println("nada rodado")
			return exitCancelled
		}
		id, picked = r.ID, true
		if herdrDialog() {
			labelCase(id)
		}
	}
	rec, err := st.Load(id)
	if err != nil {
		return loadErr(err)
	}
	switch {
	case rec.Status == StatusRunning:
		return fail(exitState, fmt.Errorf("caso %d já está rodando", id))
	case rec.Status != StatusPending && !*again:
		return fail(exitState, fmt.Errorf("caso %d está %s; use gar run %d --again para rodar de novo", id, rec.Status, id))
	}

	rn := &runner{st: st, rec: rec, stdinFd: stdinFd, step: -1}
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	go rn.handleSignals(sigCh)

	printPlan(rec)
	// Enter in the picker already confirmed the case.
	if !*yes && !picked && !confirm("Rodar? [Y/n] ") {
		rn.mu.Lock()
		defer rn.mu.Unlock()
		rn.finished = true
		if rec.Status == StatusPending {
			if _, err := st.Update(id, cancelPending); err != nil {
				return loadErr(err)
			}
			fmt.Printf("caso %d cancelado\n", id)
			return exitCancelled
		}
		fmt.Println("nada rodado")
		return exitCancelled
	}

	// Sequential cases queue behind each other; the picker and [Y/n] hold nothing.
	if !rec.Parallel {
		lock, err := queueRunLock(st, id)
		if err != nil {
			return fail(exitFailed, err)
		}
		defer lock.release()
	}

	// Claim the case under the store lock: a concurrent cancel or run loses.
	rn.mu.Lock()
	rec, err = st.Update(id, func(r *Run) error {
		if *again && r.Terminal() {
			if err := r.Reset(); err != nil {
				return err
			}
		}
		if r.Status != StatusPending {
			return fmt.Errorf("caso %d está %s; não rodei: %w", id, r.Status, errState)
		}
		now := time.Now()
		r.RanAt, r.RunBy, r.RunPID = &now, runnerID(), os.Getpid()
		return r.SetStatus(StatusRunning)
	})
	if err != nil {
		rn.finished = true
		rn.mu.Unlock()
		return loadErr(err)
	}
	rn.rec = rec
	rn.mu.Unlock()

	go rn.forwardStdin()

	exit := 0
	for i := range rec.Steps {
		exit = rn.runStep(i)
		if exit != 0 {
			break
		}
	}

	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.finished = true
	now := time.Now()
	rec.FinishedAt = &now
	final := StatusDone
	for i := range rec.Steps {
		switch rec.Steps[i].Status {
		case StatusFailed:
			final = StatusFailed
		case StatusPending:
			rec.Steps[i].Status = StepSkipped
		}
	}
	if err := rec.SetStatus(final); err != nil {
		return fail(exitFailed, err)
	}
	if err := st.SaveLocked(rec); err != nil {
		return fail(exitFailed, err)
	}
	fmt.Printf("\ncaso %d: %s (exit %d)\n", id, final, exit)
	return exit
}

func cancelPending(r *Run) error {
	if err := r.SetStatus(StatusCancelled); err != nil {
		return err
	}
	now := time.Now()
	r.FinishedAt = &now
	for i := range r.Steps {
		r.Steps[i].Status = StepSkipped
	}
	return nil
}

func printPlan(r *Run) {
	fmt.Printf("caso %d", r.ID)
	if r.CreatedBy != "" {
		fmt.Printf(" (de %s)", r.CreatedBy)
	}
	fmt.Println()
	if r.Parallel {
		fmt.Println("paralelo: roda junto de outros casos, sem fila")
	}
	if r.Why != "" {
		fmt.Println("por quê:", r.Why)
	}
	if r.Expect != "" {
		fmt.Println("espera:", r.Expect)
	}
	if r.Cwd != "" {
		fmt.Println("em:", r.Cwd)
	}
	for i, s := range r.Steps {
		fmt.Printf("  %d. %s\n", i+1, s.Cmd)
	}
}

// confirm reads one line straight from fd 0 (no bufio: leftover bytes would
// be lost to the stdin forwarder). Empty answer means yes.
func confirm(prompt string) bool {
	fmt.Print(prompt)
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if n == 1 {
			if b[0] == '\n' || b[0] == '\r' {
				break
			}
			line = append(line, b[0])
		}
		if err != nil {
			break
		}
	}
	switch strings.ToLower(strings.TrimSpace(string(line))) {
	case "", "y", "yes", "s", "sim":
		return true
	}
	return false
}

// runnerID says who took a case, in terms the human recognizes: the pid, the
// workspace and, inside the Herdr app, the terminal.
func runnerID() string {
	s := fmt.Sprintf("pid %d", os.Getpid())
	if ws := detectWorkspace(); ws != "" {
		s += " · " + ws
	}
	if t := os.Getenv("HC_TERMINAL_ID"); t != "" {
		s += " · terminal " + t
	}
	return s
}

func childShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

func (rn *runner) save() {
	if err := rn.st.SaveLocked(rn.rec); err != nil {
		fmt.Fprintln(os.Stderr, "gar: gravando caso:", err)
	}
}

// runStep runs step i in a PTY wired to the human's terminal and returns its exit code.
func (rn *runner) runStep(i int) int {
	rec := rn.rec
	fmt.Printf("\n▶ [%d/%d] %s\n", i+1, len(rec.Steps), rec.Steps[i].Cmd)

	c := exec.Command(childShell(), "-c", rec.Steps[i].Cmd)
	if fi, err := os.Stat(rec.Cwd); err == nil && fi.IsDir() {
		c.Dir = rec.Cwd
	}
	c.Env = append(os.Environ(), fmt.Sprintf("GAR_RUN_ID=%d", rec.ID))
	buf := &tailBuffer{}

	rn.mu.Lock()
	start := time.Now()
	s := &rec.Steps[i]
	s.Status, s.StartedAt = StatusRunning, &start
	rn.step, rn.started, rn.buf = i, start, buf
	rn.save()
	ptmx, err := pty.Start(c)
	if err != nil {
		fmt.Fprintf(buf, "gar: não consegui iniciar o passo: %v\n", err)
		rn.mu.Unlock()
		return rn.finishStep(127, "")
	}
	_ = pty.InheritSize(os.Stdin, ptmx)
	rn.child, rn.ptmx = c, ptmx
	if st, err := term.MakeRaw(rn.stdinFd); err == nil {
		rn.rawState = st
	}
	rn.mu.Unlock()

	copied := make(chan struct{})
	go func() {
		io.Copy(io.MultiWriter(os.Stdout, buf), ptmx) // EIO once the child side closes
		close(copied)
	}()
	limit := rec.stepTimeout()
	var timedOut atomic.Bool
	timer := time.AfterFunc(limit, func() {
		timedOut.Store(true)
		pgid := c.Process.Pid // pty.Start makes the child a session leader
		syscall.Kill(-pgid, syscall.SIGHUP)
		time.AfterFunc(5*time.Second, func() { syscall.Kill(-pgid, syscall.SIGKILL) })
	})
	c.Wait()
	timer.Stop()
	// Drain what the child wrote; a background grandchild holding the PTY
	// open must not hang us, so give up after a moment.
	select {
	case <-copied:
	case <-time.After(2 * time.Second):
	}

	rn.mu.Lock()
	ptmx.Close()
	rn.restoreTerm()
	rn.child, rn.ptmx = nil, nil
	rn.mu.Unlock()
	if timedOut.Load() {
		msg := fmt.Sprintf("gar: passo estourou o timeout de %s; parei o caso\n", limit)
		fmt.Fprint(os.Stderr, "\n"+msg)
		fmt.Fprint(buf, msg)
		return rn.finishStep(exitTimeout, "timeout")
	}
	return rn.finishStep(exitCode(c.ProcessState), "")
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 127
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func (rn *runner) finishStep(code int, reason string) int {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	s := &rn.rec.Steps[rn.step]
	now := time.Now()
	s.ExitCode, s.FinishedAt = &code, &now
	s.DurationMs = now.Sub(rn.started).Milliseconds()
	s.Output, s.OutputTruncated = rn.buf.Result()
	s.Status, s.Reason = StatusDone, reason
	if code != 0 {
		s.Status = StatusFailed
	}
	rn.save()
	return code
}

func (rn *runner) restoreTerm() {
	if rn.rawState != nil {
		_ = term.Restore(rn.stdinFd, rn.rawState)
		rn.rawState = nil
	}
}

// forwardStdin copies the human's keystrokes to whichever step is running;
// bytes typed between steps are dropped.
func (rn *runner) forwardStdin() {
	b := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(b)
		if n > 0 {
			rn.mu.Lock()
			p := rn.ptmx
			rn.mu.Unlock()
			if p != nil {
				p.Write(b[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

// handleSignals forwards window resizes to the child PTY. SIGINT/SIGTERM
// reaching gar itself (Ctrl-C in raw mode goes to the child instead) mark the
// case cancelled and stop.
func (rn *runner) handleSignals(ch <-chan os.Signal) {
	for sig := range ch {
		if sig == syscall.SIGWINCH {
			rn.mu.Lock()
			if rn.ptmx != nil {
				_ = pty.InheritSize(os.Stdin, rn.ptmx)
			}
			rn.mu.Unlock()
			continue
		}
		rn.cancel(sig.(syscall.Signal))
	}
}

func (rn *runner) cancel(sig syscall.Signal) {
	rn.mu.Lock() // held until exit: the step loop must not save after this
	if rn.finished {
		rn.mu.Unlock()
		return
	}
	if rn.child != nil && rn.child.Process != nil {
		syscall.Kill(-rn.child.Process.Pid, syscall.SIGHUP) // pty.Start makes the child a session leader
	}
	rn.restoreTerm()
	rec := rn.rec
	if rec.Status == StatusPending {
		// Not claimed yet (confirming or queued): another run may own the case
		// by now, so cancel only if the stored case is still pending.
		if _, err := rn.st.Update(rec.ID, cancelPending); err == nil {
			fmt.Fprintf(os.Stderr, "\ngar: caso %d cancelado (%s)\n", rec.ID, sig)
		}
	} else if rec.Status == StatusRunning {
		now := time.Now()
		for i := range rec.Steps {
			s := &rec.Steps[i]
			switch s.Status {
			case StatusRunning:
				s.Status, s.FinishedAt = StatusCancelled, &now
				s.DurationMs = now.Sub(rn.started).Milliseconds()
				if rn.buf != nil {
					s.Output, s.OutputTruncated = rn.buf.Result()
				}
			case StatusPending:
				s.Status = StepSkipped
			}
		}
		rec.FinishedAt = &now
		_ = rec.SetStatus(StatusCancelled)
		rn.save()
		fmt.Fprintf(os.Stderr, "\ngar: caso %d cancelado (%s)\n", rec.ID, sig)
	}
	os.Exit(128 + int(sig))
}
