package main

import (
	"net/http"
	"time"
)

// rowsOf runs a query and returns each row as a map, for a read-only JSON endpoint.
func (a *App) rowsOf(q string, args ...any) ([]map[string]any, error) {
	rows, err := a.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

const perfCols = `COUNT(*) AS runs,
	SUM(r.status='succeeded') AS ok,
	SUM(r.status IN ('failed','timed_out')) AS failed,
	COALESCE(SUM(r.usd),0) AS usd,
	COALESCE(AVG(CASE WHEN r.ended_at>0 THEN r.ended_at-r.started_at END),0) AS avg_sec,
	COALESCE(SUM(r.turns),0) AS turns,
	COALESCE(SUM(r.input),0) AS input,
	COALESCE(SUM(r.output),0) AS output,
	COALESCE(SUM(r.cache_read),0) AS cache_read,
	COALESCE(SUM(r.cache_write),0) AS cache_write,
	COALESCE(SUM(r.retries),0) AS retries,
	COALESCE(SUM(r.tool_calls),0) AS tool_calls,
	COALESCE(SUM(r.denials),0) AS denials`

// handleMetrics returns cost and performance over a window, default 30 days.
func (a *App) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	since := time.Now().AddDate(0, 0, -30).Unix()
	out := map[string]any{
		"spent_today":        a.spentSince(dayStart(), 0),
		"spent_month":        a.spentSince(monthStart(), 0),
		"budget_daily_usd":   a.settingFloat("budget_daily_usd"),
		"budget_monthly_usd": a.settingFloat("budget_monthly_usd"),
	}
	queries := map[string]string{
		"by_day": `SELECT date(r.started_at,'unixepoch','localtime') AS day, COALESCE(SUM(r.usd),0) AS usd, COUNT(*) AS runs
			FROM runs r WHERE r.started_at>=? GROUP BY day ORDER BY day`,
		"by_agent": `SELECT COALESCE(g.name,'(deleted)') AS name, ` + perfCols + `
			FROM runs r LEFT JOIN agents g ON g.id=r.agent_id WHERE r.started_at>=? GROUP BY r.agent_id ORDER BY usd DESC`,
		"by_provider": `SELECT CASE WHEN g.id IS NULL THEN '(deleted)' WHEN c.preset IS NULL THEN 'subscription' ELSE c.preset END AS name, ` + perfCols + `
			FROM runs r LEFT JOIN agents g ON g.id=r.agent_id LEFT JOIN credentials c ON c.id=g.credential_id
			WHERE r.started_at>=? GROUP BY name ORDER BY usd DESC`,
		"by_model": `SELECT m.model AS name, COUNT(*) AS runs, COALESCE(SUM(m.usd),0) AS usd, SUM(m.input) AS input, SUM(m.output) AS output,
			SUM(m.cache_read) AS cache_read, SUM(m.cache_write) AS cache_write
			FROM run_models m JOIN runs r ON r.id=m.run_id WHERE r.started_at>=? GROUP BY m.model ORDER BY usd DESC`,
		"usd_states": `SELECT usd_state AS state, COUNT(*) AS runs FROM runs r WHERE r.started_at>=? AND usd_state!='' GROUP BY usd_state`,
		"tasks":      `SELECT status, COUNT(*) AS n FROM tasks WHERE created_at>=? GROUP BY status`,
		"recent": `SELECT r.id, r.status, r.usd, r.usd_state, r.turns, r.retries, r.tool_calls, r.started_at, r.ended_at,
			r.input+r.output+r.cache_read+r.cache_write AS tokens, r.error, r.task_id, COALESCE(g.name,'(deleted)') AS agent
			FROM runs r LEFT JOIN agents g ON g.id=r.agent_id WHERE r.started_at>=? ORDER BY r.id DESC LIMIT 50`,
	}
	for key, q := range queries {
		rows, err := a.rowsOf(q, since)
		if err != nil {
			a.serverError(w, err)
			return
		}
		out[key] = rows
	}
	writeJSON(w, out)
}
