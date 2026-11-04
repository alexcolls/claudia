package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A task that hits its agent's per-run cap must not restart by itself. No budget
// period ever clears that cap, so each restart would spend it again.
func TestPerRunCapDoesNotRestart(t *testing.T) {
	a := testApp(t)
	saved := minFreeBytes
	minFreeBytes = 0 // don't depend on this machine's free disk space
	t.Cleanup(func() { minFreeBytes = saved })

	// A fake docker: containers look healthy and each CLI run replays a real budget stop.
	dir := t.TempDir()
	fixture, err := filepath.Abs("testdata/claude_budget.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(dir, "runs")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  "image inspect"*) echo sha ;;
  "inspect"*) echo "true sha" ;;
  *" claude -p "*) echo run >>%q; cat %q ;;
esac
`, runs, fixture)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ag := Agent{Name: "capped", CLI: "claude", Mode: ModeFull, AcceptsQueue: true, MaxRunUSD: 1,
		CredentialID: cred(t, a, "c", "anthropic", map[string]string{"ANTHROPIC_API_KEY": "k"})}
	if err := a.createAgent(&ag); err != nil {
		t.Fatal(err)
	}
	task := addTask(t, a, "capped")

	for range 3 { // the scheduler looks again every time a run ends
		a.schedule()
		for a.isRunning(ag.ID) {
			time.Sleep(5 * time.Millisecond)
		}
	}

	b, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "run"); n != 1 {
		t.Errorf("the CLI ran %d times, want 1", n)
	}
	var status string
	if err := a.db.QueryRow(`SELECT status FROM tasks WHERE id=?`, task.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != TaskFailed {
		t.Errorf("task is %q, want %q so the owner decides", status, TaskFailed)
	}
}
