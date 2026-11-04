package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Permission modes an agent can run in. Which ones apply depends on the CLI
// and the provider, see Adapter.Modes.
const (
	ModeAsk       = "ask"       // every risky action waits for the owner
	ModeReviewed  = "reviewed"  // the CLI's own classifier decides
	ModeAllowlist = "allowlist" // listed tools run, anything else is denied
	ModeFull      = "full"      // no checks inside the container
)

// Tokens counts usage for one model. USD is only set when the CLI reported it.
// Input excludes cached input, so each field is priced at its own rate.
type Tokens struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	USD        float64 `json:"usd,omitempty"`
	Basis      string  `json:"basis,omitempty"` // Claude Code's costBasis: list, managed or unknown
}

func (t Tokens) total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

// Usage is token counts by model.
type Usage map[string]Tokens

func (u Usage) total() int64 {
	var n int64
	for _, t := range u {
		n += t.total()
	}
	return n
}

// add folds other into u.
func (u Usage) add(other Usage) {
	for m, t := range other {
		c := u[m]
		c.Input += t.Input
		c.Output += t.Output
		c.CacheRead += t.CacheRead
		c.CacheWrite += t.CacheWrite
		c.USD += t.USD
		if t.Basis != "" {
			c.Basis = t.Basis
		}
		u[m] = c
	}
}

// Event is one normalized feed entry. Kinds: user, text, delta (never stored),
// tool_call, tool_result, status, error, result. Input is a string because tool
// input gets clipped, which can leave it as invalid JSON.
type Event struct {
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	ToolID  string `json:"tool_id,omitempty"`
	Input   string `json:"input,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// Result is what a CLI reports at the end of a turn.
type Result struct {
	OK         bool
	Reason     string
	Error      string
	Cumulative Usage // session-wide totals: every CLI restores them on resume
	Turns      int
	DurationMS int64
	APIMS      int64
	Denials    []string
}

// Update is what one line of CLI output adds to a run.
type Update struct {
	Events  []Event
	Session string
	Live    Usage   // tokens first seen on this line, for estimates during the run
	Retry   string  // non-empty when the CLI is retrying; holds its error category
	Result  *Result // set when the line reports the end of a turn
}

// Parser reads one run's output. It keeps state, so each run needs its own.
type Parser interface {
	Parse(line []byte) Update
}

// RunSpec is what an adapter needs to build a command.
type RunSpec struct {
	Agent   Agent
	Prompt  string
	Session string // existing session to resume, or a new ID for CLIs that accept one
	Resume  bool
	MaxUSD  float64
	MCPURL  string
}

// Adapter is everything specific to one CLI.
type Adapter interface {
	// Command returns the argv to run in the container and the text for stdin.
	Command(s RunSpec) (argv []string, stdin string)
	Parser(a Agent) Parser
	// Modes lists permission modes valid for this CLI on a credential preset.
	Modes(preset string) []string
	// ReportsUSD says whether the CLI prices its own usage on this preset.
	ReportsUSD(preset string) bool
	// LingersAfterResult says the process can hang once it has reported a
	// result, so the run must end it.
	LingersAfterResult() bool
}

var adapters = map[string]Adapter{
	"claude": claudeAdapter{},
	"codex":  codexAdapter{},
	"agy":    agyAdapter{},
}

func validMode(cli, preset, mode string) error {
	ad, ok := adapters[cli]
	if !ok {
		return fmt.Errorf("unknown CLI %q", cli)
	}
	modes := ad.Modes(preset)
	for _, m := range modes {
		if m == mode {
			return nil
		}
	}
	return fmt.Errorf("mode %q isn't available for %s here; choose from %s", mode, cli, strings.Join(modes, ", "))
}

const maxEventText = 16 << 10

// clip keeps stored events small. Tool output can be megabytes.
func clip(s string) string {
	if len(s) <= maxEventText {
		return s
	}
	return s[:maxEventText] + "\n... [clipped]"
}

func clipRaw(b json.RawMessage) string { return clip(string(b)) }
