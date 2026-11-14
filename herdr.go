package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// herdrDialog reports whether gar run is the picker the gar-run mod shows in a
// Herdr desktop dialog. HC_TERMINAL_ID alone is not enough: a bora pane inside a
// Herdr terminal inherits it and would mark the outer terminal.
func herdrDialog() bool { return os.Getenv("GAR_HERDR_DIALOG") == "1" }

// quietRun runs name with a short timeout, detached from the picker's TTY.
func quietRun(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run() // nil stdio: /dev/null
}

// hideDialog closes the top Herdr dialog; the hc terminal running the picker stays alive.
func hideDialog() error {
	bin, err := exec.LookPath("herdr-app")
	if err != nil {
		bin = filepath.Join(os.Getenv("HOME"), ".local", "bin", "herdr-app")
	}
	return quietRun(bin, "do", "overlay.close")
}

// labelCase tags this hc terminal with the case it runs, so gar-run and gar-panel
// find it. Best effort: errors are ignored.
func labelCase(id int) {
	if t := os.Getenv("HC_TERMINAL_ID"); t != "" {
		_ = quietRun("hc", "label", t, "gar.case="+strconv.Itoa(id))
	}
}
