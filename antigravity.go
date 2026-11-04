package main

import (
	"encoding/json"
	"strings"
)

// agyAdapter is experimental. API key or Application Default Credentials only:
// Google's FAQ says wrapping agy isn't supported on consumer plans.
type agyAdapter struct{}

func (agyAdapter) Modes(string) []string { return []string{ModeReviewed, ModeAllowlist, ModeFull} }

func (agyAdapter) ReportsUSD(string) bool { return false }

// Unauthenticated, agy reports a result and then hangs instead of exiting.
func (agyAdapter) LingersAfterResult() bool { return true }

func (agyAdapter) Command(s RunSpec) ([]string, string) {
	settings := map[string]any{}
	if s.Agent.Preset == "gemini" {
		settings["modelProvider"] = "gemini" // GEMINI_API_KEY has no effect without this
	}
	argv := []string{"agy", "-p", s.Prompt, "--output-format", "stream-json"}
	if s.Resume {
		argv = append(argv, "--conversation", s.Session)
	}
	if s.Agent.Model != "" {
		argv = append(argv, "--model", s.Agent.Model)
	}
	switch s.Agent.Mode {
	case ModeReviewed:
		argv = append(argv, "--mode", "auto")
	case ModeAllowlist:
		var rules []string
		for _, r := range strings.Split(s.Agent.AllowedTools, ",") {
			if r = strings.TrimSpace(r); r != "" {
				rules = append(rules, r)
			}
		}
		settings["permissions"] = map[string]any{"allow": rules}
	default:
		argv = append(argv, "--dangerously-skip-permissions")
	}
	b, _ := json.Marshal(settings)
	return append(argv, "--settings", string(b)), ""
}

func (agyAdapter) Parser(a Agent) Parser { return &agyParser{model: a.Model, text: map[int]string{}, tools: map[int]bool{}} }

// agyParser needs the agent's model because agy only sometimes names one.
type agyParser struct {
	model string
	text  map[int]string // assistant text so far, by step
	tools map[int]bool   // tool steps already announced
}

type agyUsage struct {
	Input     int64 `json:"input_tokens"` // excludes cache_read_tokens
	Output    int64 `json:"output_tokens"`
	CacheRead int64 `json:"cache_read_tokens"`
}

func (u *agyUsage) tokens() Tokens {
	if u == nil {
		return Tokens{}
	}
	return Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead}
}

type agyLine struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Init           *struct {
		Model string `json:"model"`
	} `json:"init"`
	StepUpdate *struct {
		ConversationID string          `json:"conversation_id"`
		StepIndex      int             `json:"step_index"`
		State          string          `json:"state"`
		StepType       string          `json:"step_type"`
		TextDelta      string          `json:"text_delta"`
		Usage          *agyUsage       `json:"usage"`
		ToolName       string          `json:"tool_name"`
		ToolInfo       json.RawMessage `json:"tool_info"`
	} `json:"step_update"`
	Result *struct {
		ConversationID string    `json:"conversation_id"`
		Status         string    `json:"status"`
		Response       string    `json:"response"`
		Error          string    `json:"error"`
		DurationSec    float64   `json:"duration_seconds"`
		NumTurns       int       `json:"num_turns"`
		Usage          *agyUsage `json:"usage"`
	} `json:"result"`
}

func (p *agyParser) Parse(line []byte) Update {
	var l agyLine
	if json.Unmarshal(line, &l) != nil {
		return Update{}
	}
	u := Update{Session: l.ConversationID}
	switch {
	case l.Init != nil:
		if l.Init.Model != "" {
			p.model = l.Init.Model
		}
	case l.StepUpdate != nil:
		p.step(&l, &u)
	case l.Result != nil:
		r := l.Result
		u.Session = r.ConversationID
		res := &Result{
			OK: r.Status == "SUCCESS", Reason: strings.ToLower(r.Status), Error: r.Error,
			Turns: r.NumTurns, DurationMS: int64(r.DurationSec * 1000), Cumulative: Usage{},
		}
		if r.Usage != nil && r.Usage.Input+r.Usage.Output+r.Usage.CacheRead > 0 {
			res.Cumulative[p.model] = r.Usage.tokens() // cumulative across a resume
		}
		if !res.OK && res.Error == "" {
			res.Error = "agy reported status " + r.Status
		}
		u.Result = res
		text := r.Error
		if text == "" && !res.OK {
			text = res.Error
		}
		u.Events = []Event{{Kind: "result", Text: text, IsError: !res.OK}}
	}
	return u
}

func (p *agyParser) step(l *agyLine, u *Update) {
	s := l.StepUpdate
	u.Session = s.ConversationID
	if s.Usage != nil {
		u.Live = Usage{p.model: s.Usage.tokens()} // per step, unlike the result's
	}
	switch s.StepType {
	case "agent_response":
		if s.TextDelta != "" {
			p.text[s.StepIndex] += s.TextDelta
			u.Events = append(u.Events, Event{Kind: "delta", Text: s.TextDelta})
		}
		if s.State == "DONE" && p.text[s.StepIndex] != "" {
			u.Events = append(u.Events, Event{Kind: "text", Text: p.text[s.StepIndex]})
			delete(p.text, s.StepIndex)
		}
	case "tool":
		id := "step-" + itoa(s.StepIndex)
		if !p.tools[s.StepIndex] {
			p.tools[s.StepIndex] = true
			u.Events = append(u.Events, Event{Kind: "tool_call", Tool: s.ToolName, ToolID: id, Input: clipRaw(s.ToolInfo)})
		}
		if s.State == "DONE" {
			u.Events = append(u.Events, Event{Kind: "tool_result", ToolID: id})
		}
	case "error_message":
		u.Events = append(u.Events, Event{Kind: "status", Text: "agy reported an error"})
	}
}
