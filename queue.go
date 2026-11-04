package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"syscall"
	"time"
)

// Task statuses. A queued task with an unfinished dependency reports "waiting",
// or "blocked" when the dependency failed. Those two are derived, never stored.
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskDone      = "done"
	TaskFailed    = "failed"
	TaskNoChanges = "no_changes" // the agent finished but committed nothing
)

const minFreeBytes = 2 << 30

type Task struct {
	ID              int64   `json:"id"`
	Title           string  `json:"title"`
	Repo            string  `json:"repo"`
	BaseBranch      string  `json:"base_branch"`
	Prompt          string  `json:"prompt"`
	Priority        int     `json:"priority"`
	CLI             string  `json:"cli"`
	Preset          string  `json:"preset"`
	GitCredentialID int64   `json:"git_credential_id"`
	Status          string  `json:"status"`
	AgentID         int64   `json:"agent_id"`
	Attempts        int     `json:"attempts"`
	MaxAttempts     int     `json:"max_attempts"`
	Session         string  `json:"session"`
	StartSHA        string  `json:"start_sha"`
	HeadSHA         string  `json:"head_sha"`
	Error           string  `json:"error"`
	CreatedAt       int64   `json:"created_at"`
	UpdatedAt       int64   `json:"updated_at"`
	Deps            []int64 `json:"deps"`
	View            string  `json:"view"` // status as the owner should read it
	Note            string  `json:"note,omitempty"`
	Branch          string  `json:"branch"`
}

const taskCols = `id,title,repo,base_branch,prompt,priority,cli,preset,COALESCE(git_credential_id,0),status,COALESCE(agent_id,0),
	attempts,max_attempts,session,start_sha,head_sha,error,created_at,updated_at`

func scanTask(s interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := s.Scan(&t.ID, &t.Title, &t.Repo, &t.BaseBranch, &t.Prompt, &t.Priority, &t.CLI, &t.Preset, &t.GitCredentialID, &t.Status,
		&t.AgentID, &t.Attempts, &t.MaxAttempts, &t.Session, &t.StartSHA, &t.HeadSHA, &t.Error, &t.CreatedAt, &t.UpdatedAt)
	t.Branch = taskBranch(t.ID)
	return t, err
}

// tasks loads every task with its dependencies and derived view.
func (a *App) tasks() ([]Task, error) {
	rows, err := a.db.Query(`SELECT ` + taskCols + ` FROM tasks ORDER BY priority DESC, id`)
	if err != nil {
		return nil, err
	}
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	deps, err := a.db.Query(`SELECT task_id,dep_id FROM task_deps`)
	if err != nil {
		return nil, err
	}
	byTask := map[int64][]int64{}
	for deps.Next() {
		var t, d int64
		if err := deps.Scan(&t, &d); err != nil {
			deps.Close()
			return nil, err
		}
		byTask[t] = append(byTask[t], d)
	}
	deps.Close()
	status := map[int64]string{}
	for _, t := range out {
		status[t.ID] = t.Status
	}
	for i := range out {
		out[i].Deps = byTask[out[i].ID]
		if out[i].Deps == nil {
			out[i].Deps = []int64{}
		}
		out[i].View = taskView(out[i].Status, out[i].Deps, status)
	}
	return out, deps.Err()
}

// taskView derives what a queued task is really doing from its dependencies.
func taskView(s string, deps []int64, status map[int64]string) string {
	if s != TaskQueued {
		return s
	}
	view := s
	for _, d := range deps {
		switch status[d] {
		case TaskDone:
		case TaskFailed, TaskNoChanges:
			return "blocked" // waits until the dependency is retried
		default:
			view = "waiting"
		}
	}
	return view
}

// createTask validates and stores a task. Dependencies must already exist, which
// is what makes cycles impossible.
func (a *App) createTask(t *Task) error {
	t.Title, t.Prompt = strings.TrimSpace(t.Title), strings.TrimSpace(t.Prompt)
	if t.Prompt == "" {
		return errors.New("a prompt is required")
	}
	if t.Title == "" {
		t.Title = firstLine(t.Prompt)
	}
	if t.BaseBranch == "" {
		t.BaseBranch = "main"
	}
	if err := validateRepo(t.Repo, t.BaseBranch); err != nil {
		return err
	}
	if t.CLI != "" {
		if _, ok := adapters[t.CLI]; !ok {
			return fmt.Errorf("unknown CLI %q", t.CLI)
		}
	}
	if t.Preset != "" {
		if _, ok := presetByKey(t.Preset); !ok {
			return fmt.Errorf("unknown provider %q", t.Preset)
		}
	}
	for _, d := range t.Deps {
		var one int
		if err := a.db.QueryRow(`SELECT 1 FROM tasks WHERE id=?`, d).Scan(&one); err != nil {
			return fmt.Errorf("dependency %d doesn't exist", d)
		}
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 2
	}
	var cred any
	if t.GitCredentialID != 0 {
		var preset string
		if err := a.db.QueryRow(`SELECT preset FROM credentials WHERE id=?`, t.GitCredentialID).Scan(&preset); err != nil || preset != "git" {
			return errors.New("the git credential must be a git token")
		}
		cred = t.GitCredentialID
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO tasks(title,repo,base_branch,prompt,priority,cli,preset,git_credential_id,max_attempts,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, t.Title, t.Repo, t.BaseBranch, t.Prompt, t.Priority, t.CLI, t.Preset, cred, t.MaxAttempts, now(), now())
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	for _, d := range t.Deps {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO task_deps(task_id,dep_id) VALUES(?,?)`, t.ID, d); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.kick()
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// readyTasks returns queued tasks whose dependencies are all done, best first.
func (a *App) readyTasks() ([]Task, error) {
	rows, err := a.db.Query(`SELECT ` + taskCols + ` FROM tasks t WHERE status='queued' AND NOT EXISTS (
		SELECT 1 FROM task_deps d JOIN tasks p ON p.id=d.dep_id WHERE d.task_id=t.id AND p.status!='done')
		ORDER BY priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// eligible says whether an agent could ever take a task, busy or not.
func eligible(ag Agent, t Task) bool {
	return ag.AcceptsQueue && ag.PauseReason == "" &&
		(t.CLI == "" || t.CLI == ag.CLI) && (t.Preset == "" || t.Preset == ag.Preset)
}

// pickAgent chooses an idle agent for a task. A task with work on an agent's
// volume waits for that agent, unless it can no longer take the task. Other
// agents then start from what was pushed.
func pickAgent(t Task, agents []Agent, busy func(int64) bool) (Agent, bool) {
	if t.AgentID != 0 {
		for _, ag := range agents {
			if ag.ID == t.AgentID && eligible(ag, t) {
				return ag, !busy(ag.ID)
			}
		}
	}
	for _, ag := range agents {
		if eligible(ag, t) && !busy(ag.ID) {
			return ag, true
		}
	}
	return Agent{}, false
}

// dispatchBlock says why nothing new can start from the queue, if anything stops it.
func (a *App) dispatchBlock() string {
	if a.setting("queue_paused") == "1" {
		return "The queue is paused"
	}
	if r := a.budgetBlock(); r != "" {
		return r
	}
	var st syscall.Statfs_t
	if syscall.Statfs(a.cfg.DataDir, &st) == nil {
		if free := st.Bavail * uint64(st.Bsize); free < minFreeBytes {
			return fmt.Sprintf("Low disk space (%.1f GB free)", float64(free)/(1<<30))
		}
	}
	return ""
}

// kick asks the scheduler to look at the queue soon.
func (a *App) kick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *App) scheduler(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-tick.C:
		}
		a.schedule()
	}
}

// schedule starts every ready task it has an agent for.
func (a *App) schedule() {
	if a.dispatchBlock() != "" {
		return
	}
	tasks, err := a.readyTasks()
	if err != nil {
		log.Printf("queue: %v", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	agents, err := a.agents()
	if err != nil {
		log.Printf("queue: %v", err)
		return
	}
	for _, t := range tasks {
		ag, ok := pickAgent(t, agents, a.isRunning)
		if !ok {
			continue
		}
		if why := a.preflight(ag); why != "" {
			a.db.Exec(`UPDATE tasks SET error=?,updated_at=? WHERE id=?`, why, now(), t.ID)
			continue
		}
		r, err := a.reserve(ag.ID)
		if err != nil {
			continue
		}
		if _, err := a.db.Exec(`UPDATE tasks SET status=?,agent_id=?,attempts=attempts+1,error='',updated_at=? WHERE id=?`, TaskRunning, ag.ID, now(), t.ID); err != nil {
			log.Printf("task %d: %v", t.ID, err)
			a.release(r)
			continue
		}
		go a.runTask(t, ag, r)
	}
}

// preflight says why an agent can't start right now, if anything stops it.
func (a *App) preflight(ag Agent) string {
	if r := a.agentBudgetBlock(ag); r != "" {
		return r
	}
	return a.priceBlock(ag)
}

// taskAfterRun decides what happens to a task after a failed or stopped run.
// Auth, billing and rate-limit failures, and budget stops, don't use up retries:
// retrying can't fix them, and the owner or the next budget period will.
func taskAfterRun(t Task, o Outcome) (status, errText string, refund bool) {
	errText = o.Error
	switch {
	case o.Pause != "", o.Status == RunBudgetStopped:
		return TaskQueued, errText, true
	case o.Status == RunStopped:
		return TaskFailed, errText, false // the owner stopped it: don't restart behind their back
	case t.Attempts >= t.MaxAttempts:
		return TaskFailed, errText, false
	}
	return TaskQueued, errText, false
}

// runTask runs one attempt at a task: checkout, run, commit, push.
func (a *App) runTask(t Task, ag Agent, r *liveRun) {
	defer a.release(r)
	t.Attempts++
	// Bounded by the run limit plus time for git.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ag.RunLimitSec)*time.Second+30*time.Minute)
	defer cancel()

	finish := func(status, errText string, refund bool) {
		q := `UPDATE tasks SET status=?,error=?,updated_at=? WHERE id=?`
		if refund {
			q = `UPDATE tasks SET status=?,error=?,updated_at=?,attempts=attempts-1 WHERE id=?`
		}
		if _, err := a.db.Exec(q, status, errText, now(), t.ID); err != nil {
			log.Printf("task %d: %v", t.ID, err)
		}
		if status == TaskFailed || status == TaskNoChanges {
			a.notify(fmt.Sprintf("Task %d %s", t.ID, strings.ReplaceAll(status, "_", " ")), t.Title+": "+errText, fmt.Sprintf("#/queue/%d", t.ID))
		}
	}
	failAttempt := func(err error) {
		s, msg, _ := taskAfterRun(t, Outcome{Error: err.Error()})
		a.addEvent(ag.ID, 0, Event{Kind: "error", Text: fmt.Sprintf("Task %d: %s", t.ID, msg)})
		finish(s, msg, false)
	}

	if err := a.ensureContainer(ctx, ag.ID); err != nil {
		failAttempt(fmt.Errorf("starting the container: %w", err))
		return
	}
	if ag.Subscription() {
		if why := a.signedInCheck(ag.ID); why != "" {
			a.pauseAgent(ag.ID, why)
			finish(TaskQueued, why, true)
			return
		}
	}
	token := ""
	if t.GitCredentialID != 0 {
		b, err := a.bundle(t.GitCredentialID)
		if err != nil {
			failAttempt(err)
			return
		}
		token = b.Env["GIT_TOKEN"]
	}
	start, merge, err := a.taskStart(t)
	if err != nil {
		failAttempt(err)
		return
	}
	dir, branch := taskDir(t.ID), taskBranch(t.ID)
	resume := t.AgentID == ag.ID && t.Session != ""

	startSHA, err := gitExec(ctx, ag.ID, token, prepareScript, t.Repo, t.BaseBranch, start, dir, branch)
	if err != nil {
		failAttempt(err)
		return
	}
	if t.StartSHA == "" {
		t.StartSHA = startSHA
		a.db.Exec(`UPDATE tasks SET start_sha=? WHERE id=?`, startSHA, t.ID)
	}
	if reason := r.stopReason(); reason != "" {
		st, msg, refund := taskAfterRun(t, Outcome{Status: reason, Error: "Stopped before the run started"})
		finish(st, msg, refund)
		return
	}

	session := ""
	if resume {
		session = t.Session
	}
	o := a.execRun(r, Job{Agent: ag, Prompt: taskPrompt(t, dir, branch, merge, resume), Display: "Task " + itoa(int(t.ID)) + ": " + t.Title, Cwd: dir, Session: session, TaskID: t.ID})
	if o.Pause != "" {
		a.pauseAgent(ag.ID, o.Pause+": "+o.Error)
	}
	if !o.OK {
		st, msg, refund := taskAfterRun(t, o)
		finish(st, msg, refund)
		return
	}

	head, err := gitExec(ctx, ag.ID, token, finishScript, dir, fmt.Sprintf("claudia: task %d: %s", t.ID, t.Title))
	if err != nil {
		failAttempt(err)
		return
	}
	if head == t.StartSHA {
		finish(TaskNoChanges, "The agent finished without committing any changes", false)
		return
	}
	if _, err := gitExec(ctx, ag.ID, token, pushScript, dir, branch); err != nil {
		failAttempt(err)
		return
	}
	if _, err := a.db.Exec(`UPDATE tasks SET head_sha=? WHERE id=?`, head, t.ID); err != nil {
		log.Printf("task %d: %v", t.ID, err)
	}
	a.addEvent(ag.ID, o.RunID, Event{Kind: "status", Text: fmt.Sprintf("Pushed %s to %s", shortSHA(head), branch)})
	finish(TaskDone, "", false)
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// taskStart finds where a task starts: the recorded commit of a single dependency
// in the same repo, or the base branch. With several dependencies the agent is
// told to merge them.
func (a *App) taskStart(t Task) (start string, merge []string, err error) {
	rows, err := a.db.Query(`SELECT p.id,p.repo,p.head_sha FROM task_deps d JOIN tasks p ON p.id=d.dep_id WHERE d.task_id=? ORDER BY p.id`, t.ID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var shas []string
	for rows.Next() {
		var id int64
		var repo, sha string
		if err := rows.Scan(&id, &repo, &sha); err != nil {
			return "", nil, err
		}
		if repo == t.Repo && sha != "" {
			shas = append(shas, sha)
			merge = append(merge, sha+" (branch "+taskBranch(id)+")")
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(shas) == 1 {
		return shas[0], nil, nil
	}
	return "", merge, nil
}

// taskPrompt wraps a task's prompt. --setting-sources keeps the repo's CLAUDE.md
// from loading on its own, so the agent is told to read it.
func taskPrompt(t Task, dir, branch string, merge []string, resume bool) string {
	var b strings.Builder
	if resume {
		fmt.Fprintf(&b, "The previous attempt didn't finish")
		if t.Error != "" {
			fmt.Fprintf(&b, " (%s)", firstLine(t.Error))
		}
		b.WriteString(". Continue the task.\n\n")
	}
	fmt.Fprintf(&b, "You're working in the git repository at %s, on branch %s.\n", dir, branch)
	b.WriteString("Read CLAUDE.md or AGENTS.md first if the repository has one, and follow it.\n")
	b.WriteString("Commit your work when you're done. Don't push: pushing is handled for you, and your environment has no git credentials.\n")
	if len(merge) > 0 {
		b.WriteString("Before you start, merge these commits into your branch, which were produced by the tasks this one depends on:\n")
		for _, m := range merge {
			b.WriteString("- " + m + "\n")
		}
	}
	b.WriteString("\nTask:\n" + t.Prompt)
	return b.String()
}

func (a *App) pauseAgent(id int64, reason string) {
	if _, err := a.db.Exec(`UPDATE agents SET pause_reason=? WHERE id=?`, reason, id); err != nil {
		log.Printf("agent %d: %v", id, err)
	}
	a.notify("Agent paused", reason, fmt.Sprintf("#/agents/%d", id))
}

// retryTask gives a failed or no-changes task another go. Tasks blocked on it
// then run in turn, once it's done.
func (a *App) retryTask(id int64) error {
	res, err := a.db.Exec(`UPDATE tasks SET status=?,attempts=0,error='',updated_at=? WHERE id=? AND status IN (?,?)`, TaskQueued, now(), id, TaskFailed, TaskNoChanges)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("only failed or no-changes tasks can be retried")
	}
	a.kick()
	return nil
}

// deleteTask refuses tasks that are running or that others depend on.
func (a *App) deleteTask(id int64) error {
	var status string
	if err := a.db.QueryRow(`SELECT status FROM tasks WHERE id=?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("no such task")
		}
		return err
	}
	if status == TaskRunning {
		return errors.New("stop the agent running this task first")
	}
	var dep int
	if a.db.QueryRow(`SELECT 1 FROM task_deps WHERE dep_id=? LIMIT 1`, id).Scan(&dep) == nil {
		return errors.New("other tasks depend on this one")
	}
	if _, err := a.db.Exec(`DELETE FROM task_deps WHERE task_id=?`, id); err != nil {
		return err
	}
	_, err := a.db.Exec(`DELETE FROM tasks WHERE id=?`, id)
	return err
}
