package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web
var webFS embed.FS

const (
	cookieName  = "claudia_session"
	sessionLife = 30 * 24 * time.Hour
	maxBody     = 4 << 20
)

// ---- live feed

// Message is one item on the live feed. ID is zero for items that aren't stored.
type Message struct {
	ID      int64           `json:"id,omitempty"`
	AgentID int64           `json:"agent_id"`
	RunID   int64           `json:"run_id,omitempty"`
	TS      int64           `json:"ts,omitempty"`
	Data    json.RawMessage `json:"data"`
}

// Hub fans messages out to open feeds. A feed that falls behind is closed, and the
// browser reconnects with Last-Event-ID, so it loses nothing that was stored.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Message]struct{}
}

func (h *Hub) subscribe() chan Message {
	ch := make(chan Message, 1024)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *Hub) publish(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- m:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func isStatePing(m Message) bool { return string(m.Data) == `{"kind":"state"}` }

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		apiError(w, http.StatusInternalServerError, "streaming isn't supported")
		return
	}
	agentID, _ := strconv.ParseInt(r.URL.Query().Get("agent"), 10, 64)
	last, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	sub := a.hub.subscribe() // before the backlog, so nothing falls in the gap
	defer a.hub.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(m Message) {
		b, _ := json.Marshal(m)
		if m.ID != 0 {
			fmt.Fprintf(w, "id: %d\n", m.ID)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	if agentID != 0 {
		backlog, err := a.eventsAfter(agentID, last, 500)
		if err != nil {
			log.Printf("events: %v", err)
		}
		for _, m := range backlog {
			send(m)
			last = m.ID
		}
	}
	fmt.Fprint(w, ": ready\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case m, open := <-sub:
			if !open {
				return
			}
			state := isStatePing(m)
			if agentID == 0 && !state {
				continue
			}
			if agentID != 0 && !state && m.AgentID != agentID {
				continue
			}
			if m.ID != 0 && m.ID <= last {
				continue
			}
			send(m)
			if m.ID != 0 {
				last = m.ID
			}
		}
		flusher.Flush()
	}
}

// ---- auth

func (a *App) sessionKey() []byte {
	k, _ := hex.DecodeString(a.setting("session_key"))
	return k
}

func (a *App) signSession(exp int64) string {
	mac := hmac.New(sha256.New, a.sessionKey())
	fmt.Fprint(mac, exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(mac.Sum(nil)))
}

func (a *App) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !ok || err != nil || exp < now() {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(a.signSession(exp)))
}

// isHTTPS decides the cookie's Secure flag. A spoofed header only makes it stricter.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	// ponytail: a global lockout, since behind a proxy every client shares an IP.
	// Someone hammering the login can lock you out for a minute at a time.
	a.mu.Lock()
	locked := time.Now().Before(a.loginLocked)
	a.mu.Unlock()
	if locked {
		apiError(w, http.StatusTooManyRequests, "Too many attempts. Try again in a minute.")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(a.setting("password_hash")), []byte(body.Password)) != nil {
		a.mu.Lock()
		a.loginFails++
		if a.loginFails >= 5 {
			a.loginFails = 0
			a.loginLocked = time.Now().Add(time.Minute)
		}
		a.mu.Unlock()
		apiError(w, http.StatusUnauthorized, "Wrong password")
		return
	}
	a.mu.Lock()
	a.loginFails = 0
	a.mu.Unlock()
	exp := time.Now().Add(sessionLife)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: a.signSession(exp.Unix()), Path: "/", Expires: exp,
		// Lax rather than Strict: Strict drops the cookie on links from notifications.
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- helpers

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "Invalid request body")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "Invalid ID")
		return 0, false
	}
	return id, true
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	apiError(w, http.StatusInternalServerError, "Something went wrong on the server")
}

// ---- routes

func (a *App) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", a.handleState)
	api.HandleFunc("GET /api/events", a.handleEvents)
	api.HandleFunc("GET /api/presets", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, presets) })
	api.HandleFunc("GET /api/modes", a.handleModes)

	api.HandleFunc("POST /api/agents", a.handleCreateAgent)
	api.HandleFunc("GET /api/agents/{id}", a.handleGetAgent)
	api.HandleFunc("PUT /api/agents/{id}", a.handleUpdateAgent)
	api.HandleFunc("DELETE /api/agents/{id}", a.handleDeleteAgent)
	api.HandleFunc("POST /api/agents/{id}/messages", a.handleMessage)
	api.HandleFunc("POST /api/agents/{id}/stop", a.handleStop)
	api.HandleFunc("POST /api/agents/{id}/reset", a.handleReset)
	api.HandleFunc("POST /api/agents/{id}/unpause", a.handleUnpause)
	api.HandleFunc("GET /api/agents/{id}/terminal", a.handleTerminal)
	api.HandleFunc("POST /api/stop-all", a.handleStopAll)

	api.HandleFunc("GET /api/tasks", a.handleTasks)
	api.HandleFunc("POST /api/tasks", a.handleCreateTask)
	api.HandleFunc("POST /api/tasks/{id}/retry", a.handleRetryTask)
	api.HandleFunc("DELETE /api/tasks/{id}", a.handleDeleteTask)
	api.HandleFunc("POST /api/queue", a.handleQueue)

	api.HandleFunc("GET /api/approvals", a.handleApprovals)
	api.HandleFunc("POST /api/approvals/{id}", a.handleDecide)

	api.HandleFunc("GET /api/credentials", a.handleCredentials)
	api.HandleFunc("POST /api/credentials", a.handleCreateCredential)
	api.HandleFunc("DELETE /api/credentials/{id}", a.handleDeleteCredential)
	api.HandleFunc("GET /api/prices", a.handlePrices)
	api.HandleFunc("PUT /api/prices", a.handleSetPrice)
	api.HandleFunc("DELETE /api/prices/{model}", a.handleDeletePrice)
	api.HandleFunc("GET /api/settings", a.handleSettings)
	api.HandleFunc("PUT /api/settings", a.handleSetSettings)
	api.HandleFunc("GET /api/metrics", a.handleMetrics)
	api.HandleFunc("GET /api/system", a.handleSystem)

	root := http.NewServeMux()
	root.HandleFunc("POST /api/login", a.handleLogin)
	root.HandleFunc("POST /api/logout", a.handleLogout)
	root.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			apiError(w, http.StatusUnauthorized, "Please log in")
			return
		}
		api.ServeHTTP(w, r)
	})
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // embedded at build time
	}
	root.Handle("/", http.FileServerFS(static))

	// Blocks cross-origin writes. WebSockets check their own origin.
	cop := http.NewCrossOriginProtection()
	return securityHeaders(cop.Handler(root))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// xterm.js injects <style> elements, hence unsafe-inline for styles only.
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ---- state

func (a *App) handleState(w http.ResponseWriter, _ *http.Request) {
	agents, err := a.agents()
	if err != nil {
		a.serverError(w, err)
		return
	}
	for i := range agents {
		agents[i].Running = a.isRunning(agents[i].ID)
	}
	pending, err := a.approvals(true)
	if err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"agents":             agents,
		"pending_approvals":  len(pending),
		"queue_paused":       a.setting("queue_paused") == "1",
		"dispatch_block":     a.dispatchBlock(),
		"spent_today":        a.spentSince(dayStart(), 0),
		"spent_month":        a.spentSince(monthStart(), 0),
		"budget_daily_usd":   a.settingFloat("budget_daily_usd"),
		"budget_monthly_usd": a.settingFloat("budget_monthly_usd"),
	})
}

func (a *App) handleModes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ad, ok := adapters[q.Get("cli")]
	if !ok {
		apiError(w, http.StatusBadRequest, "Unknown CLI")
		return
	}
	writeJSON(w, ad.Modes(q.Get("preset")))
}

// ---- agents

func (a *App) decodeAgent(w http.ResponseWriter, r *http.Request, ag *Agent) bool {
	var body struct {
		Name           string   `json:"name"`
		CLI            string   `json:"cli"`
		Model          string   `json:"model"`
		Mode           string   `json:"mode"`
		AllowedTools   string   `json:"allowed_tools"`
		CredentialID   int64    `json:"credential_id"`
		AcceptsQueue   *bool    `json:"accepts_queue"`
		MaxRunUSD      *float64 `json:"max_run_usd"`
		DailyBudgetUSD *float64 `json:"daily_budget_usd"`
		RunLimitSec    int      `json:"run_limit_sec"`
	}
	if !readJSON(w, r, &body) {
		return false
	}
	ag.Name, ag.Model, ag.Mode, ag.AllowedTools = strings.TrimSpace(body.Name), strings.TrimSpace(body.Model), body.Mode, strings.TrimSpace(body.AllowedTools)
	ag.CLI, ag.CredentialID, ag.RunLimitSec = body.CLI, body.CredentialID, body.RunLimitSec
	// Subscription agents stay out of the queue unless the owner opts in: plan
	// limits assume ordinary individual use.
	ag.AcceptsQueue = ag.CredentialID != 0
	if body.AcceptsQueue != nil {
		ag.AcceptsQueue = *body.AcceptsQueue
	}
	if body.MaxRunUSD != nil {
		ag.MaxRunUSD = *body.MaxRunUSD
	}
	if body.DailyBudgetUSD != nil {
		ag.DailyBudgetUSD = *body.DailyBudgetUSD
	}
	return true
}

func (a *App) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var ag Agent
	if !a.decodeAgent(w, r, &ag) {
		return
	}
	if err := a.createAgent(&ag); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.kick()
	writeJSON(w, ag)
}

func (a *App) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ag, err := a.agent(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "No such agent")
		return
	}
	ag.Running = a.isRunning(id)
	writeJSON(w, ag)
}

func (a *App) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ag, err := a.agent(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "No such agent")
		return
	}
	cli, session := ag.CLI, ag.Session
	if !a.decodeAgent(w, r, &ag) {
		return
	}
	ag.CLI, ag.Session = cli, session // a session belongs to one CLI, so that stays fixed
	if err := a.validateAgent(&ag); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	var cred any
	if ag.CredentialID != 0 {
		cred = ag.CredentialID
	}
	if _, err := a.db.Exec(`UPDATE agents SET name=?,model=?,mode=?,allowed_tools=?,credential_id=?,accepts_queue=?,max_run_usd=?,daily_budget_usd=?,run_limit_sec=? WHERE id=?`,
		ag.Name, ag.Model, ag.Mode, ag.AllowedTools, cred, ag.AcceptsQueue, ag.MaxRunUSD, ag.DailyBudgetUSD, ag.RunLimitSec, id); err != nil {
		apiError(w, http.StatusBadRequest, "Couldn't save the agent: "+err.Error())
		return
	}
	a.kick()
	writeJSON(w, ag)
}

func (a *App) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if a.isRunning(id) {
		apiError(w, http.StatusConflict, "Stop this agent first")
		return
	}
	if err := removeAgentContainer(r.Context(), id); err != nil {
		apiError(w, http.StatusInternalServerError, "Couldn't remove the container: "+err.Error())
		return
	}
	// Runs stay, so cost history survives the agent.
	if _, err := a.db.Exec(`DELETE FROM agents WHERE id=?`, id); err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		apiError(w, http.StatusBadRequest, "Message is empty")
		return
	}
	ag, err := a.agent(id)
	if err != nil {
		apiError(w, http.StatusNotFound, "No such agent")
		return
	}
	for _, why := range []string{ag.PauseReason, a.budgetBlock(), a.preflight(ag)} {
		if why != "" {
			apiError(w, http.StatusConflict, why)
			return
		}
	}
	run, err := a.reserve(id)
	if err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	go func() {
		defer a.release(run)
		if ag.Subscription() {
			if why := a.signedInCheck(ag.ID); why != "" {
				a.addEvent(ag.ID, 0, Event{Kind: "error", Text: why})
				return
			}
		}
		o := a.execRun(run, Job{Agent: ag, Prompt: body.Text, Display: body.Text, Cwd: agentHome + "/work/chat", Session: ag.Session})
		if o.Pause != "" {
			a.pauseAgent(ag.ID, o.Pause+": "+o.Error)
		}
	}()
	writeJSON(w, map[string]bool{"ok": true})
}

// signedInCheck confirms a subscription agent has signed in, before a run fails
// with something less clear.
func (a *App) signedInCheck(id int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := a.ensureContainer(ctx, id); err != nil {
		return "Couldn't start the container: " + err.Error()
	}
	out, err := docker(ctx, "exec", containerName(id), "claude", "auth", "status", "--json")
	var st struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err != nil || json.Unmarshal([]byte(out), &st) != nil || !st.LoggedIn {
		return "This agent isn't signed in. Open its terminal tab and run: claude auth login"
	}
	return ""
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !a.stopRun(id, RunStopped) {
		apiError(w, http.StatusConflict, "This agent isn't running")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleStopAll(w http.ResponseWriter, _ *http.Request) {
	a.stopAll(RunStopped)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleReset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if a.isRunning(id) {
		apiError(w, http.StatusConflict, "Stop this agent first")
		return
	}
	if _, err := a.db.Exec(`UPDATE agents SET session='' WHERE id=?`, id); err != nil {
		a.serverError(w, err)
		return
	}
	a.addEvent(id, 0, Event{Kind: "status", Text: "Started a new conversation"})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleUnpause(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.db.Exec(`UPDATE agents SET pause_reason='' WHERE id=?`, id); err != nil {
		a.serverError(w, err)
		return
	}
	a.kick()
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- queue

func (a *App) handleTasks(w http.ResponseWriter, _ *http.Request) {
	tasks, err := a.tasks()
	if err != nil {
		a.serverError(w, err)
		return
	}
	agents, err := a.agents()
	if err != nil {
		a.serverError(w, err)
		return
	}
	// Say so when a ready task has nobody who could ever take it.
	for i, t := range tasks {
		if t.View != TaskQueued {
			continue
		}
		found := false
		for _, ag := range agents {
			found = found || eligible(ag, t)
		}
		if !found {
			tasks[i].Note = "No eligible agent"
		}
	}
	writeJSON(w, map[string]any{"tasks": tasks, "paused": a.setting("queue_paused") == "1", "block": a.dispatchBlock()})
}

func (a *App) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var t Task
	if !readJSON(w, r, &t) {
		return
	}
	if err := a.createTask(&t); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, t)
}

func (a *App) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.retryTask(id); err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.deleteTask(id); err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	v := "0"
	if body.Paused {
		v = "1"
	}
	if err := a.setSetting("queue_paused", v); err != nil {
		a.serverError(w, err)
		return
	}
	a.kick()
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- approvals

func (a *App) handleApprovals(w http.ResponseWriter, r *http.Request) {
	list, err := a.approvals(r.URL.Query().Get("pending") == "1")
	if err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, list)
}

func (a *App) handleDecide(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var d decision
	if !readJSON(w, r, &d) {
		return
	}
	if err := a.decideApproval(id, d); err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- settings

func (a *App) handleCredentials(w http.ResponseWriter, _ *http.Request) {
	list, err := a.credentials()
	if err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, list)
}

func (a *App) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Preset string `json:"preset"`
		Bundle
	}
	if !readJSON(w, r, &body) {
		return
	}
	id, err := a.addCredential(strings.TrimSpace(body.Name), body.Preset, body.Bundle)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (a *App) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var used int
	if a.db.QueryRow(`SELECT 1 FROM agents WHERE credential_id=? UNION SELECT 1 FROM tasks WHERE git_credential_id=? AND status IN ('queued','running') LIMIT 1`, id, id).Scan(&used) == nil {
		apiError(w, http.StatusConflict, "An agent or a pending task still uses this credential")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM credentials WHERE id=?`, id); err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handlePrices(w http.ResponseWriter, _ *http.Request) {
	p, err := a.prices()
	if err != nil {
		a.serverError(w, err)
		return
	}
	list := make([]Price, 0, len(p))
	for _, v := range p {
		list = append(list, v)
	}
	writeJSON(w, list)
}

func (a *App) handleSetPrice(w http.ResponseWriter, r *http.Request) {
	var p Price
	if !readJSON(w, r, &p) {
		return
	}
	if err := a.setPrice(p); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleDeletePrice(w http.ResponseWriter, r *http.Request) {
	if _, err := a.db.Exec(`DELETE FROM prices WHERE model=?`, r.PathValue("model")); err != nil {
		a.serverError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

var ownerSettings = []string{"budget_daily_usd", "budget_monthly_usd", "webhook_url"}

func (a *App) handleSettings(w http.ResponseWriter, _ *http.Request) {
	out := map[string]string{}
	for _, k := range ownerSettings {
		out[k] = a.setting(k)
	}
	writeJSON(w, out)
}

func (a *App) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if !readJSON(w, r, &body) {
		return
	}
	for _, k := range []string{"budget_daily_usd", "budget_monthly_usd"} {
		if v, ok := body[k]; ok && v != "" {
			if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 {
				apiError(w, http.StatusBadRequest, "Budgets must be zero or more")
				return
			}
		}
	}
	if v := body["webhook_url"]; v != "" && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
		apiError(w, http.StatusBadRequest, "The webhook must be an http(s) URL")
		return
	}
	for _, k := range ownerSettings {
		if v, ok := body[k]; ok {
			if err := a.setSetting(k, v); err != nil {
				a.serverError(w, err)
				return
			}
		}
	}
	a.enforceBudgets()
	a.kick()
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- system

func (a *App) handleSystem(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"containers": []any{}}
	var st syscall.Statfs_t
	if syscall.Statfs(a.cfg.DataDir, &st) == nil {
		out["disk_free"] = st.Bavail * uint64(st.Bsize)
		out["disk_total"] = st.Blocks * uint64(st.Bsize)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// Live only, never stored.
	cmd := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}")
	b, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			log.Printf("docker stats: %v", err)
		}
		writeJSON(w, out)
		return
	}
	list := []map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		name, _ := row["Name"].(string)
		if !strings.HasPrefix(name, "claudia-agent-") {
			continue
		}
		cpu, _ := row["CPUPerc"].(string)
		mem, _ := row["MemUsage"].(string)
		list = append(list, map[string]string{"name": name, "cpu": cpu, "mem": mem})
	}
	out["containers"] = list
	writeJSON(w, out)
}
