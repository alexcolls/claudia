package main

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// replay feeds a recorded stream through a parser. See testdata/README.md.
func replay(t *testing.T, p Parser, name string) (ups []Update, last *Result, live Usage) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	live = Usage{}
	s := bufio.NewScanner(f)
	s.Buffer(nil, 16<<20)
	for s.Scan() {
		u := p.Parse(s.Bytes())
		ups = append(ups, u)
		if u.Result != nil {
			last = u.Result
		}
		live.add(u.Live)
	}
	return ups, last, live
}

func kinds(ups []Update) map[string]int {
	k := map[string]int{}
	for _, u := range ups {
		for _, e := range u.Events {
			k[e.Kind]++
		}
	}
	return k
}

func session(ups []Update) string {
	for _, u := range ups {
		if u.Session != "" {
			return u.Session
		}
	}
	return ""
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func newClaude() Parser { return claudeAdapter{}.Parser(Agent{}) }

func TestClaudeSuccess(t *testing.T) {
	ups, res, live := replay(t, newClaude(), "claude_first")
	if res == nil || !res.OK {
		t.Fatalf("want a successful result, got %+v", res)
	}
	if session(ups) == "" {
		t.Error("no session ID")
	}
	k := kinds(ups)
	if k["text"] != 1 || k["delta"] == 0 || k["result"] != 1 {
		t.Errorf("events: %v", k)
	}
	m := res.Cumulative["claude-sonnet-4-5"]
	if m.Input != 100 || m.Output != 7 || m.CacheRead != 40 || m.CacheWrite != 10 || m.Basis != "list" || m.USD <= 0 {
		t.Errorf("usage: %+v", m)
	}
	// The stream's count must match the result: one response, counted once.
	l := live["claude-sonnet-4-5"]
	if l.Input != 100 || l.Output != 7 || l.CacheRead != 40 || l.CacheWrite != 10 {
		t.Errorf("live: %+v", l)
	}
}

func TestClaudeToolCall(t *testing.T) {
	ups, res, live := replay(t, newClaude(), "claude_tool")
	k := kinds(ups)
	if k["tool_call"] != 1 || k["tool_result"] != 1 {
		t.Errorf("events: %v", k)
	}
	if !res.OK || res.Turns != 2 {
		t.Errorf("result: %+v", res)
	}
	if got := live["claude-sonnet-4-5"].Input; got != 200 {
		t.Errorf("two responses should count 200 input tokens, got %d", got)
	}
}

// Exit 0 and a success result, but the tool never ran.
func TestClaudeDenied(t *testing.T) {
	for _, name := range []string{"claude_manual_denied", "claude_allowlist_denied", "claude_approve_deny"} {
		_, res, _ := replay(t, newClaude(), name)
		if !res.OK || len(res.Denials) != 1 || res.Denials[0] != "Bash" {
			t.Errorf("%s: %+v", name, res)
		}
	}
}

func TestClaudeSigint(t *testing.T) {
	_, res, live := replay(t, newClaude(), "claude_sigint")
	if res == nil || res.OK || res.Reason != "aborted_streaming" {
		t.Fatalf("result: %+v", res)
	}
	if res.Cumulative.total() != 0 {
		t.Errorf("recording shows a zeroed result, got %+v", res.Cumulative)
	}
	if live.total() == 0 {
		t.Error("the stream should still show the aborted response's input tokens")
	}
	s := settle(res, Usage{}, live, false, map[string]Price{"claude-sonnet-4-5": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}})
	if s.State != USDEstimated || s.USD <= 0 {
		t.Errorf("settled: %+v", s)
	}
}

func TestClaudeNoResult(t *testing.T) {
	_, res, live := replay(t, newClaude(), "claude_sigterm")
	if res != nil {
		t.Fatalf("SIGTERM should leave no result, got %+v", res)
	}
	if s := settle(nil, Usage{}, live, false, nil); s.State != USDUnpriced {
		t.Errorf("no price known: %+v", s)
	}
}

func TestClaudeBudgetStop(t *testing.T) {
	_, res, _ := replay(t, newClaude(), "claude_budget")
	if res.OK || res.Reason != "budget_exhausted" {
		t.Errorf("%+v", res)
	}
	if res.Cumulative["claude-sonnet-4-5"].USD <= 0 {
		t.Error("a budget stop still carries its cost")
	}
}

// A crash loses a run's spend from the CLI's totals. The next run's cost must
// be only its own.
func TestClaudeCrashThenResume(t *testing.T) {
	marks := Usage{}
	_, r0, _ := replay(t, newClaude(), "claude_crash_0")
	d0 := advanceMarks(marks, r0.Cumulative)
	first := d0["claude-sonnet-4-5"].USD
	if first <= 0 {
		t.Fatal("first run has no cost")
	}
	_, r1, _ := replay(t, newClaude(), "claude_crash_1")
	if r1 != nil {
		t.Fatalf("SIGKILL should leave no result, got %+v", r1)
	}
	_, r2, _ := replay(t, newClaude(), "claude_crash_2")
	d2 := advanceMarks(marks, r2.Cumulative)
	if got := d2["claude-sonnet-4-5"].USD; !near(got, first) {
		t.Errorf("resumed run should cost one response (%v), got %v", first, got)
	}
	if got := d2["claude-sonnet-4-5"].Input; got != 100 {
		t.Errorf("resumed run should add 100 input tokens, got %d", got)
	}
}

func TestAdvanceMarksNeverLowers(t *testing.T) {
	marks := Usage{"m": {Input: 500, USD: 5}}
	if d := advanceMarks(marks, Usage{"m": {}}); len(d) != 0 {
		t.Errorf("zeroed totals must add nothing: %+v", d)
	}
	if marks["m"].Input != 500 || marks["m"].USD != 5 {
		t.Errorf("marks moved down: %+v", marks["m"])
	}
	d := advanceMarks(marks, Usage{"m": {Input: 700, USD: 7}})
	if d["m"].Input != 200 || !near(d["m"].USD, 2) {
		t.Errorf("delta: %+v", d["m"])
	}
}

func TestSettleStates(t *testing.T) {
	prices := map[string]Price{"m": {Input: 10, Output: 20}}
	ok := &Result{OK: true}
	cases := []struct {
		name  string
		res   *Result
		delta Usage
		live  Usage
		sub   bool
		state string
		usd   float64
	}{
		{"CLI's figure", ok, Usage{"m": {Input: 1e6, USD: 3, Basis: "list"}}, nil, false, USDReported, 3},
		{"unknown basis uses the table", ok, Usage{"m": {Input: 1e6, USD: 99, Basis: "unknown"}}, nil, false, USDPriced, 10},
		{"no USD from the CLI", ok, Usage{"m": {Input: 1e6, Output: 1e6}}, nil, false, USDPriced, 30},
		{"unknown model is not $0", ok, Usage{"x": {Input: 5}}, nil, false, USDUnpriced, 0},
		{"subscription", ok, Usage{"m": {Input: 5, USD: 3, Basis: "list"}}, nil, true, USDSubscription, 0},
		{"no result", nil, Usage{}, Usage{"m": {Input: 1e6}}, false, USDEstimated, 10},
		{"failed, stream larger", &Result{}, Usage{}, Usage{"m": {Input: 1e6}}, false, USDEstimated, 10},
		{"failed, result larger", &Result{}, Usage{"m": {Input: 2e6, USD: 7, Basis: "list"}}, Usage{"m": {Input: 1e6}}, false, USDReported, 7},
	}
	for _, c := range cases {
		s := settle(c.res, c.delta, c.live, c.sub, prices)
		if s.State != c.state || !near(s.USD, c.usd) {
			t.Errorf("%s: got %s $%v, want %s $%v", c.name, s.State, s.USD, c.state, c.usd)
		}
	}
}

// Recorded across a resume: Codex's usage is cumulative for the thread.
func TestCodexCumulative(t *testing.T) {
	model := "gpt-test"
	p := func() Parser { return codexAdapter{}.Parser(Agent{Model: model}) }
	marks := Usage{}
	ups, r1, _ := replay(t, p(), "codex_first")
	if !r1.OK || session(ups) == "" {
		t.Fatalf("%+v session=%q", r1, session(ups))
	}
	first := r1.Cumulative[model]
	if first.Input != 60 || first.CacheRead != 40 || first.Output != 7 {
		t.Errorf("input should exclude cached: %+v", first)
	}
	advanceMarks(marks, r1.Cumulative)
	_, r2, _ := replay(t, p(), "codex_resume")
	d := advanceMarks(marks, r2.Cumulative)
	if d[model].Input != 60 || d[model].Output != 7 {
		t.Errorf("resumed turn should add one response: %+v", d[model])
	}
}

func TestCodexToolAndFailure(t *testing.T) {
	ups, res, _ := replay(t, codexAdapter{}.Parser(Agent{Model: "m"}), "codex_tool")
	k := kinds(ups)
	if k["tool_call"] != 1 || k["tool_result"] != 1 || k["text"] != 1 || !res.OK {
		t.Errorf("events: %v result %+v", k, res)
	}
	ups, res, _ = replay(t, codexAdapter{}.Parser(Agent{Model: "m"}), "codex_fail")
	if res != nil {
		t.Errorf("recorded run never ended: %+v", res)
	}
	retries := 0
	for _, u := range ups {
		if u.Retry != "" {
			retries++
		}
	}
	if retries == 0 {
		t.Error("reconnect errors should count as retries")
	}
}

func TestAgy(t *testing.T) {
	p := func() Parser { return agyAdapter{}.Parser(Agent{Model: "gemini-test"}) }
	ups, res, live := replay(t, p(), "agy_first")
	k := kinds(ups)
	if !res.OK || session(ups) == "" || k["text"] != 1 || k["delta"] != 1 {
		t.Fatalf("result %+v events %v", res, k)
	}
	if u := res.Cumulative["gemini-test"]; u.Input != 60 || u.Output != 9 || u.CacheRead != 40 {
		t.Errorf("usage: %+v", u)
	}
	if u := live["gemini-test"]; u.Input != 60 || u.Output != 9 {
		t.Errorf("live: %+v", u)
	}
	marks := Usage{}
	advanceMarks(marks, res.Cumulative)
	_, r2, _ := replay(t, p(), "agy_resume")
	if d := advanceMarks(marks, r2.Cumulative); d["gemini-test"].Input != 60 {
		t.Errorf("resume should add one turn: %+v", d)
	}
	_, res, _ = replay(t, p(), "agy_fakekey")
	if res.OK || !strings.Contains(res.Error, "API key not valid") {
		t.Errorf("invalid key: %+v", res)
	}
	_, res, _ = replay(t, p(), "agy_noauth")
	if res.OK || !strings.Contains(res.Error, "authentication") {
		t.Errorf("no auth: %+v", res)
	}
	if !agyAdapter.LingersAfterResult(agyAdapter{}) {
		t.Error("agy hangs after an auth failure, so the run must end it")
	}
}

// Junk on stdout must never break a run.
func TestParsersIgnoreJunk(t *testing.T) {
	for _, p := range []Parser{newClaude(), codexAdapter{}.Parser(Agent{}), agyAdapter{}.Parser(Agent{})} {
		for _, line := range []string{"", "not json", "{", "[]", `{"type":5}`, `{"event":null}`} {
			if u := p.Parse([]byte(line)); u.Result != nil {
				t.Errorf("%q produced a result", line)
			}
		}
	}
}

func TestCommands(t *testing.T) {
	a := Agent{Token: "tok", Mode: ModeAsk, Model: "m"}
	argv, stdin := claudeAdapter{}.Command(RunSpec{Agent: a, Prompt: "hi", Session: "s", MCPURL: "http://x/mcp", MaxUSD: 2})
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--setting-sources=user", "--strict-mcp-config", "--session-id=s", "--max-budget-usd=2.0000",
		`"timeout":86400000`, "--permission-prompt-tool=mcp__claudia__approve", "--permission-mode=manual"} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude argv lacks %s: %s", want, joined)
		}
	}
	if stdin != "hi" || strings.Contains(joined, " hi") {
		t.Error("the prompt belongs on stdin")
	}
	argv, _ = claudeAdapter{}.Command(RunSpec{Agent: Agent{Mode: ModeFull}, Session: "s", Resume: true})
	if !strings.Contains(strings.Join(argv, " "), "--resume=s") {
		t.Error("resume flag missing")
	}
	argv, stdin = codexAdapter{}.Command(RunSpec{Agent: Agent{Model: "m"}, Prompt: "hi", Session: "t", Resume: true})
	if got := strings.Join(argv, " "); !strings.HasPrefix(got, "codex exec resume --json") || !strings.HasSuffix(got, "t -") || stdin != "hi" {
		t.Errorf("codex resume argv: %s", got)
	}
	argv, _ = agyAdapter{}.Command(RunSpec{Agent: Agent{Preset: "gemini", Mode: ModeFull}, Prompt: "hi", Session: "c", Resume: true})
	got := strings.Join(argv, " ")
	for _, want := range []string{"--conversation c", `"modelProvider":"gemini"`, "--dangerously-skip-permissions"} {
		if !strings.Contains(got, want) {
			t.Errorf("agy argv lacks %s: %s", want, got)
		}
	}
}

func TestModes(t *testing.T) {
	if err := validMode("codex", "openai", ModeAsk); err == nil {
		t.Error("Codex is full auto only")
	}
	if err := validMode("claude", "gateway", ModeReviewed); err == nil {
		t.Error("auto mode isn't offered on generic gateways")
	}
	if err := validMode("claude", "anthropic", ModeReviewed); err != nil {
		t.Error(err)
	}
	if err := validMode("agy", "gemini", ModeAsk); err == nil {
		t.Error("agy has no human approval path")
	}
	if err := validMode("nope", "", ModeFull); err == nil {
		t.Error("unknown CLI")
	}
}
