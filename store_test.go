package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestStatusTransitions(t *testing.T) {
	all := []string{StatusPending, StatusRunning, StatusDone, StatusFailed, StatusCancelled}
	allowed := map[[2]string]bool{
		{StatusPending, StatusRunning}:   true,
		{StatusPending, StatusCancelled}: true,
		{StatusRunning, StatusDone}:      true,
		{StatusRunning, StatusFailed}:    true,
		{StatusRunning, StatusCancelled}: true,
	}
	for _, from := range all {
		for _, to := range all {
			r := &Run{ID: 1, Status: from}
			err := r.SetStatus(to)
			if want := allowed[[2]string{from, to}]; want != (err == nil) {
				t.Errorf("%s → %s: err=%v, quer permitido=%v", from, to, err, want)
			}
			if err != nil && r.Status != from {
				t.Errorf("%s → %s recusado mas status virou %s", from, to, r.Status)
			}
		}
	}
}

func TestResetOnlyFromTerminal(t *testing.T) {
	code := 3
	r := &Run{ID: 1, Status: StatusFailed, Steps: []Step{{Cmd: "x", Status: StatusFailed, ExitCode: &code, Output: "boom"}}}
	if err := r.Reset(); err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusPending || r.Steps[0].ExitCode != nil || r.Steps[0].Output != "" || r.Steps[0].Cmd != "x" {
		t.Fatalf("reset incompleto: %+v", r)
	}
	for _, s := range []string{StatusPending, StatusRunning} {
		if err := (&Run{Status: s}).Reset(); err == nil {
			t.Errorf("reset de %s deveria falhar", s)
		}
	}
}

// A reader racing the writer must always see a whole record, and no temp
// files may be left behind.
func TestSaveIsAtomic(t *testing.T) {
	st := Store{Home: t.TempDir()}
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	r := &Run{ID: 1001, Status: StatusRunning, Steps: []Step{{Cmd: "x"}}}
	if err := st.Save(r); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	errs := make(chan error, 1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := st.Load(1001)
			if err == nil && len(got.Steps[0].Output) != 0 && len(got.Steps[0].Output) != 200_000 && len(got.Steps[0].Output) != 100_000 {
				err = os.ErrInvalid
			}
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
		}
	}()
	for i := range 200 {
		n := 100_000
		if i%2 == 0 {
			n = 200_000
		}
		r.Steps[0].Output = strings.Repeat(string(rune('a'+i%26)), n)
		if err := st.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("leitor viu registro parcial: %v", err)
	default:
	}
	left, _ := filepath.Glob(filepath.Join(st.runsDir(), ".*"))
	if len(left) > 0 {
		t.Fatalf("arquivos temporários sobraram: %v", left)
	}
	b, _ := os.ReadFile(st.path(1001))
	if !json.Valid(b) {
		t.Fatal("arquivo final não é JSON válido")
	}
}

func TestNextIDSkipsExistingCase(t *testing.T) {
	st := Store{Home: t.TempDir()}
	st.Init()
	if err := st.Save(&Run{ID: 1001}); err != nil { // counter lost, case exists
		t.Fatal(err)
	}
	id, err := st.NextID()
	if err != nil || id != 1002 {
		t.Fatalf("NextID = %d, %v; quer 1002", id, err)
	}
}

func TestCleanStripsANSI(t *testing.T) {
	cases := map[string]string{
		"\x1b[1;31mvermelho\x1b[0m\r\n":            "vermelho\n",
		"\x1b]0;título\x07texto":                   "texto",
		"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\": "link",
		"a\x1b[2K\x1b[1Gb":                         "ab",
		"\x1b(Bok\x1b=\r\r\n":                      "ok\n",
		"senha: \r\nlinha\nfim":                    "senha: \nlinha\nfim",
		"acentuação ✓":                             "acentuação ✓",
	}
	for in, want := range cases {
		if got := Clean([]byte(in)); got != want {
			t.Errorf("Clean(%q) = %q, quer %q", in, got, want)
		}
	}
}

func TestTailBufferKeepsLast64KiB(t *testing.T) {
	var tb tailBuffer
	tb.Write([]byte("início\r\n"))
	if out, trunc := tb.Result(); out != "início\n" || trunc {
		t.Fatalf("curto: %q trunc=%v", out, trunc)
	}
	// 100 KiB of text with escapes interleaved, ending in a marker.
	for range 100 * 1024 / 16 {
		tb.Write([]byte("\x1b[32mé123456789\x1b[0m\r\n")) // 13 visible bytes
	}
	tb.Write([]byte("FIM"))
	out, trunc := tb.Result()
	if !trunc {
		t.Fatal("deveria marcar output_truncated")
	}
	if len(out) > MaxOutput || len(out) < MaxOutput-4 {
		t.Fatalf("tamanho %d, quer ~%d", len(out), MaxOutput)
	}
	if !strings.HasSuffix(out, "FIM") || strings.Contains(out, "início") || strings.Contains(out, "\x1b") || strings.Contains(out, "\r") {
		t.Fatalf("cauda errada: começo %q fim %q", out[:20], out[len(out)-20:])
	}
	if !utf8.ValidString(out) {
		t.Fatalf("corte no meio de uma runa: %q", out[:4])
	}
}

func TestTailBufferBoundsRawBytes(t *testing.T) {
	var tb tailBuffer
	chunk := []byte(strings.Repeat("\x1b[0m", 1024))
	for range 2000 {
		tb.Write(chunk)
	}
	if len(tb.buf) > rawCap {
		t.Fatalf("buffer cru cresceu para %d", len(tb.buf))
	}
	if _, trunc := tb.Result(); !trunc {
		t.Fatal("bytes descartados devem marcar truncado")
	}
}
