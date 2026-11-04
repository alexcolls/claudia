package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Run statuses.
const (
	RunRunning       = "running"
	RunSucceeded     = "succeeded"
	RunFailed        = "failed"
	RunTimedOut      = "timed_out"
	RunStopped       = "stopped"        // the owner stopped it
	RunBudgetStopped = "budget_stopped" // a budget cap stopped it
	RunInterrupted   = "interrupted"    // the control plane restarted mid-run
)

const (
	stopGrace  = 30 * time.Second // from SIGINT to SIGTERM, and from SIGTERM to a restart
	lingerWait = 10 * time.Second
	maxLine    = 64 << 20
)

var errBusy = errors.New("this agent is already running")

// liveRun is an agent's slot while it's busy. It's held from before a task's
// git checkout until after its push, so a chat message can't slip in between.
type liveRun struct {
	agentID int64
	mu      sync.Mutex
	stop    string // why it's being stopped, once it is
	active  bool   // a CLI process is running, so there's something to signal
	done    chan struct{}
	live    Usage
}

func (r *liveRun) stopReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stop
}

func (r *liveRun) liveCopy() Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := Usage{}
	c.add(r.live)
	return c
}

// reserve takes the agent's slot, or fails if it's busy.
func (a *App) reserve(agentID int64) (*liveRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, busy := a.runs[agentID]; busy {
		return nil, errBusy
	}
	r := &liveRun{agentID: agentID, done: make(chan struct{}), live: Usage{}}
	a.runs[agentID] = r
	return r, nil
}

func (a *App) release(r *liveRun) {
	a.mu.Lock()
	delete(a.runs, r.agentID)
	a.mu.Unlock()
	close(r.done)
	a.hub.publish(Message{AgentID: r.agentID, Data: []byte(`{"kind":"state"}`)})
	a.kick()
}

func (a *App) isRunning(agentID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.runs[agentID]
	return ok
}

// stopRun asks a run to end: SIGINT, then SIGTERM, then restarting the container.
// SIGINT comes first because Claude Code then reports a result, which SIGTERM
// doesn't.
func (a *App) stopRun(agentID int64, reason string) bool {
	a.mu.Lock()
	r := a.runs[agentID]
	a.mu.Unlock()
	if r == nil {
		return false
	}
	r.mu.Lock()
	first := r.stop == ""
	if first {
		r.stop = reason
	}
	active := r.active
	r.mu.Unlock()
	if !first || !active {
		return true // a checkout or push in progress checks the reason itself
	}
	go func() {
		name := containerName(agentID)
		for _, step := range []string{"INT", "TERM"} {
			if err := signalRun(name, step); err != nil {
				log.Printf("agent %d: SIG%s: %v", agentID, step, err)
			}
			select {
			case <-r.done:
				return
			case <-time.After(stopGrace):
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := docker(ctx, "restart", name); err != nil {
			log.Printf("agent %d: restart: %v", agentID, err)
		}
	}()
	return true
}

func (a *App) stopAll(reason string) {
	a.mu.Lock()
	ids := make([]int64, 0, len(a.runs))
	for id := range a.runs {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.stopRun(id, reason)
	}
}

// Outcome is a finished run, as the queue sees it.
type Outcome struct {
	RunID   int64
	Status  string
	OK      bool
	Error   string
	Session string
	Pause   string // non-empty when it failed in a way only the owner can fix
}

// Job is one CLI invocation.
type Job struct {
	Agent   Agent
	Prompt  string // what the CLI receives
	Display string // what the feed shows
	Cwd     string
	Session string // to resume, or empty for a new session
	TaskID  int64
}

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// execRun runs one turn and records everything about it. Holding r is the
// caller's job.
func (a *App) execRun(r *liveRun, j Job) Outcome {
	ag := j.Agent
	ad := adapters[ag.CLI]
	out := Outcome{Status: RunFailed}
	var taskID any
	if j.TaskID != 0 {
		taskID = j.TaskID
	}
	res, err := a.db.Exec(`INSERT INTO runs(agent_id,task_id,started_at) VALUES(?,?,?)`, ag.ID, taskID, now())
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.RunID, _ = res.LastInsertId()
	a.addEvent(ag.ID, out.RunID, Event{Kind: "user", Text: j.Display})

	fail := func(err error) Outcome {
		out.Error = err.Error()
		a.addEvent(ag.ID, out.RunID, Event{Kind: "error", Text: out.Error})
		a.db.Exec(`UPDATE runs SET status=?,error=?,ended_at=? WHERE id=?`, RunFailed, out.Error, now(), out.RunID)
		return out
	}

	ctx := context.Background()
	if err := a.ensureContainer(ctx, ag.ID); err != nil {
		return fail(fmt.Errorf("starting the container: %w", err))
	}
	name := containerName(ag.ID)
	bundle, err := a.bundle(ag.CredentialID)
	if err != nil {
		return fail(err)
	}
	env := map[string]string{}
	for k, v := range bundle.Env {
		env[k] = v
	}
	files, err := writeCredentialFiles(ctx, name, bundle)
	if err != nil {
		return fail(err)
	}
	for k, v := range files {
		env[k] = v
	}
	if _, err := docker(ctx, "exec", name, "mkdir", "-p", j.Cwd); err != nil {
		return fail(fmt.Errorf("creating %s: %w", j.Cwd, err))
	}

	spec := RunSpec{Agent: ag, Prompt: j.Prompt, Session: j.Session, Resume: j.Session != "", MaxUSD: ag.MaxRunUSD, MCPURL: a.cfg.MCPURL}
	if !spec.Resume && ag.CLI == "claude" {
		spec.Session = newSessionID() // never --continue: it can pick up the wrong session
	}
	argv, stdin := ad.Command(spec)

	// The pid file lets a later exec signal this run. timeout forwards signals
	// and enforces the wall-clock limit, the only real ceiling for a CLI that
	// reports nothing until a turn ends.
	script := `echo $$ >/tmp/run.pid; exec timeout -s INT -k 60 "$0" "$@"`
	flags, procEnv := execEnv(env)
	args := append([]string{"exec", "-i", "-w", j.Cwd}, flags...)
	args = append(args, name, "sh", "-c", script, itoa(ag.RunLimitSec))
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	cmd.Env = procEnv
	cmd.Stdin = strings.NewReader(stdin)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(err)
	}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	r.mu.Lock()
	r.active = true
	preStop := r.stop
	r.mu.Unlock()
	if preStop != "" { // stopped between the reservation and now
		a.stopRunRetry(r)
	}

	tail := &tailBuf{max: 8 << 10}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(tail, stderr) }()

	parser := ad.Parser(ag)
	session := "" // only once the CLI confirms it: a session that never started can't be resumed
	if spec.Resume {
		session = j.Session
	}
	var result *Result
	var retries, tools, turnsSeen int
	pause := ""
	var denials []string
	lingered := false

	prices, _ := a.prices()
	marks := Usage{}
	if spec.Resume {
		marks = a.marks(j.Session)
	}

	br := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			up := parser.Parse(line)
			if up.Session != "" && up.Session != session {
				session = up.Session
				a.saveSession(j, session) // at once, so a restart mid-run can still resume
			}
			for _, e := range up.Events {
				if e.Kind == "tool_call" {
					tools++
				}
				a.addEvent(ag.ID, out.RunID, e)
			}
			if up.Retry != "" {
				retries++
				if p := pauseClass(up.Retry); p != "" {
					pause = p
				}
			}
			if len(up.Live) > 0 {
				r.mu.Lock()
				r.live.add(up.Live)
				r.mu.Unlock()
				a.persistLive(out.RunID, r.liveCopy(), prices, ag.Subscription())
				a.enforceBudgets()
			}
			if up.Result != nil {
				result = up.Result
				turnsSeen = result.Turns
				denials = result.Denials
				if ad.LingersAfterResult() && !lingered {
					lingered = true
					time.AfterFunc(lingerWait, func() {
						select {
						case <-r.done:
						default:
							a.endLingering(r)
						}
					})
				}
			}
		}
		if err != nil {
			break
		}
	}
	wg.Wait()
	waitErr := cmd.Wait()
	exitCode := 0
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		exitCode = ee.ExitCode()
	}
	r.mu.Lock()
	r.active = false
	r.mu.Unlock()

	// Accounting.
	delta := Usage{}
	if result != nil {
		delta = advanceMarks(marks, result.Cumulative)
		if session != "" {
			if err := a.saveMarks(session, marks); err != nil {
				log.Printf("run %d: saving marks: %v", out.RunID, err)
			}
		}
	}
	settled := settle(result, delta, r.liveCopy(), ag.Subscription(), prices)

	// Status.
	status := RunFailed
	errText := ""
	reason := r.stopReason()
	switch {
	case result != nil && result.OK: // finished before any stop took effect
		status = RunSucceeded
	case reason == RunBudgetStopped:
		status, errText = RunBudgetStopped, "A budget cap was reached"
	case reason != "":
		status, errText = RunStopped, "Stopped by the owner"
	case exitCode == 124 || (exitCode == 137 && result == nil):
		status, errText = RunTimedOut, fmt.Sprintf("Hit the %d second run limit", ag.RunLimitSec)
	case result != nil:
		errText = result.Error
		if result.Reason == "budget_exhausted" {
			status, errText = RunBudgetStopped, "Hit this agent's per-run budget"
		}
	default:
		errText = lastLine(tail.String())
		if errText == "" {
			errText = fmt.Sprintf("The CLI exited with code %d and no result", exitCode)
		}
	}

	out.Status, out.Error, out.OK, out.Session = status, errText, status == RunSucceeded, session
	if status == RunFailed || status == RunTimedOut {
		out.Pause = pause
	}
	var apiMS int64
	if result != nil {
		apiMS = result.APIMS
	}
	var t Tokens
	for _, m := range settled.Usage {
		t.Input += m.Input
		t.Output += m.Output
		t.CacheRead += m.CacheRead
		t.CacheWrite += m.CacheWrite
	}
	if _, err := a.db.Exec(`UPDATE runs SET session=?,status=?,error=?,usd=?,usd_state=?,input=?,output=?,cache_read=?,cache_write=?,
		turns=?,api_ms=?,retries=?,tool_calls=?,denials=?,exit_code=?,stderr_tail=?,ended_at=? WHERE id=?`,
		session, status, errText, settled.USD, settled.State, t.Input, t.Output, t.CacheRead, t.CacheWrite,
		turnsSeen, apiMS, retries, tools, len(denials), exitCode, tail.String(), now(), out.RunID); err != nil {
		log.Printf("run %d: saving: %v", out.RunID, err)
	}
	a.saveRunModels(out.RunID, settled.Usage, prices, ag.Subscription())
	if errText != "" {
		a.addEvent(ag.ID, out.RunID, Event{Kind: "error", Text: errText})
	}
	if len(denials) > 0 {
		a.addEvent(ag.ID, out.RunID, Event{Kind: "status", Text: "Denied: " + strings.Join(denials, ", ")})
	}
	a.addEvent(ag.ID, out.RunID, Event{Kind: "status", Text: runSummary(status, settled)})
	a.enforceBudgets()
	return out
}

// saveSession records a confirmed session for the next turn. A task keeps its own
// session, separate from the agent's chat.
func (a *App) saveSession(j Job, session string) {
	var err error
	if j.TaskID != 0 {
		_, err = a.db.Exec(`UPDATE tasks SET session=? WHERE id=?`, session, j.TaskID)
	} else {
		_, err = a.db.Exec(`UPDATE agents SET session=? WHERE id=?`, session, j.Agent.ID)
	}
	if err != nil {
		log.Printf("agent %d: saving session: %v", j.Agent.ID, err)
	}
}

func runSummary(status string, s Settled) string {
	if s.State == USDSubscription || s.State == USDUnpriced {
		return fmt.Sprintf("Run %s. %d tokens, %s.", strings.ReplaceAll(status, "_", " "), s.Usage.total(), s.State)
	}
	return fmt.Sprintf("Run %s. %d tokens, about $%.4f (%s).", strings.ReplaceAll(status, "_", " "), s.Usage.total(), s.USD, s.State)
}

// stopRunRetry signals a run that was asked to stop before its process existed.
func (a *App) stopRunRetry(r *liveRun) {
	go func() {
		time.Sleep(time.Second) // the pid file is written as the process starts
		signalRun(containerName(r.agentID), "INT")
		select {
		case <-r.done:
		case <-time.After(stopGrace):
			signalRun(containerName(r.agentID), "TERM")
		}
	}()
}

// endLingering ends a CLI that has reported its result but hasn't exited.
func (a *App) endLingering(r *liveRun) {
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if active {
		signalRun(containerName(r.agentID), "INT")
	}
}

// readLine reads a full line of any size up to maxLine. bufio.Scanner would stop
// at its buffer limit, which then blocks the CLI on a full pipe. An oversized
// line is dropped, never truncated, so it can't parse as something else.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	over := false
	for {
		frag, err := br.ReadSlice('\n')
		if !over {
			line = append(line, frag...)
			if len(line) > maxLine {
				over, line = true, nil
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if over {
			return nil, err
		}
		return line, err
	}
}

// tailBuf keeps the last max bytes written to it.
type tailBuf struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// persistLive keeps the running token count in the database, so a control plane
// restart can still account for a run that never reports a result.
func (a *App) persistLive(runID int64, live Usage, prices map[string]Price, subscription bool) {
	usd := liveUSD(live, prices)
	if subscription {
		usd = 0
	}
	var t Tokens
	for _, m := range live {
		t.Input += m.Input
		t.Output += m.Output
		t.CacheRead += m.CacheRead
		t.CacheWrite += m.CacheWrite
	}
	if _, err := a.db.Exec(`UPDATE runs SET input=?,output=?,cache_read=?,cache_write=?,usd=?,usd_state=? WHERE id=?`,
		t.Input, t.Output, t.CacheRead, t.CacheWrite, usd, USDEstimated, runID); err != nil {
		log.Printf("run %d: saving live usage: %v", runID, err)
	}
	a.saveRunModels(runID, live, prices, subscription)
}

func (a *App) saveRunModels(runID int64, u Usage, prices map[string]Price, subscription bool) {
	if _, err := a.db.Exec(`DELETE FROM run_models WHERE run_id=?`, runID); err != nil {
		log.Printf("run %d: clearing models: %v", runID, err)
		return
	}
	for model, t := range u {
		usd := t.USD
		if usd == 0 || t.Basis == "unknown" {
			usd = 0
			if p, ok := prices[model]; ok {
				usd = p.cost(t)
			}
		}
		if subscription {
			usd = 0
		}
		if _, err := a.db.Exec(`INSERT INTO run_models(run_id,model,input,output,cache_read,cache_write,usd) VALUES(?,?,?,?,?,?,?)`,
			runID, model, t.Input, t.Output, t.CacheRead, t.CacheWrite, usd); err != nil {
			log.Printf("run %d: saving model %s: %v", runID, model, err)
		}
	}
}

// ---- budgets

// spentSince sums finished and in-flight spend. Subscription runs have USD 0.
func (a *App) spentSince(since int64, agentID int64) float64 {
	q, args := `SELECT COALESCE(SUM(usd),0) FROM runs WHERE started_at>=?`, []any{since}
	if agentID != 0 {
		q += ` AND agent_id=?`
		args = append(args, agentID)
	}
	var usd float64
	a.db.QueryRow(q, args...).Scan(&usd)
	return usd
}

func dayStart() int64 {
	t := time.Now()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Unix()
}

func monthStart() int64 {
	t := time.Now()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).Unix()
}

// budgetBlock says why nothing new can start, if anything stops it.
func (a *App) budgetBlock() string {
	if c := a.settingFloat("budget_daily_usd"); c > 0 {
		if s := a.spentSince(dayStart(), 0); s >= c {
			return fmt.Sprintf("Daily budget reached ($%.2f of $%.2f)", s, c)
		}
	}
	if c := a.settingFloat("budget_monthly_usd"); c > 0 {
		if s := a.spentSince(monthStart(), 0); s >= c {
			return fmt.Sprintf("Monthly budget reached ($%.2f of $%.2f)", s, c)
		}
	}
	return ""
}

func (a *App) agentBudgetBlock(ag Agent) string {
	if ag.DailyBudgetUSD > 0 {
		if s := a.spentSince(dayStart(), ag.ID); s >= ag.DailyBudgetUSD {
			return fmt.Sprintf("Agent's daily budget reached ($%.2f of $%.2f)", s, ag.DailyBudgetUSD)
		}
	}
	return ""
}

// anyUSDBudget says a USD cap is in force, which makes unpriced usage a blind spot.
func (a *App) anyUSDBudget(ag Agent) bool {
	return ag.DailyBudgetUSD > 0 || ag.MaxRunUSD > 0 || a.settingFloat("budget_daily_usd") > 0 || a.settingFloat("budget_monthly_usd") > 0
}

// priceBlock refuses an agent whose spend can't be measured while a USD budget
// is active: unpriced usage would never count against the cap.
func (a *App) priceBlock(ag Agent) string {
	if ag.Subscription() || adapters[ag.CLI].ReportsUSD(ag.Preset) || !a.anyUSDBudget(ag) {
		return ""
	}
	prices, err := a.prices()
	if err != nil {
		return "Couldn't read the price table"
	}
	if _, ok := prices[ag.Model]; !ok {
		return fmt.Sprintf("No price for model %q, so its spend can't count against your budget. Add one in Settings.", ag.Model)
	}
	return ""
}

// enforceBudgets stops whatever a cap applies to. Runs persist live usage, so
// the spend it reads includes in-flight estimates.
func (a *App) enforceBudgets() {
	if reason := a.budgetBlock(); reason != "" {
		a.stopAll(RunBudgetStopped)
		return
	}
	a.mu.Lock()
	ids := make([]int64, 0, len(a.runs))
	for id := range a.runs {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		if ag, err := a.agent(id); err == nil && a.agentBudgetBlock(ag) != "" {
			a.stopRun(id, RunBudgetStopped)
		}
	}
}
