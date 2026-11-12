package main

import (
	"regexp"
	"sync"
	"unicode/utf8"
)

// MaxOutput is how much cleaned output a step keeps (the tail).
const MaxOutput = 64 << 10

// rawCap bounds the raw bytes held while a step runs; cleaning only shrinks
// text, so 4× leaves room for escape-heavy output.
const rawCap = 4 * MaxOutput

var (
	// OSC (…BEL or …ST), CSI, DCS/SOS/PM/APC strings, charset selection, other ESC sequences.
	ansiRE = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[PX^_][^\x1b]*\x1b\\|\x1b[()*+][0-~]|\x1b[ -/]*[0-~]`)
	crlfRE = regexp.MustCompile(`\r+\n`)
)

// Clean strips ANSI escapes and turns terminal line endings into \n.
func Clean(raw []byte) string {
	b := ansiRE.ReplaceAll(raw, nil)
	return string(crlfRE.ReplaceAll(b, []byte("\n")))
}

// tailBuffer keeps the last rawCap bytes written to it; safe for concurrent use.
type tailBuffer struct {
	mu      sync.Mutex
	buf     []byte
	dropped bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - rawCap; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.dropped = true
	}
	return len(p), nil
}

// Result returns the cleaned tail (at most MaxOutput bytes, cut on a rune
// boundary) and whether anything was left out.
func (t *tailBuffer) Result() (string, bool) {
	t.mu.Lock()
	out := Clean(t.buf)
	truncated := t.dropped
	t.mu.Unlock()
	cut := 0
	if len(out) > MaxOutput {
		cut = len(out) - MaxOutput
		truncated = true
	}
	if truncated {
		for cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut++
		}
	}
	return out[cut:], truncated
}
