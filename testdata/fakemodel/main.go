// fakemodel is a local stand-in for model APIs, so the real CLIs can be tested
// with no keys and no spend. Dev only, never part of a deployment.
//
//	go run ./testdata/fakemodel -addr 10.77.0.1:9999 -log /tmp/fakemodel.jsonl
//
// The last user message picks the behaviour:
//
//	TOOL:<shell command>   ask to run the command, then answer "done"
//	SLOW                   stream text, stalling for a minute mid-response
//	ASK                    call AskUserQuestion, then answer "done"
//	anything else          answer "ok"
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	logMu   sync.Mutex
	logFile io.Writer = io.Discard
	nextID  atomic.Int64
	toolRe  = regexp.MustCompile(`TOOL:([^\n<"\\]+)`)
)

func record(v map[string]any) {
	b, _ := json.Marshal(v)
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintln(logFile, string(b))
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "listen address")
	logPath := flag.String("log", "", "append one JSON line per request to this file")
	flag.Parse()
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Fatal(err)
		}
		logFile = f
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", anthropic)
	mux.HandleFunc("POST /v1/responses", openai)
	mux.HandleFunc("/mcp", mcp)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "enerateContent") { // /v1beta/models/<model>:streamGenerateContent
			gemini(w, r)
			return
		}
		record(map[string]any{"unhandled": r.Method + " " + r.URL.Path})
		http.NotFound(w, r)
	})
	log.Printf("fakemodel listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func sse(w http.ResponseWriter, event string, v any) {
	b, _ := json.Marshal(v)
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	w.(http.Flusher).Flush()
}

// choose returns (tool command, slow) from the latest user text.
func choose(text string) (string, bool) {
	if m := toolRe.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1]), false
	}
	return "", strings.Contains(text, "SLOW")
}

type anthropicReq struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens int    `json:"max_tokens"`
	System    any    `json:"system"`
	Tools     []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func anthropic(w http.ResponseWriter, r *http.Request) {
	var req anthropicReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	last := ""
	hasResult := false
	if n := len(req.Messages); n > 0 {
		last = string(req.Messages[n-1].Content)
		hasResult = strings.Contains(last, `"tool_result"`)
	}
	sys, _ := json.Marshal(req.System)
	all, _ := json.Marshal(req.Messages)
	tools := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, t.Name)
	}
	record(map[string]any{
		"api": "anthropic", "model": req.Model, "stream": req.Stream, "max_tokens": req.MaxTokens,
		"tools": tools, "messages": len(req.Messages), "last": trim(last, 400),
		"claude_md_marker": strings.Contains(string(sys)+string(all), "CLAUDE_MD_MARKER"),
		"hook_marker":      strings.Contains(string(sys)+string(all), "HOOK_MARKER"),
	})
	id := fmt.Sprintf("msg_%d", nextID.Add(1))
	usage := map[string]any{"input_tokens": 100, "output_tokens": 1, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 10}
	cmd, slow := choose(last)
	ask := strings.Contains(last, "ASK") && !hasResult
	if hasResult {
		cmd, slow = "", false
	}
	text := "ok"
	if hasResult {
		text = "done"
	}
	if !req.Stream {
		block := map[string]any{"type": "text", "text": text}
		stop := "end_turn"
		if cmd != "" {
			block = map[string]any{"type": "tool_use", "id": "toolu_" + id, "name": "Bash", "input": map[string]any{"command": cmd, "description": "test"}}
			stop = "tool_use"
		}
		usage["output_tokens"] = 7
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{block}, "stop_reason": stop, "stop_sequence": nil, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant",
		"model": req.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage}})
	stop := "end_turn"
	if cmd != "" || ask {
		name := "Bash"
		input, _ := json.Marshal(map[string]any{"command": cmd, "description": "test"})
		if ask {
			name = "AskUserQuestion"
			input, _ = json.Marshal(map[string]any{"questions": []any{map[string]any{"question": "Which one?", "header": "Pick", "multiSelect": false,
				"options": []any{map[string]any{"label": "A", "description": "first"}, map[string]any{"label": "B", "description": "second"}}}}})
		}
		sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_" + id, "name": name, "input": map[string]any{}}})
		sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		stop = "tool_use"
	} else {
		sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		if slow {
			select {
			case <-time.After(time.Minute):
			case <-r.Context().Done():
				record(map[string]any{"api": "anthropic", "event": "client gone during SLOW"})
				return
			}
		}
	}
	sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	sse(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 7}})
	sse(w, "message_stop", map[string]any{"type": "message_stop"})
}

// mcp is a stub permission-prompt server. Query params: delay=<seconds> before
// answering, decision=allow|deny.
func mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
			Name            string `json:"name"`
			Arguments       struct {
				ToolName string         `json:"tool_name"`
				Input    map[string]any `json:"input"`
			} `json:"arguments"`
		} `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	record(map[string]any{"api": "mcp", "method": req.Method, "tool": req.Params.Name, "tool_name": req.Params.Arguments.ToolName, "input": req.Params.Arguments.Input})
	if len(req.ID) == 0 { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": req.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "claudia", "version": "0"}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "approve", "description": "Decide a permission request",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"tool_name": map[string]any{"type": "string"}, "input": map[string]any{"type": "object"}, "tool_use_id": map[string]any{"type": "string"}}}}}}
	case "tools/call":
		if d, err := time.ParseDuration(r.URL.Query().Get("delay") + "s"); err == nil && d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				record(map[string]any{"api": "mcp", "event": "client gave up", "after": d.String()})
				return
			}
		}
		decision := map[string]any{"behavior": "allow", "updatedInput": req.Params.Arguments.Input}
		if r.URL.Query().Get("decision") == "deny" {
			decision = map[string]any{"behavior": "deny", "message": "owner said no"}
		} else if req.Params.Arguments.ToolName == "AskUserQuestion" {
			decision["updatedInput"] = map[string]any{"questions": req.Params.Arguments.Input["questions"], "answers": map[string]any{"Which one?": "B"}}
		}
		b, _ := json.Marshal(decision)
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
		record(map[string]any{"api": "mcp", "event": "answered", "decision": decision})
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func openai(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
		Input []any  `json:"input"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(body, &req)
	tools := map[string]bool{}
	for _, t := range req.Tools {
		tools[t.Name] = true
	}
	lastItem := ""
	if n := len(req.Input); n > 0 {
		b, _ := json.Marshal(req.Input[n-1])
		lastItem = string(b)
	}
	record(map[string]any{"api": "openai", "model": req.Model, "items": len(req.Input), "tools": tools, "last": trim(lastItem, 400)})
	id := fmt.Sprintf("resp_%d", nextID.Add(1))
	cmd, _ := choose(lastItem)
	if strings.Contains(lastItem, "function_call_output") {
		cmd = ""
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
	var item map[string]any
	if cmd != "" {
		name, args := "shell_command", map[string]any{"command": cmd}
		if tools["exec_command"] {
			name, args = "exec_command", map[string]any{"cmd": cmd}
		}
		b, _ := json.Marshal(args)
		item = map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": "call_" + id, "name": name, "arguments": string(b), "status": "completed"}
	} else {
		item = map[string]any{"type": "message", "id": "m_" + id, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "ok", "annotations": []any{}}}}
	}
	sse(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	sse(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id,
		"usage": map[string]any{"input_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 40},
			"output_tokens": 7, "output_tokens_details": map[string]any{"reasoning_tokens": 2}, "total_tokens": 107}}})
}

func gemini(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	record(map[string]any{"api": "gemini?", "path": r.URL.Path, "last": trim(string(body), 300)})
	if !strings.Contains(r.URL.Path, ":streamGenerateContent") && !strings.Contains(r.URL.Path, ":generateContent") {
		http.NotFound(w, r)
		return
	}
	chunk := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ok"}}}, "finishReason": "STOP"}},
		"usageMetadata": map[string]any{"promptTokenCount": 100, "cachedContentTokenCount": 40, "candidatesTokenCount": 7, "thoughtsTokenCount": 2, "totalTokenCount": 109}}
	if strings.Contains(r.URL.Path, ":generateContent") {
		json.NewEncoder(w).Encode(chunk)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, "", chunk)
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
