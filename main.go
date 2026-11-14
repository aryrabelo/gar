// Command gar (Go And Run) queues commands an agent needs a human to run,
// runs them in the human's terminal and hands the result back to the agent.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	exitFailed    = 1
	exitCancelled = 2
	exitNoDuckDB  = 3
	exitState     = 4
	exitNoTTY     = 5
	exitUsage     = 64
	exitTimeout   = 124
)

const usage = `gar — o agente registra, o humano roda, o agente lê o resultado.

  gar add [--why s] [--expect s] [--from s] [--timeout dur] [--json] -- <cmd> [<cmd>...]
  gar run [<id>] [--yes] [--again]     (só humano: exige TTY; sem id escolhe
                                       com ↑/↓ e Enter entre os pendentes do workspace)
  gar wait <id> [--timeout dur]
  gar show <id> [--json]
  gar ls [--status s] [--limit n]
  gar cancel <id>
  gar sql "<query>"                    (precisa do duckdb no PATH)

Estado em $GAR_HOME (padrão ~/.local/state/gar/).
`

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, usage)
		if len(args) == 0 {
			return exitUsage
		}
		return 0
	}
	home, err := DefaultHome()
	if err != nil {
		return fail(exitFailed, err)
	}
	st := Store{Home: home}
	if err := st.Init(); err != nil {
		return fail(exitFailed, err)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "add":
		return cmdAdd(st, rest)
	case "run":
		return cmdRun(st, rest)
	case "wait":
		return cmdWait(st, rest)
	case "show":
		return cmdShow(st, rest)
	case "ls":
		return cmdLs(st, rest)
	case "cancel":
		return cmdCancel(st, rest)
	case "sql":
		return cmdSQL(st, rest)
	}
	fmt.Fprintf(os.Stderr, "gar: comando desconhecido %q\n\n%s", cmd, usage)
	return exitUsage
}

func fail(code int, err error) int {
	fmt.Fprintln(os.Stderr, "gar:", err)
	return code
}

// parseArgs lets flags and positionals mix (`gar run 1020 --yes`).
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("gar "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// oneID parses flags and requires exactly one positional case ID.
func oneID(fs *flag.FlagSet, args []string) (int, int) {
	pos, err := parseArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return 0, 0
	}
	if err != nil {
		return 0, exitUsage
	}
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "%s: informe exatamente um id\n", fs.Name())
		return 0, exitUsage
	}
	id, err := strconv.Atoi(pos[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: id inválido %q\n", fs.Name(), pos[0])
		return 0, exitUsage
	}
	return id, -1
}

func loadErr(err error) int {
	if errors.Is(err, errNotFound) || errors.Is(err, errState) {
		return fail(exitState, err)
	}
	return fail(exitFailed, err)
}

func printJSON(v any) {
	b, _ := marshalRun(v)
	os.Stdout.Write(b)
}

func cmdAdd(st Store, args []string) int {
	fs := newFlags("add")
	why := fs.String("why", "", "por que o humano precisa rodar isto")
	expect := fs.String("expect", "", "o que se espera ver")
	from := fs.String("from", os.Getenv("GAR_FROM"), "quem registrou (padrão $GAR_FROM)")
	asJSON := fs.Bool("json", false, "imprime o registro em JSON")
	timeout := fs.Duration("timeout", defaultStepTimeout, "limite de cada passo (ex.: 10m); estourou, o passo falha e o caso para")
	flagArgs, steps := args, []string(nil)
	for i, a := range args {
		if a == "--" {
			flagArgs, steps = args[:i], args[i+1:]
			break
		}
	}
	pos, err := parseArgs(fs, flagArgs)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return exitUsage
	}
	steps = append(pos, steps...)
	if len(steps) == 0 {
		fmt.Fprintln(os.Stderr, "gar add: nenhum passo; use gar add -- '<cmd>' ['<cmd>'...]")
		return exitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "gar add: --timeout precisa ser maior que zero")
		return exitUsage
	}
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	r := &Run{
		Status: StatusPending, CreatedAt: time.Now(), CreatedBy: *from,
		Why: *why, Expect: *expect, Cwd: cwd, Host: host, Workspace: detectWorkspace(),
		StepTimeout: timeout.String(),
	}
	for _, c := range steps {
		r.Steps = append(r.Steps, Step{Cmd: c, Status: StatusPending})
	}
	if err := st.Create(r); err != nil {
		return fail(exitFailed, err)
	}
	if *asJSON {
		printJSON(r)
		return 0
	}
	if r.Workspace != "" {
		fmt.Printf("caso %d registrado em %s\n[HUMAN] ! gar run\n", r.ID, r.Workspace)
		return 0
	}
	fmt.Printf("caso %d registrado\n[HUMAN] ! gar run %d\n", r.ID, r.ID)
	return 0
}

func cmdWait(st Store, args []string) int {
	fs := newFlags("wait")
	timeout := fs.Duration("timeout", 0, "desiste depois de dur (ex.: 10m); 0 = sem limite")
	id, code := oneID(fs, args)
	if code >= 0 {
		return code
	}
	var deadline time.Time
	if *timeout > 0 {
		deadline = time.Now().Add(*timeout)
	}
	for {
		r, err := st.Load(id)
		if err != nil {
			return loadErr(err)
		}
		if r.Terminal() {
			printJSON(r)
			switch r.Status {
			case StatusDone:
				return 0
			case StatusCancelled:
				return exitCancelled
			}
			return exitFailed
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			printJSON(r)
			fmt.Fprintf(os.Stderr, "gar wait: timeout de %s; caso %d ainda %s\n", *timeout, id, r.Status)
			return exitTimeout
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func cmdShow(st Store, args []string) int {
	fs := newFlags("show")
	asJSON := fs.Bool("json", false, "imprime o registro em JSON")
	id, code := oneID(fs, args)
	if code >= 0 {
		return code
	}
	r, err := st.Load(id)
	if err != nil {
		return loadErr(err)
	}
	if *asJSON {
		printJSON(r)
		return 0
	}
	fmt.Printf("caso %d · %s · criado %s", r.ID, r.Status, r.CreatedAt.Format("2006-01-02 15:04"))
	if r.CreatedBy != "" {
		fmt.Printf(" por %s", r.CreatedBy)
	}
	fmt.Printf(" · %s:%s\n", r.Host, r.Cwd)
	if r.Why != "" {
		fmt.Println("por quê:", r.Why)
	}
	if r.Expect != "" {
		fmt.Println("espera:", r.Expect)
	}
	for i, s := range r.Steps {
		exit := "-"
		if s.ExitCode != nil {
			exit = strconv.Itoa(*s.ExitCode)
		}
		fmt.Printf("\n[%d] %s · exit %s · %s\n    $ %s\n", i+1, s.Status, exit,
			(time.Duration(s.DurationMs) * time.Millisecond).String(), s.Cmd)
		if s.Output != "" {
			lines := strings.Split(strings.TrimRight(s.Output, "\n"), "\n")
			if len(lines) > 5 {
				lines = lines[len(lines)-5:]
				fmt.Println("    …")
			}
			for _, l := range lines {
				fmt.Println("    │", l)
			}
		}
	}
	return 0
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func cmdLs(st Store, args []string) int {
	fs := newFlags("ls")
	status := fs.String("status", "", "filtra por status")
	limit := fs.Int("limit", 20, "máximo de linhas (0 = todas)")
	pos, err := parseArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil || len(pos) > 0 {
		return exitUsage
	}
	runs, err := st.List()
	if err != nil {
		return fail(exitFailed, err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tCRIADO\tWHY\tPASSO")
	n := 0
	for _, r := range runs {
		if *status != "" && r.Status != *status {
			continue
		}
		if *limit > 0 && n == *limit {
			break
		}
		n++
		first := ""
		if len(r.Steps) > 0 {
			first = r.Steps[0].Cmd
			if len(r.Steps) > 1 {
				first = fmt.Sprintf("%s (+%d)", shorten(first, 40), len(r.Steps)-1)
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", r.ID, r.Status,
			r.CreatedAt.Format("01-02 15:04"), shorten(r.Why, 40), shorten(first, 48))
	}
	tw.Flush()
	return 0
}

func cmdCancel(st Store, args []string) int {
	id, code := oneID(newFlags("cancel"), args)
	if code >= 0 {
		return code
	}
	_, err := st.Update(id, func(r *Run) error {
		if r.Status != StatusPending {
			return fmt.Errorf("caso %d está %s; só caso pending pode ser cancelado: %w", id, r.Status, errState)
		}
		now := time.Now()
		r.FinishedAt = &now
		for i := range r.Steps {
			r.Steps[i].Status = StepSkipped
		}
		return r.SetStatus(StatusCancelled)
	})
	if err != nil {
		return loadErr(err)
	}
	fmt.Printf("caso %d cancelado\n", id)
	return 0
}

func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func cmdSQL(st Store, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, `gar sql: use gar sql "<query>"`)
		return exitUsage
	}
	duck, err := exec.LookPath("duckdb")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gar sql precisa do duckdb no PATH")
		return exitNoDuckDB
	}
	glob := filepath.Join(st.runsDir(), "*.json")
	script := fmt.Sprintf(`CREATE VIEW runs AS SELECT * FROM read_json_auto(%s, union_by_name = true);
CREATE VIEW steps AS SELECT id AS run_id, unnest(steps, recursive := true) FROM runs;
%s;
`, sqlQuote(glob), strings.TrimRight(strings.TrimSpace(args[0]), ";"))
	c := exec.Command(duck)
	c.Stdin = strings.NewReader(script)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return fail(exitFailed, err)
	}
	return 0
}
