package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// An approval waits at most this long. It's under the 24h MCP timeout set in the
// agent's config, so the agent gets a clean denial rather than a dropped call.
const approvalLimit = 23 * time.Hour

const maxApprovalBody = 16 << 20

// Approval is a tool call waiting on the owner.
type Approval struct {
	ID        int64  `json:"id"`
	AgentID   int64  `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Tool      string `json:"tool"`
	Input     string `json:"input"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	CreatedAt int64  `json:"created_at"`
	DecidedAt int64  `json:"decided_at"`
}

// decision is the owner's answer. Input replaces the tool's input when set, which
// is how AskUserQuestion answers get back to the agent.
type decision struct {
	Allow   bool            `json:"allow"`
	Message string          `json:"message"`
	Input   json.RawMessage `json:"input,omitempty"`
}

// mcpHandler serves the one route agents can reach. It stays on its own listener,
// away from the UI and API. An agent's token can create a request and wait: it can
// never decide one.
func (a *App) mcpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", a.handleMCP)
	return mux
}

func rpcReply(w http.ResponseWriter, id json.RawMessage, result any, rpcErr map[string]any) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ag, err := a.agentByToken(token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed) // no server-initiated stream
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
			Name            string `json:"name"`
			Arguments       struct {
				ToolName string          `json:"tool_name"`
				Input    json.RawMessage `json:"input"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxApprovalBody)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification needs no reply
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		rpcReply(w, req.ID, map[string]any{
			"protocolVersion": req.Params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "claudia", "version": "1"},
		}, nil)
	case "ping":
		rpcReply(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		rpcReply(w, req.ID, map[string]any{"tools": []any{map[string]any{
			"name": "approve", "description": "Ask the owner whether a tool call may run",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"tool_name": map[string]any{"type": "string"}, "input": map[string]any{"type": "object"}, "tool_use_id": map[string]any{"type": "string"},
			}, "required": []string{"tool_name", "input"}},
		}}}, nil)
	case "tools/call":
		if req.Params.Name != "approve" {
			rpcReply(w, req.ID, nil, map[string]any{"code": -32602, "message": "unknown tool"})
			return
		}
		d := a.requestApproval(r.Context(), ag, req.Params.Arguments.ToolName, req.Params.Arguments.Input)
		reply := map[string]any{"behavior": "deny", "message": d.Message}
		if d.Allow {
			input := d.Input
			if len(input) == 0 {
				input = req.Params.Arguments.Input
			}
			reply = map[string]any{"behavior": "allow", "updatedInput": input}
		}
		b, _ := json.Marshal(reply)
		rpcReply(w, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}, nil)
	default:
		rpcReply(w, req.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
	}
}

// requestApproval records a request and blocks until the owner decides, the limit
// passes or the agent goes away.
func (a *App) requestApproval(ctx context.Context, ag Agent, tool string, input json.RawMessage) decision {
	res, err := a.db.Exec(`INSERT INTO approvals(agent_id,tool,input,created_at) VALUES(?,?,?,?)`, ag.ID, tool, clipRaw(input), now())
	if err != nil {
		log.Printf("approval: %v", err)
		return decision{Message: "The control plane couldn't record this request"}
	}
	id, _ := res.LastInsertId()
	ch := make(chan decision, 1)
	a.mu.Lock()
	a.waiting[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.waiting, id)
		a.mu.Unlock()
	}()
	a.addEvent(ag.ID, 0, Event{Kind: "status", Text: "Waiting for your approval: " + tool})
	a.hub.publish(statePing(ag.ID))
	// Names the tool only: the input can hold secrets and this goes to a third party.
	a.notify(ag.Name+" needs approval", "Wants to use "+tool, fmt.Sprintf("#/approvals/%d", id))

	timer := time.NewTimer(approvalLimit)
	defer timer.Stop()
	select {
	case d := <-ch:
		return d
	case <-timer.C:
		a.closeApproval(id, "expired", "No decision within the time limit")
		return decision{Message: "The owner didn't respond in time"}
	case <-ctx.Done():
		a.closeApproval(id, "expired", "The agent stopped waiting")
		return decision{Message: "Cancelled"}
	}
}

func (a *App) pendingApprovals(agentID int64) int {
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE agent_id=? AND status='pending'`, agentID).Scan(&n); err != nil {
		log.Printf("approvals: %v", err)
	}
	return n
}

func (a *App) closeApproval(id int64, status, message string) {
	if _, err := a.db.Exec(`UPDATE approvals SET status=?,message=?,decided_at=? WHERE id=? AND status='pending'`, status, message, now(), id); err != nil {
		log.Printf("approval %d: %v", id, err)
	}
	a.hub.publish(statePing(0))
}

// decideApproval applies the owner's answer. It fails for a request no agent is
// waiting on any more.
func (a *App) decideApproval(id int64, d decision) error {
	a.mu.Lock()
	ch, ok := a.waiting[id]
	a.mu.Unlock()
	if !ok {
		return errors.New("that request is no longer waiting")
	}
	status := "denied"
	if d.Allow {
		status = "allowed"
	} else if d.Message == "" {
		d.Message = "The owner denied this"
	}
	res, err := a.db.Exec(`UPDATE approvals SET status=?,message=?,decided_at=? WHERE id=? AND status='pending'`, status, d.Message, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("that request has already been decided")
	}
	ch <- d
	a.hub.publish(statePing(0))
	return nil
}

func (a *App) approvals(pendingOnly bool) ([]Approval, error) {
	q := `SELECT p.id,p.agent_id,COALESCE(g.name,''),p.tool,p.input,p.status,p.message,p.created_at,p.decided_at
		FROM approvals p LEFT JOIN agents g ON g.id=p.agent_id`
	if pendingOnly {
		q += ` WHERE p.status='pending'`
	}
	rows, err := a.db.Query(q + ` ORDER BY p.id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		var p Approval
		if err := rows.Scan(&p.ID, &p.AgentID, &p.AgentName, &p.Tool, &p.Input, &p.Status, &p.Message, &p.CreatedAt, &p.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// notify posts to the owner's webhook, if there is one. Never blocks the caller.
func (a *App) notify(title, text, path string) {
	hook := a.setting("webhook_url")
	if hook == "" {
		return
	}
	link := ""
	if base := strings.TrimRight(a.cfg.PublicURL, "/"); base != "" {
		link = base + "/" + path
	}
	go func() {
		if err := postWebhook(hook, title, text, link); err != nil {
			log.Printf("webhook: %v", err) // the URL can hold a secret: log only the error
		}
	}()
}

func postWebhook(hook, title, text, link string) error {
	u, err := url.Parse(hook)
	if err != nil {
		return errors.New("invalid webhook URL")
	}
	var req *http.Request
	if strings.Contains(u.Host, "ntfy") {
		req, err = http.NewRequest(http.MethodPost, hook, strings.NewReader(text))
		if err == nil {
			req.Header.Set("Title", title)
			if link != "" {
				req.Header.Set("Click", link)
			}
		}
	} else {
		msg := title + ": " + text
		if link != "" {
			msg += "\n" + link
		}
		// Slack reads text and Discord reads content.
		b, _ := json.Marshal(map[string]string{"text": msg, "content": msg})
		req, err = http.NewRequest(http.MethodPost, hook, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return errors.New("couldn't build the webhook request")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err // url.Error's message includes the URL
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
