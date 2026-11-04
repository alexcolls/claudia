package main

import (
	"encoding/json"
	"strings"
)

type codexAdapter struct{}

// Full auto only. Codex's workspace-write sandbox keeps the network off and may
// not work in a container, and OpenAI's docs advise danger-full-access inside
// Docker, where the container is the boundary.
func (codexAdapter) Modes(string) []string { return []string{ModeFull} }

func (codexAdapter) ReportsUSD(string) bool { return false }

func (codexAdapter) LingersAfterResult() bool { return false }

func (codexAdapter) Command(s RunSpec) ([]string, string) {
	argv := []string{"codex", "exec"}
	if s.Resume {
		argv = append(argv, "resume")
	}
	argv = append(argv, "--json", "--skip-git-repo-check")
	if !s.Resume { // resume keeps the sandbox of the original session
		argv = append(argv, "--sandbox", "danger-full-access")
	}
	if s.Agent.Model != "" {
		argv = append(argv, "--model", s.Agent.Model)
	}
	if s.Resume {
		argv = append(argv, s.Session)
	}
	return append(argv, "-"), s.Prompt
}

func (codexAdapter) Parser(a Agent) Parser { return &codexParser{model: a.Model} }

// codexParser needs the agent's model because Codex's stream doesn't name one.
type codexParser struct{ model string }

type codexLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Item     *struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Text     string `json:"text"`
		Message  string `json:"message"`
		Command  string `json:"command"`
		Output   string `json:"aggregated_output"`
		ExitCode *int   `json:"exit_code"`
	} `json:"item"`
	Usage *struct {
		Input      int64 `json:"input_tokens"` // includes cached_input_tokens
		Cached     int64 `json:"cached_input_tokens"`
		CacheWrite int64 `json:"cache_write_input_tokens"`
		Output     int64 `json:"output_tokens"` // includes reasoning_output_tokens
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *codexParser) Parse(line []byte) Update {
	var l codexLine
	if json.Unmarshal(line, &l) != nil {
		return Update{}
	}
	u := Update{Session: l.ThreadID}
	switch l.Type {
	case "item.started":
		if l.Item != nil && l.Item.Type == "command_execution" {
			u.Events = []Event{{Kind: "tool_call", Tool: "shell", ToolID: l.Item.ID, Input: clip(l.Item.Command)}}
		}
	case "item.completed":
		if l.Item != nil {
			u.Events = codexItem(l)
		}
	case "turn.completed":
		// Cumulative for the thread, including across a resume.
		r := &Result{OK: true, Reason: "completed", Turns: 1, Cumulative: Usage{}}
		if l.Usage != nil {
			r.Cumulative[p.model] = Tokens{
				Input: l.Usage.Input - l.Usage.Cached, Output: l.Usage.Output,
				CacheRead: l.Usage.Cached, CacheWrite: l.Usage.CacheWrite,
			}
		}
		u.Result = r
		u.Events = []Event{{Kind: "result"}}
	case "turn.failed":
		msg := "turn failed"
		if l.Error != nil {
			msg = l.Error.Message
		}
		u.Result = &Result{Reason: "failed", Error: msg}
		u.Events = []Event{{Kind: "result", Text: msg, IsError: true}}
	case "error":
		// Not fatal. Codex loops on these with no end if the model is unreachable,
		// so the run's wall-clock limit is what stops it.
		if strings.HasPrefix(l.Message, "Reconnecting") {
			u.Retry = "reconnecting"
		}
		u.Events = []Event{{Kind: "status", Text: l.Message}}
	}
	return u
}

func codexItem(l codexLine) []Event {
	it := l.Item
	switch it.Type {
	case "agent_message":
		return []Event{{Kind: "text", Text: it.Text}}
	case "command_execution":
		return []Event{{Kind: "tool_result", ToolID: it.ID, Text: clip(it.Output), IsError: it.ExitCode != nil && *it.ExitCode != 0}}
	case "error":
		return []Event{{Kind: "status", Text: it.Message}}
	case "reasoning":
		return nil
	}
	return []Event{{Kind: "tool_call", Tool: it.Type, ToolID: it.ID}}
}
