package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	a, err := newApp(Config{DataDir: t.TempDir(), MCPURL: "http://x/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Close() })
	if err := a.setupPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	return a
}

func cred(t *testing.T, a *App, name, preset string, env map[string]string) int64 {
	t.Helper()
	id, err := a.addCredential(name, preset, Bundle{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func apiAgent(t *testing.T, a *App, name, cli, mode string, credID int64, model string) Agent {
	t.Helper()
	ag := Agent{Name: name, CLI: cli, Mode: mode, CredentialID: credID, Model: model, AcceptsQueue: true}
	if err := a.createAgent(&ag); err != nil {
		t.Fatal(err)
	}
	return ag
}

func TestClaudeTokensAreRejected(t *testing.T) {
	a := testApp(t)
	for _, env := range []map[string]string{
		{"CLAUDE_CODE_OAUTH_TOKEN": "anything"},
		{"ANTHROPIC_API_KEY": "sk-ant-oat01-abc"},
		{"OTHER_NAME": "sk-ant-oat01-abc"},
	} {
		if _, err := a.addCredential("x", "anthropic", Bundle{Env: env}); err == nil {
			t.Errorf("should reject %v", env)
		}
	}
	if _, err := a.addCredential("bad", "anthropic", Bundle{Env: map[string]string{"bad name": "x"}}); err == nil {
		t.Error("should reject an invalid variable name")
	}
}

func TestCredentialsEncryptedAtRest(t *testing.T) {
	a := testApp(t)
	secret := "sk-ant-api-THIS-IS-THE-SECRET"
	id := cred(t, a, "main", "anthropic", map[string]string{"ANTHROPIC_API_KEY": secret})
	var box []byte
	if err := a.db.QueryRow(`SELECT secret FROM credentials WHERE id=?`, id).Scan(&box); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(box, []byte(secret)) {
		t.Error("secret is stored in plain text")
	}
	b, err := a.bundle(id)
	if err != nil || b.Env["ANTHROPIC_API_KEY"] != secret {
		t.Errorf("round trip: %v %v", b, err)
	}
	list, err := a.credentials()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(list)
	if bytes.Contains(out, []byte(secret)) {
		t.Error("API leaks the secret")
	}
	if !bytes.Contains(out, []byte("ANTHROPIC_API_KEY")) {
		t.Error("API should list variable names")
	}
	info, err := os.Stat(filepath.Join(a.cfg.DataDir, "master.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("master key mode: %v %v", info.Mode(), err)
	}
	// A different key can't read it.
	if _, err := unseal(make([]byte, 32), box); err == nil {
		t.Error("decrypted with the wrong key")
	}
}

func TestAgentValidation(t *testing.T) {
	a := testApp(t)
	c := cred(t, a, "c", "anthropic", map[string]string{"ANTHROPIC_API_KEY": "k"})
	o := cred(t, a, "o", "openai", map[string]string{"CODEX_API_KEY": "k"})
	gw := cred(t, a, "g", "gateway", map[string]string{"ANTHROPIC_BASE_URL": "http://x", "ANTHROPIC_AUTH_TOKEN": "k"})
	bad := []Agent{
		{Name: "", CLI: "claude", Mode: ModeFull, CredentialID: c},
		{Name: "a", CLI: "claude", Mode: ModeFull, CredentialID: o}, // wrong provider for this CLI
		{Name: "a", CLI: "codex", Mode: ModeAsk, CredentialID: o, Model: "m"},
		{Name: "a", CLI: "codex", Mode: ModeFull, CredentialID: o}, // can't be priced with no model
		{Name: "a", CLI: "codex", Mode: ModeFull, Model: "m"},      // subscription is Claude Code only
		{Name: "a", CLI: "claude", Mode: ModeReviewed, CredentialID: gw, Model: "m"},
		{Name: "a", CLI: "claude", Mode: ModeFull, CredentialID: c, Model: "--evil"},
		{Name: "a", CLI: "claude", Mode: ModeAllowlist, CredentialID: c},
		{Name: "a", CLI: "nope", Mode: ModeFull},
	}
	for i, ag := range bad {
		if err := a.validateAgent(&ag); err == nil {
			t.Errorf("case %d should fail: %+v", i, ag)
		}
	}
	sub := Agent{Name: "s", CLI: "claude", Mode: ModeFull}
	if err := a.validateAgent(&sub); err != nil || !sub.Subscription() {
		t.Errorf("subscription agent: %v", err)
	}
}

func TestValidateRepo(t *testing.T) {
	for _, bad := range []string{"ext::sh -c id", "file:///etc", "git@github.com:a/b.git", "-upload-pack=x", "https://user:pw@github.com/a/b", "https://a b/c", ""} {
		if validateRepo(bad, "main") == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, bad := range []string{"-x", "a..b", "a b", ""} {
		if validateRepo("https://github.com/a/b", bad) == nil {
			t.Errorf("branch %q should be rejected", bad)
		}
	}
	if err := validateRepo("https://github.com/a/b.git", "release/1.2"); err != nil {
		t.Error(err)
	}
}

func addTask(t *testing.T, a *App, title string, deps ...int64) Task {
	t.Helper()
	task := Task{Title: title, Repo: "https://github.com/a/b", Prompt: "do " + title, Deps: deps}
	if err := a.createTask(&task); err != nil {
		t.Fatal(err)
	}
	return task
}

func setStatus(t *testing.T, a *App, id int64, status string) {
	t.Helper()
	if _, err := a.db.Exec(`UPDATE tasks SET status=? WHERE id=?`, status, id); err != nil {
		t.Fatal(err)
	}
}

func readyIDs(t *testing.T, a *App) map[int64]bool {
	t.Helper()
	list, err := a.readyTasks()
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]bool{}
	for _, x := range list {
		m[x.ID] = true
	}
	return m
}

func views(t *testing.T, a *App) map[int64]string {
	t.Helper()
	list, err := a.tasks()
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]string{}
	for _, x := range list {
		m[x.ID] = x.View
	}
	return m
}

func TestQueueDependencies(t *testing.T) {
	a := testApp(t)
	x := addTask(t, a, "a")
	y := addTask(t, a, "b")
	z := addTask(t, a, "c", x.ID, y.ID)
	if r := readyIDs(t, a); !r[x.ID] || !r[y.ID] || r[z.ID] {
		t.Errorf("only the roots are ready: %v", r)
	}
	if views(t, a)[z.ID] != "waiting" {
		t.Error("c should be waiting")
	}
	setStatus(t, a, x.ID, TaskDone)
	if readyIDs(t, a)[z.ID] {
		t.Error("c still needs b")
	}
	setStatus(t, a, y.ID, TaskFailed)
	if v := views(t, a)[z.ID]; v != "blocked" {
		t.Errorf("a failed dependency should block c, got %s", v)
	}
	if readyIDs(t, a)[z.ID] {
		t.Error("a blocked task must not run")
	}
	// no_changes blocks too: nothing was produced to build on.
	setStatus(t, a, y.ID, TaskNoChanges)
	if v := views(t, a)[z.ID]; v != "blocked" {
		t.Errorf("no_changes should block c, got %s", v)
	}
	if err := a.retryTask(y.ID); err != nil {
		t.Fatal(err)
	}
	if v := views(t, a)[z.ID]; v != "waiting" {
		t.Errorf("after a retry c waits again, got %s", v)
	}
	setStatus(t, a, y.ID, TaskDone)
	if !readyIDs(t, a)[z.ID] {
		t.Error("c should be ready")
	}
	if err := a.retryTask(z.ID); err == nil {
		t.Error("a queued task can't be retried")
	}
	if err := a.deleteTask(x.ID); err == nil {
		t.Error("can't delete a task others depend on")
	}
	bad := Task{Repo: "https://github.com/a/b", Prompt: "p", Deps: []int64{999}}
	if err := a.createTask(&bad); err == nil {
		t.Error("a missing dependency must be rejected")
	}
	setStatus(t, a, z.ID, TaskRunning)
	if err := a.deleteTask(z.ID); err == nil {
		t.Error("can't delete a running task")
	}
}

func TestPickAgent(t *testing.T) {
	idle := func(int64) bool { return false }
	busy := func(id int64) map[int64]bool { return map[int64]bool{id: true} }
	agents := []Agent{
		{ID: 1, CLI: "claude", Preset: "anthropic", AcceptsQueue: true},
		{ID: 2, CLI: "codex", Preset: "openai", AcceptsQueue: true},
		{ID: 3, CLI: "claude", Preset: "bedrock", AcceptsQueue: false},
		{ID: 4, CLI: "claude", Preset: "vertex", AcceptsQueue: true, PauseReason: "billing"},
	}
	cases := []struct {
		name string
		t    Task
		want int64
		busy map[int64]bool
	}{
		{"first idle", Task{}, 1, nil},
		{"CLI filter", Task{CLI: "codex"}, 2, nil},
		{"provider filter", Task{Preset: "anthropic"}, 1, nil},
		{"out of the queue", Task{Preset: "bedrock"}, 0, nil},
		{"paused", Task{Preset: "vertex"}, 0, nil},
		{"next idle", Task{}, 2, busy(1)},
		{"keeps its agent", Task{AgentID: 2}, 2, nil},
		{"waits for its busy agent", Task{AgentID: 2}, 0, busy(2)},
		{"moves on if its agent is gone", Task{AgentID: 99}, 1, nil},
		{"moves on if its agent is paused", Task{AgentID: 4}, 1, nil},
	}
	_ = idle
	for _, c := range cases {
		got, ok := pickAgent(c.t, agents, func(id int64) bool { return c.busy[id] })
		if c.want == 0 && ok {
			t.Errorf("%s: should pick nobody, got %d", c.name, got.ID)
		}
		if c.want != 0 && (!ok || got.ID != c.want) {
			t.Errorf("%s: want %d, got %d (%v)", c.name, c.want, got.ID, ok)
		}
	}
}

func TestTaskAfterRun(t *testing.T) {
	first := Task{Attempts: 1, MaxAttempts: 2}
	last := Task{Attempts: 2, MaxAttempts: 2}
	cases := []struct {
		name   string
		t      Task
		o      Outcome
		status string
		refund bool
	}{
		{"retries", first, Outcome{Status: RunFailed}, TaskQueued, false},
		{"gives up", last, Outcome{Status: RunFailed}, TaskFailed, false},
		{"auth failure keeps its retry", last, Outcome{Status: RunFailed, Pause: "authentication failed"}, TaskQueued, true},
		{"budget stop keeps its retry", last, Outcome{Status: RunBudgetStopped}, TaskQueued, true},
		{"owner's stop sticks", first, Outcome{Status: RunStopped}, TaskFailed, false},
		{"timeout retries", first, Outcome{Status: RunTimedOut}, TaskQueued, false},
	}
	for _, c := range cases {
		s, _, r := taskAfterRun(c.t, c.o)
		if s != c.status || r != c.refund {
			t.Errorf("%s: got %s refund=%v", c.name, s, r)
		}
	}
}

func TestTaskPrompt(t *testing.T) {
	task := Task{Prompt: "fix it", Error: "boom\nmore"}
	p := taskPrompt(task, "/d", "b", []string{"abc (branch x)"}, true)
	for _, want := range []string{"CLAUDE.md", "Don't push", "/d", "abc (branch x)", "fix it", "boom"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "more") {
		t.Error("only the first line of the last error belongs in the prompt")
	}
}

func TestBudgets(t *testing.T) {
	a := testApp(t)
	o := cred(t, a, "o", "openai", map[string]string{"CODEX_API_KEY": "k"})
	c := cred(t, a, "c", "anthropic", map[string]string{"ANTHROPIC_API_KEY": "k"})
	known := apiAgent(t, a, "known", "codex", ModeFull, o, "gpt-6.1-sol")
	unknown := apiAgent(t, a, "unknown", "codex", ModeFull, o, "gpt-unknown")
	claude := apiAgent(t, a, "claude", "claude", ModeFull, c, "")
	sub := apiAgent(t, a, "sub", "claude", ModeFull, 0, "")
	if err := a.seedPrices(); err != nil {
		t.Fatal(err)
	}
	// No budget set: nothing to protect yet.
	if why := a.priceBlock(unknown); why != "" {
		t.Errorf("no budget, no block: %s", why)
	}
	if err := a.setSetting("budget_daily_usd", "1"); err != nil {
		t.Fatal(err)
	}
	if why := a.priceBlock(unknown); why == "" {
		t.Error("an unpriced model must be blocked while a budget is active")
	}
	for _, ag := range []Agent{known, claude, sub} {
		if why := a.priceBlock(ag); why != "" {
			t.Errorf("%s shouldn't be blocked: %s", ag.Name, why)
		}
	}
	if why := a.budgetBlock(); why != "" {
		t.Errorf("nothing spent yet: %s", why)
	}
	if _, err := a.db.Exec(`INSERT INTO runs(agent_id,usd,started_at) VALUES(?,?,?)`, known.ID, 1.5, now()); err != nil {
		t.Fatal(err)
	}
	if why := a.budgetBlock(); !strings.Contains(why, "Daily budget") {
		t.Errorf("want a daily block, got %q", why)
	}
	if why := a.dispatchBlock(); why == "" {
		t.Error("the queue should stop at the cap")
	}
	if err := a.setSetting("budget_daily_usd", "0"); err != nil {
		t.Fatal(err)
	}
	if why := a.budgetBlock(); why != "" {
		t.Errorf("no cap now: %q", why)
	}
	saved := minFreeBytes
	minFreeBytes = 1 << 62
	if why := a.dispatchBlock(); !strings.Contains(why, "disk") {
		t.Errorf("low disk should stop dispatch: %q", why)
	}
	minFreeBytes = 0
	if why := a.dispatchBlock(); why != "" {
		t.Errorf("nothing should block: %q", why)
	}
	minFreeBytes = saved
	if err := a.setSetting("queue_paused", "1"); err != nil {
		t.Fatal(err)
	}
	if why := a.dispatchBlock(); why == "" {
		t.Error("a paused queue blocks dispatch")
	}
	known.DailyBudgetUSD = 1
	if why := a.agentBudgetBlock(known); why == "" {
		t.Error("agent budget should block")
	}
	claude.DailyBudgetUSD = 1
	if why := a.agentBudgetBlock(claude); why != "" {
		t.Errorf("another agent's spend doesn't count: %q", why)
	}
}

func rpc(t *testing.T, srv *httptest.Server, token, method string, params any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func waitPending(t *testing.T, a *App) Approval {
	t.Helper()
	for i := 0; i < 200; i++ {
		list, err := a.approvals(true)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 {
			return list[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending approval")
	return Approval{}
}

func callReply(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	var r map[string]any
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestApprovals(t *testing.T) {
	a := testApp(t)
	ag := apiAgent(t, a, "agent", "claude", ModeAsk, 0, "")
	srv := httptest.NewServer(a.mcpHandler())
	defer srv.Close()

	if code, _ := rpc(t, srv, "", "tools/list", nil); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := rpc(t, srv, "wrong", "tools/list", nil); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code, out := rpc(t, srv, ag.Token, "initialize", map[string]any{"protocolVersion": "2025-06-18"}); code != 200 || out["result"] == nil {
		t.Errorf("initialize: %d %v", code, out)
	}
	if _, out := rpc(t, srv, ag.Token, "nonsense", nil); out["error"] == nil {
		t.Errorf("unknown method: %v", out)
	}

	// The MCP listener has no way to decide. Its token only opens /mcp.
	for _, path := range []string{"/api/approvals/1", "/api/state", "/api/stop-all"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{"allow":true}`))
		req.Header.Set("Authorization", "Bearer "+ag.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s on the agent listener: %d", path, resp.StatusCode)
		}
	}

	call := func(tool string, input any) chan map[string]any {
		ch := make(chan map[string]any, 1)
		go func() {
			_, out := rpc(t, srv, ag.Token, "tools/call", map[string]any{"name": "approve", "arguments": map[string]any{"tool_name": tool, "input": input}})
			ch <- out
		}()
		return ch
	}

	ch := call("Bash", map[string]any{"command": "ls"})
	p := waitPending(t, a)
	if p.Tool != "Bash" || !strings.Contains(p.Input, "ls") || p.AgentName != "agent" {
		t.Errorf("pending: %+v", p)
	}
	select {
	case <-ch:
		t.Fatal("must block until the owner decides")
	case <-time.After(50 * time.Millisecond):
	}
	if err := a.decideApproval(p.ID, decision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	r := callReply(t, <-ch)
	if r["behavior"] != "allow" || r["updatedInput"].(map[string]any)["command"] != "ls" {
		t.Errorf("allow: %v", r)
	}
	if err := a.decideApproval(p.ID, decision{Allow: true}); err == nil {
		t.Error("can't decide twice")
	}

	ch = call("Bash", map[string]any{"command": "rm"})
	p = waitPending(t, a)
	if err := a.decideApproval(p.ID, decision{Message: "no way"}); err != nil {
		t.Fatal(err)
	}
	if r := callReply(t, <-ch); r["behavior"] != "deny" || r["message"] != "no way" {
		t.Errorf("deny: %v", r)
	}

	// Answers to a question come back as the tool's input.
	ch = call("AskUserQuestion", map[string]any{"questions": []any{}})
	p = waitPending(t, a)
	if err := a.decideApproval(p.ID, decision{Allow: true, Input: json.RawMessage(`{"questions":[],"answers":{"q":"B"}}`)}); err != nil {
		t.Fatal(err)
	}
	if r := callReply(t, <-ch); r["updatedInput"].(map[string]any)["answers"].(map[string]any)["q"] != "B" {
		t.Errorf("answers: %v", r)
	}

	// An agent that stops waiting leaves nothing to decide.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan decision, 1)
	go func() { done <- a.requestApproval(ctx, ag, "Bash", json.RawMessage(`{}`)) }()
	p = waitPending(t, a)
	cancel()
	<-done
	if err := a.decideApproval(p.ID, decision{Allow: true}); err == nil {
		t.Error("a request nobody waits on can't be decided")
	}
	if list, _ := a.approvals(true); len(list) != 0 {
		t.Errorf("should have expired: %+v", list)
	}
}

func TestReconcile(t *testing.T) {
	a := testApp(t)
	a.db.Exec(`INSERT INTO tasks(id,title,repo,prompt,status,attempts,created_at,updated_at) VALUES(1,'t','https://x/y','p','running',1,0,0)`)
	a.db.Exec(`INSERT INTO approvals(id,agent_id,tool,input,created_at) VALUES(1,1,'Bash','{}',0)`)
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	a.db.QueryRow(`SELECT status,attempts FROM tasks WHERE id=1`).Scan(&status, &attempts)
	if status != TaskQueued || attempts != 0 {
		t.Errorf("task after restart: %s attempts=%d", status, attempts)
	}
	if list, _ := a.approvals(true); len(list) != 0 {
		t.Error("pending approvals must expire on restart")
	}
}

func TestReadLineLongLines(t *testing.T) {
	long := strings.Repeat("x", 3<<20) // past Scanner's limit and the reader's buffer
	input := long + "\nshort\nlast"
	br := newTestReader(input)
	got, err := readLine(br)
	if err != nil || len(got) != len(long)+1 {
		t.Fatalf("long line: %d %v", len(got), err)
	}
	got, _ = readLine(br)
	if string(got) != "short\n" {
		t.Errorf("next line: %q", got)
	}
	got, err = readLine(br)
	if string(got) != "last" || err != io.EOF {
		t.Errorf("last line: %q %v", got, err)
	}
}

func TestWebAuth(t *testing.T) {
	a := testApp(t)
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	post := func(path, body string, cookie *http.Cookie, origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("static page: %d csp=%q", resp.StatusCode, csp)
	}
	for _, p := range []string{"/api/state", "/api/credentials", "/api/events", "/api/agents/1/terminal"} {
		r, err := client.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d", p, r.StatusCode)
		}
	}
	if r := post("/api/login", `{"password":"nope"}`, nil, ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", r.StatusCode)
	}
	r := post("/api/login", `{"password":"correct horse"}`, nil, "")
	if r.StatusCode != 200 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	var sess *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == cookieName {
			sess = c
		}
	}
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie: %+v", sess)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.AddCookie(sess)
	st, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	st.Body.Close()
	if st.StatusCode != 200 {
		t.Errorf("state with a session: %d", st.StatusCode)
	}
	if r := post("/api/queue", `{"paused":true}`, sess, "https://evil.example"); r.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-origin write must be refused: %d", r.StatusCode)
	}
	forged := *sess
	forged.Value = "9999999999.deadbeef"
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.AddCookie(&forged)
	st, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	st.Body.Close()
	if st.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged cookie: %d", st.StatusCode)
	}
	// Lockout after repeated failures, even with the right password afterwards.
	for range 5 {
		post("/api/login", `{"password":"nope"}`, nil, "")
	}
	if r := post("/api/login", `{"password":"correct horse"}`, nil, ""); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("should be locked out: %d", r.StatusCode)
	}
}

func TestSettingsNeverReturnSecrets(t *testing.T) {
	a := testApp(t)
	a.setSetting("webhook_url", "https://ntfy.sh/topic")
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	exp := now() + 100
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/settings", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: a.signSession(exp)})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, leak := range []string{"password_hash", "session_key"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("%s leaked: %s", leak, body)
		}
	}
}

func newTestReader(s string) *bufio.Reader { return bufio.NewReaderSize(strings.NewReader(s), 1<<20) }
