package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type claudeAdapter struct{}

func (claudeAdapter) Modes(preset string) []string {
	// Auto mode isn't offered on generic gateways.
	if preset == "gateway" || preset == "gateway_other" {
		return []string{ModeAsk, ModeAllowlist, ModeFull}
	}
	return []string{ModeAsk, ModeReviewed, ModeAllowlist, ModeFull}
}

// On gateways Claude Code can't know the price: modelUsage reports costBasis
// "unknown". Those runs fall back to the price table.
func (claudeAdapter) ReportsUSD(preset string) bool {
	return preset != "gateway" && preset != "gateway_other"
}

func (claudeAdapter) LingersAfterResult() bool { return false }

func (claudeAdapter) Command(s RunSpec) ([]string, string) {
	// --setting-sources and --strict-mcp-config keep a repo's hooks, env block
	// and MCP servers out. Without them, -p runs those with no trust dialog, so
	// one agent's branch could run code in the next agent's turn.
	argv := []string{"claude", "-p", "--output-format=stream-json", "--verbose", "--include-partial-messages",
		"--setting-sources=user", "--strict-mcp-config"}
	if s.Resume {
		argv = append(argv, "--resume="+s.Session)
	} else {
		argv = append(argv, "--session-id="+s.Session)
	}
	if s.Agent.Model != "" {
		argv = append(argv, "--model="+s.Agent.Model)
	}
	if s.MaxUSD > 0 {
		argv = append(argv, fmt.Sprintf("--max-budget-usd=%.4f", s.MaxUSD))
	}
	switch s.Agent.Mode {
	case ModeAsk:
		// A 24h per-server timeout is what stops the 5-minute idle abort on HTTP
		// MCP calls. The control plane answers by itself at 23h.
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"claudia": map[string]any{
			"type": "http", "url": s.MCPURL, "timeout": 86400000,
			"headers": map[string]string{"Authorization": "Bearer " + s.Agent.Token},
		}}})
		argv = append(argv, "--permission-mode=manual", "--mcp-config", string(cfg), "--permission-prompt-tool=mcp__claudia__approve")
	case ModeReviewed:
		argv = append(argv, "--permission-mode=auto", "--permission-prompts=none")
	case ModeAllowlist:
		argv = append(argv, "--permission-mode=manual", "--permission-prompts=none")
		if s.Agent.AllowedTools != "" {
			argv = append(argv, "--allowedTools="+s.Agent.AllowedTools)
		}
	default:
		argv = append(argv, "--permission-mode=bypassPermissions")
	}
	return argv, s.Prompt // on stdin, because variadic flags can swallow a positional prompt
}

func (claudeAdapter) Parser(Agent) Parser { return &claudeParser{seen: map[string]int64{}} }

// claudeParser tracks usage per API response: one response can produce several
// assistant messages sharing an ID, and their output counts are placeholders
// until message_delta reports the real one.
type claudeParser struct {
	seen    map[string]int64 // message ID to output tokens counted so far
	current string           // message the stream events belong to
	model   string
}

type claudeUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *claudeUsage    `json:"usage"`
}

type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`

	Message *claudeMessage `json:"message"`
	Event   *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		Usage   *claudeUsage   `json:"usage"`
		Message *claudeMessage `json:"message"`
	} `json:"event"`

	// system/api_retry
	Error   string `json:"error"`
	Attempt int    `json:"attempt"`

	// rate_limit_event: a subscription's usage limits
	RateLimit *struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`

	// system/init
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`

	// result
	IsError        bool     `json:"is_error"`
	Result         string   `json:"result"`
	Errors         []string `json:"errors"`
	TerminalReason string   `json:"terminal_reason"`
	NumTurns       int      `json:"num_turns"`
	DurationMS     int64    `json:"duration_ms"`
	DurationAPIMS  int64    `json:"duration_api_ms"`
	ModelUsage     map[string]struct {
		Input      int64   `json:"inputTokens"`
		Output     int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		USD        float64 `json:"costUSD"`
		Basis      string  `json:"costBasis"`
	} `json:"modelUsage"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

func (p *claudeParser) Parse(line []byte) Update {
	var l claudeLine
	if json.Unmarshal(line, &l) != nil {
		return Update{}
	}
	u := Update{Session: l.SessionID}
	switch l.Type {
	case "stream_event":
		p.stream(&l, &u)
	case "assistant":
		if l.Message != nil {
			u.Live = p.count(l.Message)
			u.Events = assistantEvents(l.Message.Content)
		}
	case "user":
		if l.Message != nil {
			u.Events = toolResults(l.Message.Content)
		}
	case "system":
		switch l.Subtype {
		case "api_retry":
			u.Retry = l.Error
			u.Events = []Event{{Kind: "status", Text: fmt.Sprintf("Retrying the API call (attempt %d): %s", l.Attempt, l.Error)}}
		case "permission_denied":
			u.Events = []Event{{Kind: "status", Text: "Permission denied for a tool call"}}
		case "init":
			for _, s := range l.MCPServers {
				if s.Status != "connected" {
					u.Events = append(u.Events, Event{Kind: "status", Text: "MCP server " + s.Name + ": " + s.Status})
				}
			}
		}
	case "rate_limit_event":
		if l.RateLimit != nil && l.RateLimit.Status == "rejected" {
			u.Pause = PauseUsageLimit
		}
	case "result":
		u.Result = claudeResult(&l)
		text := l.Result
		if u.Result.Error != "" {
			text = u.Result.Error
		}
		u.Events = []Event{{Kind: "result", Text: text, IsError: l.IsError}}
	}
	return u
}

func claudeResult(l *claudeLine) *Result {
	r := &Result{
		OK: !l.IsError && l.Subtype == "success", Reason: l.TerminalReason,
		Turns: l.NumTurns, DurationMS: l.DurationMS, APIMS: l.DurationAPIMS,
		Cumulative: Usage{},
	}
	for m, t := range l.ModelUsage {
		r.Cumulative[m] = Tokens{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, USD: t.USD, Basis: t.Basis}
	}
	for _, d := range l.PermissionDenials {
		r.Denials = append(r.Denials, d.ToolName)
	}
	if !r.OK {
		switch {
		case l.IsError && l.Result != "":
			r.Error = l.Result
		case len(l.Errors) > 0:
			r.Error = strings.Join(l.Errors, "; ")
		default:
			r.Error = l.Subtype
		}
	}
	return r
}

func (p *claudeParser) stream(l *claudeLine, u *Update) {
	e := l.Event
	if e == nil {
		return
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			u.Live = p.count(e.Message)
		}
	case "content_block_delta":
		if e.Delta != nil && e.Delta.Type == "text_delta" {
			u.Events = []Event{{Kind: "delta", Text: e.Delta.Text}}
		}
	case "message_delta":
		if e.Usage == nil || p.current == "" {
			return
		}
		// Reports the real output so far. Count only what's new.
		if extra := e.Usage.Output - p.seen[p.current]; extra > 0 {
			p.seen[p.current] = e.Usage.Output
			u.Live = Usage{p.model: {Output: extra}}
		}
	}
}

// count adds a response's input and cache tokens once per message ID.
func (p *claudeParser) count(m *claudeMessage) Usage {
	if m.ID == "" || m.Usage == nil {
		return nil
	}
	if _, dup := p.seen[m.ID]; dup {
		return nil
	}
	// Starts at zero: the output count on this message is only a placeholder.
	p.seen[m.ID], p.current, p.model = 0, m.ID, m.Model
	return Usage{m.Model: {Input: m.Usage.Input, CacheRead: m.Usage.CacheRead, CacheWrite: m.Usage.CacheWrite}}
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func assistantEvents(content json.RawMessage) []Event {
	var blocks []claudeBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var out []Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, Event{Kind: "text", Text: b.Text})
		case "tool_use":
			out = append(out, Event{Kind: "tool_call", Tool: b.Name, ToolID: b.ID, Input: clipRaw(b.Input)})
		}
	}
	return out
}

func toolResults(content json.RawMessage) []Event {
	var blocks []claudeBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil // a plain string: the prompt or an injected message, not a tool result
	}
	var out []Event
	for _, b := range blocks {
		if b.Type == "tool_result" {
			out = append(out, Event{Kind: "tool_result", ToolID: b.ToolUseID, Text: clip(toolText(b.Content)), IsError: b.IsError})
		}
	}
	return out
}

// toolText flattens tool_result content, which is a string or a list of blocks.
func toolText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, c := range blocks {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}
