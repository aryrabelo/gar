package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// casesIn returns the workspace's cases in status, oldest first (FIFO).
// Without a workspace every case in status qualifies.
func casesIn(runs []*Run, ws, status string) []*Run {
	var out []*Run
	for i := len(runs) - 1; i >= 0; i-- { // List is newest first
		r := runs[i]
		if r.Status == status && (ws == "" || r.Workspace == ws) {
			out = append(out, r)
		}
	}
	return out
}

// pickCase lets the human choose a case with ↑/↓ (or k/j) and Enter; Esc, q
// and Ctrl-C give up. The oldest case starts selected. stdin must be a TTY.
func pickCase(cases []*Run, ws string) (*Run, error) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	defer term.Restore(fd, old)

	width := 100
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		width = w
	}
	where := ws
	if where == "" {
		where = "todos os workspaces"
	}
	sel, drawn := 0, 0
	draw := func() {
		var b strings.Builder
		if drawn > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", drawn)
		}
		b.WriteString("\r\x1b[J")
		lines := []string{fmt.Sprintf("gar · %d pendente(s) em %s · ↑/↓ escolhe · Enter roda · Esc sai", len(cases), where)}
		for i, r := range cases {
			mark := "  "
			if i == sel {
				mark = "> "
			}
			lines = append(lines, mark+caseLine(r))
		}
		r := cases[sel]
		lines = append(lines, "")
		if r.Why != "" {
			lines = append(lines, "por quê: "+r.Why)
		}
		if r.Expect != "" {
			lines = append(lines, "espera: "+r.Expect)
		}
		if r.Cwd != "" {
			lines = append(lines, "em: "+r.Cwd)
		}
		for i, s := range r.Steps {
			lines = append(lines, fmt.Sprintf("  %d. %s", i+1, s.Cmd))
		}
		for i, l := range lines {
			l = shorten(l, width-1)
			if i == sel+1 {
				l = "\x1b[7m" + l + "\x1b[0m"
			}
			b.WriteString(l + "\r\n")
		}
		drawn = len(lines)
		os.Stdout.WriteString(b.String())
	}
	clear := func() { fmt.Printf("\x1b[%dA\r\x1b[J", drawn) }

	draw()
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			clear()
			return nil, err
		}
		switch k := string(buf[:n]); k {
		case "\x1b[A", "\x1bOA", "k":
			sel = (sel + len(cases) - 1) % len(cases)
		case "\x1b[B", "\x1bOB", "j":
			sel = (sel + 1) % len(cases)
		case "\r", "\n":
			clear()
			return cases[sel], nil
		case "\x1b", "q", "\x03", "\x04":
			clear()
			return nil, nil
		default:
			continue
		}
		draw()
	}
}

func caseLine(r *Run) string {
	what := r.Why
	if len(r.Steps) > 0 {
		first := shorten(r.Steps[0].Cmd, 50)
		if len(r.Steps) > 1 {
			first = fmt.Sprintf("%s (+%d)", first, len(r.Steps)-1)
		}
		if what == "" {
			what = first
		} else {
			what = shorten(what, 40) + " · " + first
		}
	}
	by := ""
	if r.CreatedBy != "" {
		by = " " + r.CreatedBy
	}
	return fmt.Sprintf("%d  %s%s  %s", r.ID, r.CreatedAt.Format("15:04"), by, what)
}
