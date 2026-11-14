package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// detectWorkspace names the multiplexer workspace this process lives in, so
// `gar add` can tag a case and a bare `gar run` can offer only that
// workspace's cases. Empty when no multiplexer is detected.
//
//   - $GAR_WORKSPACE wins when set (tests, odd setups).
//   - bora / herdr TUI: $HERDR_WORKSPACE_ID, prefixed with the binary name in
//     $HERDR_BIN_PATH ("bora:w1D6"). Checked first: a bora pane opened inside a
//     Herdr app terminal inherits HC_TERMINAL_ID, and bora is the inner one.
//   - Herdr app: the space of $HC_TERMINAL_ID from `hc ls --json`
//     ("herdr-app:<space>"); if hc can't answer, the terminal itself.
func detectWorkspace() string {
	if ws, ok := os.LookupEnv("GAR_WORKSPACE"); ok {
		return ws
	}
	if id := os.Getenv("HERDR_WORKSPACE_ID"); id != "" {
		app := "herdr"
		if bin := os.Getenv("HERDR_BIN_PATH"); bin != "" {
			app = filepath.Base(bin)
		}
		return app + ":" + id
	}
	if term := os.Getenv("HC_TERMINAL_ID"); term != "" {
		if space := hcSpace(term); space != "" {
			return "herdr-app:" + space
		}
		return "herdr-app:terminal:" + term
	}
	return ""
}

// hcSpace asks herdr-core which space a terminal sits in. Its `where` reads
// "<space> / <tab>".
func hcSpace(term string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "hc", "ls", "--json").Output()
	if err != nil {
		return ""
	}
	var ls struct {
		Terminals []struct {
			Address string `json:"address"`
			Where   string `json:"where"`
		} `json:"terminals"`
	}
	if json.Unmarshal(out, &ls) != nil {
		return ""
	}
	for _, t := range ls.Terminals {
		if t.Address == term {
			space, _, _ := strings.Cut(t.Where, " / ")
			return strings.TrimSpace(space)
		}
	}
	return ""
}
