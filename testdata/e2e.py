#!/usr/bin/env python3
"""End-to-end check of a running stack: real containers, real CLIs, real git, fake model.

Costs nothing. Start things first:

  mkdir -p /tmp/g && git init -q --bare -b main /tmp/g/repo.git   # plus one commit on main
  go run ./testdata/fakemodel -addr 10.77.0.1:9999 -log /tmp/fake.jsonl -git /tmp/g
  CLAUDIA_PASSWORD=e2e-password CLAUDIA_PORT=18080 docker compose up -d --build
  python3 testdata/e2e.py /tmp/fake.jsonl /tmp/g

It creates and deletes its own agents and tasks. Point it at a throwaway stack only.
"""
import http.cookiejar
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("CLAUDIA_URL", "http://127.0.0.1:18080")
PASSWORD = os.environ.get("CLAUDIA_PASSWORD", "e2e-password")
FAKE = "http://10.77.0.1:9999"
FAKE_LOG = sys.argv[1]
GIT_ROOT = sys.argv[2]
REPO = FAKE + "/git/repo.git"
CONTROL = "claudia-claudia-1"
ONLY = sys.argv[3].split(",") if len(sys.argv) > 3 else None

passed, failed = 0, 0
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def check(name, ok, detail=""):
    global passed, failed
    if ok:
        passed += 1
        print("PASS ", name)
    else:
        failed += 1
        print("FAIL ", name, detail)


def api(path, method="GET", body=None, want=200):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + "/api" + path, data=data, method=method, headers={"Content-Type": "application/json"} if data else {})
    try:
        with opener.open(req, timeout=60) as r:
            code, raw = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, raw = e.code, e.read()
    out = json.loads(raw) if raw else None
    if want is not None and code != want:
        raise RuntimeError(f"{method} {path}: {code} {raw[:300]}")
    return out


def wait(what, fn, timeout=120):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        last = fn()
        if last:
            return last
        time.sleep(0.5)
    raise RuntimeError(f"timed out waiting for {what}; last={last}")


def sh(*args, input=None, check_rc=False):
    p = subprocess.run(args, input=input, capture_output=True, text=True, timeout=120)
    if check_rc and p.returncode != 0:
        raise RuntimeError(f"{args}: {p.stderr}")
    return p


def container(ag):
    return f"claudia-agent-{ag['id']}"


def run_in(ag, *cmd):
    return sh("docker", "exec", container(ag), *cmd)


def agent_state(ag):
    return api(f"/agents/{ag['id']}")


def idle(ag, timeout=180):
    wait(f"{ag['name']} to finish", lambda: not agent_state(ag)["running"], timeout)


def running(ag, timeout=60):
    wait(f"{ag['name']} to start", lambda: agent_state(ag)["running"], timeout)


def runs(name):
    m = api("/metrics")
    return [r for r in m["recent"] if r["agent"] == name]


def last_run(name):
    return runs(name)[0]


def say(ag, text, want=200):
    return api(f"/agents/{ag['id']}/messages", "POST", {"text": text}, want=want)


def feed(ag, after=0, seconds=2):
    """Read the SSE backlog. Returns (events, last id)."""
    req = urllib.request.Request(BASE + f"/api/events?agent={ag['id']}", headers={"Last-Event-ID": str(after)})
    out, last = [], after
    try:
        with opener.open(req, timeout=seconds) as r:
            while True:
                line = r.readline().decode()
                if line.startswith("id: "):
                    last = int(line[4:])
                elif line.startswith("data: "):
                    m = json.loads(line[6:])
                    if m["data"].get("kind") != "state":
                        out.append(m["data"])
                elif line.startswith(": ready"):
                    break
    except Exception:
        pass
    return out, last


def fake_log():
    with open(FAKE_LOG) as f:
        return [json.loads(x) for x in f if x.strip()]


def section(name):
    print(f"\n== {name}")
    return ONLY is None or name.split()[0] in ONLY


created_agents = []


def new_agent(name, cli, cred, mode="full", model="", **extra):
    ag = api("/agents", "POST", {"name": name, "cli": cli, "credential_id": cred, "mode": mode, "model": model, "run_limit_sec": 300, "accepts_queue": False, **extra})
    created_agents.append(ag)
    return ag


def codex_agent(name, cred, model):
    """Codex ignores OPENAI_BASE_URL, so the fake has to go in its config, before the
    first run. Without this, requests go to the real API."""
    ag = new_agent(name, "codex", cred, model=model)
    config = f'''model_provider = "fake"
[model_providers.fake]
name = "fake"
base_url = "{FAKE}/v1"
env_key = "CODEX_API_KEY"
wire_api = "responses"
supports_websockets = false
'''
    sh("docker", "run", "--rm", "-i", "--user", "agent", "--mount", f"type=volume,src={container(ag)},dst=/home/agent",
       "--entrypoint", "sh", "claudia-agent:latest", "-c", "mkdir -p ~/.codex && cat > ~/.codex/config.toml", input=config, check_rc=True)
    return ag


def cleanup():
    api("/stop-all", "POST", want=None)
    api("/settings", "PUT", {"budget_daily_usd": "", "budget_monthly_usd": "", "webhook_url": ""})
    api("/queue", "POST", {"paused": False})
    for ag in created_agents:
        try:
            idle(ag, 60)
        except RuntimeError as e:
            print("cleanup:", e)
    for t in reversed(api("/tasks")["tasks"]):
        api(f"/tasks/{t['id']}", "DELETE", want=None)
    for ag in created_agents:
        api(f"/agents/{ag['id']}", "DELETE", want=None)
    for c in api("/credentials"):
        api(f"/credentials/{c['id']}", "DELETE", want=None)


def main():
    api("/login", "POST", {"password": PASSWORD})
    claude_key = "fake-claude-KEY-e2e-secret"
    c_anthropic = api("/credentials", "POST", {"name": "fake anthropic", "preset": "anthropic", "env": {"ANTHROPIC_API_KEY": claude_key, "ANTHROPIC_BASE_URL": FAKE, "DISABLE_TELEMETRY": "1"}})["id"]
    c_openai = api("/credentials", "POST", {"name": "fake openai", "preset": "openai", "env": {"CODEX_API_KEY": "fake-codex-KEY"}})["id"]
    c_gemini = api("/credentials", "POST", {"name": "fake gemini", "preset": "gemini", "env": {"GEMINI_API_KEY": "fake-gemini-KEY", "GOOGLE_GEMINI_BASE_URL": FAKE}})["id"]
    c_git = api("/credentials", "POST", {"name": "git", "preset": "git", "env": {"GIT_TOKEN": "gittoken"}})["id"]
    c_badgit = api("/credentials", "POST", {"name": "bad git", "preset": "git", "env": {"GIT_TOKEN": "nope"}})["id"]
    api("/credentials", "POST", {"name": "x", "preset": "anthropic", "env": {"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-x"}}, want=400)
    check("a Claude OAuth token can't be stored", True)

    claude = new_agent("e2e-claude", "claude", c_anthropic, model="claude-sonnet-4-5")

    if section("chat on each CLI: live output, cost and resume"):
        say(claude, "hello")
        idle(claude)
        r1 = last_run("e2e-claude")
        check("claude: run succeeded", r1["status"] == "succeeded", r1)
        check("claude: cost is the CLI's own figure", r1["usd_state"] == "reported" and r1["usd"] > 0, r1)
        evs, last = feed(claude)
        check("claude: feed has the prompt and the answer", [e for e in evs if e["kind"] == "user"] and [e for e in evs if e["kind"] == "text" and e["text"] == "ok"], evs)
        sess = agent_state(claude)["session"]
        check("claude: session recorded", bool(sess))

        say(claude, "second turn")
        idle(claude)
        r2 = last_run("e2e-claude")
        check("claude: resumed the same session", agent_state(claude)["session"] == sess)
        check("claude: a resume costs one turn, not the session's total", abs(r2["usd"] - r1["usd"]) < 1e-9, (r1["usd"], r2["usd"]))
        check("claude: tokens are per run too", r2["tokens"] == r1["tokens"], (r1["tokens"], r2["tokens"]))
        evs, _ = feed(claude, after=last)
        check("Last-Event-ID resumes without repeats", evs and evs[0]["kind"] == "user" and evs[0]["text"] == "second turn", evs[:2])

        codex = codex_agent("e2e-codex", c_openai, "gpt-6.1-sol")
        say(codex, "hello")
        idle(codex)
        r = last_run("e2e-codex")
        check("codex: run succeeded", r["status"] == "succeeded", r)
        check("codex: priced from the table", r["usd_state"] == "priced" and r["usd"] > 0, r)
        first_usd = r["usd"]
        say(codex, "second turn")
        idle(codex)
        r = last_run("e2e-codex")
        check("codex: a resume costs one turn", r["status"] == "succeeded" and abs(r["usd"] - first_usd) < 1e-9, (first_usd, r))
        unknown = codex_agent("e2e-codex-unknown", c_openai, "gpt-not-in-table")
        say(unknown, "hello")
        idle(unknown)
        r = last_run("e2e-codex-unknown")
        check("unknown model shows as unpriced, not $0", r["usd_state"] == "unpriced", r)

        agy = new_agent("e2e-agy", "agy", c_gemini, model="gemini-4-argon")
        say(agy, "hello")
        idle(agy)
        r = last_run("e2e-agy")
        check("agy: run succeeded", r["status"] == "succeeded", r)
        check("agy: priced from the table", r["usd_state"] == "priced" and r["usd"] > 0, r)
        first_usd = r["usd"]
        say(agy, "second turn")
        idle(agy)
        r = last_run("e2e-agy")
        check("agy: a resume costs one turn", r["status"] == "succeeded" and abs(r["usd"] - first_usd) < 1e-9, (first_usd, r))

    if section("permissions"):
        say(claude, "TOOL:echo full > /tmp/full")
        idle(claude)
        check("full auto: the tool ran", run_in(claude, "cat", "/tmp/full").stdout.strip() == "full")

        allow = new_agent("e2e-allow", "claude", c_anthropic, mode="allowlist", allowed_tools="Read", model="claude-sonnet-4-5")
        say(allow, "TOOL:echo no > /tmp/nope")
        idle(allow)
        check("allowlist: an unlisted tool is denied", run_in(allow, "test", "-e", "/tmp/nope").returncode != 0)
        evs, _ = feed(allow)
        check("allowlist: the denial shows in the feed", [e for e in evs if "Denied" in e.get("text", "")], evs[-3:])

    if section("modes"):
        reviewed = new_agent("e2e-reviewed", "claude", c_anthropic, mode="reviewed", model="claude-sonnet-4-5")
        say(reviewed, "hello")
        idle(reviewed)
        check("reviewed mode starts and answers", last_run("e2e-reviewed")["status"] == "succeeded", last_run("e2e-reviewed"))
        evs, _ = feed(reviewed)
        check("reviewed mode ran as auto", not [e for e in evs if e["kind"] == "error"], evs[-3:])

        allow_ok = new_agent("e2e-agy-allow", "agy", c_gemini, mode="allowlist", allowed_tools="command(echo)", model="gemini-4-argon")
        say(allow_ok, "TOOL:echo listed")
        idle(allow_ok)
        evs, _ = feed(allow_ok)
        check("agy: a listed command runs", [e for e in evs if e["kind"] == "tool_result" and "listed" in e.get("text", "")], evs[-5:])
        check("agy: tool output reaches the feed", not [e for e in evs if "Denied" in e.get("text", "")], evs[-5:])
        allow_no = new_agent("e2e-agy-deny", "agy", c_gemini, mode="allowlist", allowed_tools="command(ls)", model="gemini-4-argon")
        say(allow_no, "TOOL:echo unlisted")
        idle(allow_no)
        evs, _ = feed(allow_no)
        check("agy: an unlisted command is denied, and the feed says so", [e for e in evs if "Denied" in e.get("text", "")], evs[-5:])

    if section("approvals"):
        api("/settings", "PUT", {"webhook_url": FAKE + "/hook"})
        ask = new_agent("e2e-ask", "claude", c_anthropic, mode="ask", model="claude-sonnet-4-5")
        say(ask, "TOOL:echo approved > /tmp/approved")
        p = wait("a pending approval", lambda: next(iter(api("/approvals?pending=1")), None))
        check("approval names the tool and command", p["tool"] == "Bash" and "/tmp/approved" in p["input"], p)
        check("not run before the decision", run_in(ask, "test", "-e", "/tmp/approved").returncode != 0)
        wait("the badge", lambda: api("/state")["pending_approvals"] == 1, 5)
        api(f"/approvals/{p['id']}", "POST", {"allow": True})
        idle(ask)
        check("runs after Allow", run_in(ask, "cat", "/tmp/approved").stdout.strip() == "approved")
        hooks = [x for x in fake_log() if x.get("api") == "hook"]
        check("webhook fired", hooks and "Bash" in hooks[-1]["body"], hooks[-1:])
        check("webhook doesn't carry the tool's input", hooks and "/tmp/approved" not in hooks[-1]["body"], hooks[-1:])

        say(ask, "TOOL:echo no > /tmp/refused")
        p = wait("a pending approval", lambda: next(iter(api("/approvals?pending=1")), None))
        api(f"/approvals/{p['id']}", "POST", {"allow": False, "message": "not this"})
        idle(ask)
        check("doesn't run after Deny", run_in(ask, "test", "-e", "/tmp/refused").returncode != 0)
        evs, _ = feed(ask)
        check("the denial shows in the feed", [e for e in evs if "Denied" in e.get("text", "")], evs[-3:])

        say(ask, "ASK please")
        p = wait("a question", lambda: next(iter(api("/approvals?pending=1")), None))
        q = json.loads(p["input"])["questions"]
        api(f"/approvals/{p['id']}", "POST", {"allow": True, "input": {"questions": q, "answers": {"Which one?": "B"}}})
        idle(ask)
        check("a question gets answered", last_run("e2e-ask")["status"] == "succeeded", last_run("e2e-ask"))
        asks = [x for x in fake_log() if x.get("api") == "anthropic" and "Which one?" in x.get("last", "") and "B" in x.get("last", "")]
        check("the answer reaches the model", bool(asks))

        # Waiting on the owner doesn't count against the run limit, but working does.
        short = new_agent("e2e-short", "claude", c_anthropic, mode="ask", model="claude-sonnet-4-5", run_limit_sec=15)
        say(short, "TOOL:echo late > /tmp/late")
        p = wait("a pending approval", lambda: next((x for x in api("/approvals?pending=1") if x["agent_name"] == "e2e-short"), None))
        time.sleep(30)
        check("a long wait for approval outlasts the run limit", agent_state(short)["running"])
        api(f"/approvals/{p['id']}", "POST", {"allow": True})
        idle(short)
        check("the run carries on after the wait", run_in(short, "cat", "/tmp/late").stdout.strip() == "late" and last_run("e2e-short")["status"] == "succeeded", last_run("e2e-short"))
        say(short, "SLOW")
        running(short)
        start = time.time()
        idle(short, 50)
        r = last_run("e2e-short")
        check("working past the limit still stops it", r["status"] == "timed_out" and time.time() - start < 45, (r, time.time() - start))

        # Stopping while an approval waits clears the request.
        say(ask, "TOOL:echo no > /tmp/x")
        p = wait("a pending approval", lambda: next(iter(api("/approvals?pending=1")), None))
        api(f"/agents/{ask['id']}/stop", "POST")
        idle(ask)
        check("stopping clears a waiting approval", wait("no pending", lambda: api("/approvals?pending=1") == [], 30))
        api(f"/approvals/{p['id']}", "POST", {"allow": True}, want=409)
        api("/settings", "PUT", {"webhook_url": ""})

    if section("stop"):
        say(claude, "SLOW")
        running(claude)
        wait("the stream", lambda: any(x.get("api") == "anthropic" for x in fake_log()), 30)
        time.sleep(3)
        api(f"/agents/{claude['id']}/stop", "POST")
        start = time.time()
        idle(claude, 60)
        r = last_run("e2e-claude")
        check("stop ends a stalled run quickly", time.time() - start < 40, time.time() - start)
        check("status is stopped", r["status"] == "stopped", r)
        check("an aborted response is still counted as an estimate", r["usd_state"] == "estimated" and r["tokens"] > 0, r)
        check("the CLI is really gone", run_in(claude, "pgrep", "-x", "claude").returncode != 0)
        say(claude, "still works after a stop")
        idle(claude)
        check("the conversation carries on", last_run("e2e-claude")["status"] == "succeeded", last_run("e2e-claude"))

    if section("queue"):
        worker = new_agent("e2e-worker", "claude", c_anthropic, model="claude-sonnet-4-5", accepts_queue=True)
        task = lambda **k: api("/tasks", "POST", {"repo": REPO, "git_credential_id": c_git, **k})
        status = lambda t: next(x for x in api("/tasks")["tasks"] if x["id"] == t["id"])
        done = lambda t, s="done", timeout=180: wait(f"task {t['id']} to reach {s}", lambda: status(t)["view"] == s, timeout)

        a = task(title="A", prompt="TOOL:echo a > a.txt")
        b = task(title="B", prompt="TOOL:echo b > b.txt")
        c = task(title="C", prompt="TOOL:echo c > c.txt", deps=[a["id"]])
        d = task(title="D", prompt="TOOL:echo d > d.txt", deps=[a["id"], b["id"]])
        done(a), done(b), done(c), done(d)
        ta, tb, tc, td = status(a), status(b), status(c), status(d)
        refs = sh("git", "-C", GIT_ROOT + "/repo.git", "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads").stdout
        for t in (ta, tb, tc, td):
            check(f"task {t['title']}: branch pushed at the recorded commit", f"claudia/task-{t['id']} {t['head_sha']}" in refs, refs)
        files = lambda t: sh("git", "-C", GIT_ROOT + "/repo.git", "ls-tree", "--name-only", t["head_sha"]).stdout.split()
        check("uncommitted work gets committed", "a.txt" in files(ta), files(ta))
        check("one dependency: starts from its recorded commit", tc["start_sha"] == ta["head_sha"] and "a.txt" in files(tc) and "c.txt" in files(tc), (tc["start_sha"], files(tc)))
        check("several dependencies: starts from the base branch", "a.txt" not in files(td) and "b.txt" not in files(td), files(td))
        log = fake_log()
        told = [x for x in log if x.get("api") == "anthropic" and ta["head_sha"] in x.get("last", "") and tb["head_sha"] in x.get("last", "")]
        check("several dependencies: told to merge them", bool(told))
        check("main is untouched", sh("git", "-C", GIT_ROOT + "/repo.git", "ls-tree", "--name-only", "main").stdout.split() == ["seed.txt"])
        check("the token reached git", [x for x in log if x.get("api") == "git" and x.get("event") == "authenticated push"])
        check("the token isn't in the agent's files", run_in(worker, "grep", "-r", "gittoken", "/home/agent/work").returncode != 0)
        check("the prompt asks for CLAUDE.md", [x for x in log if x.get("api") == "anthropic" and "CLAUDE.md" in x.get("last", "")])

        n = task(title="no commit", prompt="just answer")
        done(n, "no_changes")
        waiter = task(title="waits on no-commit", prompt="TOOL:echo w > w.txt", deps=[n["id"]])
        check("no_changes blocks dependents", status(waiter)["view"] == "blocked", status(waiter))

        bad = task(title="bad base", prompt="x", base_branch="no-such-branch", max_attempts=2)
        done(bad, "failed")
        check("retries, then fails", status(bad)["attempts"] == 2, status(bad))
        child = task(title="child", prompt="TOOL:echo x > x.txt", deps=[bad["id"]])
        check("a failed dependency blocks", status(child)["view"] == "blocked")
        api(f"/tasks/{bad['id']}/retry", "POST")
        done(bad, "failed")

        badpush = api("/tasks", "POST", {"repo": REPO, "git_credential_id": c_badgit, "title": "bad token", "prompt": "TOOL:echo y > y.txt", "max_attempts": 1})
        done(badpush, "failed")
        err = status(badpush)["error"]
        check("a rejected push fails the task", "git" in err, err)
        check("the token isn't in the error", "nope" not in err, err)

        api("/queue", "POST", {"paused": True})
        p = task(title="while paused", prompt="TOOL:echo p > p.txt")
        time.sleep(3)
        check("a paused queue starts nothing", status(p)["status"] == "queued", status(p))
        api("/queue", "POST", {"paused": False})
        done(p)

        none = task(title="no taker", prompt="x", cli="agy", preset="gemini")
        time.sleep(2)
        check("says when no agent can take a task", status(none).get("note") == "No eligible agent", status(none))
        api(f"/tasks/{none['id']}", "DELETE")

    if section("budgets"):
        api("/settings", "PUT", {"budget_daily_usd": "0.000001"})
        say(claude, "x", want=409)
        check("a reached budget refuses new work", True)
        check("the reason is shown", "budget" in api("/state")["dispatch_block"].lower())
        api("/settings", "PUT", {"budget_daily_usd": ""})

        spent = api("/state")["spent_today"]
        say(claude, "SLOW")
        running(claude)
        time.sleep(1)
        api("/settings", "PUT", {"budget_daily_usd": str(spent + 0.0001)})
        idle(claude, 60)
        r = last_run("e2e-claude")
        check("a running agent is interrupted at the cap", r["status"] == "budget_stopped", r)
        api("/settings", "PUT", {"budget_daily_usd": ""})

        capped = new_agent("e2e-capped", "claude", c_anthropic, model="claude-sonnet-4-5", max_run_usd=0.0001)
        say(capped, "TOOL:echo a")
        idle(capped)
        check("per-run cap stops a run", last_run("e2e-capped")["status"] == "budget_stopped", last_run("e2e-capped"))

        api("/settings", "PUT", {"budget_daily_usd": "1000"})
        priced = codex_agent("e2e-unpriced-budget", c_openai, "gpt-not-in-table")
        err = api(f"/agents/{priced['id']}/messages", "POST", {"text": "x"}, want=409)
        check("an unpriced model is refused under a budget", "price" in err["error"].lower(), err)
        api("/settings", "PUT", {"budget_daily_usd": ""})

    if section("pause"):
        bad_key = api("/credentials", "POST", {"name": "bad key", "preset": "anthropic", "env": {"ANTHROPIC_API_KEY": "bad-key", "ANTHROPIC_BASE_URL": FAKE}})["id"]
        bad = new_agent("e2e-badkey", "claude", bad_key, model="claude-sonnet-4-5")
        say(bad, "hello")
        idle(bad, 240)
        r = last_run("e2e-badkey")
        check("a rejected key fails the run", r["status"] == "failed", r)
        st = agent_state(bad)
        check("a rejected key pauses the agent", "authentication" in st["pause_reason"], (st["pause_reason"], r["error"]))
        err = say(bad, "again", want=409)
        check("a paused agent refuses work", "authentication" in err["error"], err)
        api(f"/agents/{bad['id']}/unpause", "POST")
        check("the owner can resume it", agent_state(bad)["pause_reason"] == "")

    if section("isolation"):
        say(claude, "hello")
        idle(claude)
        check("runs as non-root", run_in(claude, "id", "-u").stdout.strip() == "1001")
        check("no capabilities", "0000000000000000" in run_in(claude, "grep", "CapEff", "/proc/self/status").stdout)
        check("no docker socket", run_in(claude, "test", "-e", "/var/run/docker.sock").returncode != 0)
        other = new_agent("e2e-other", "claude", c_anthropic, model="claude-sonnet-4-5")
        say(other, "hello")
        idle(other)
        ip = sh("docker", "inspect", container(other), "--format", '{{(index .NetworkSettings.Networks "claudia-agents").IPAddress}}').stdout.strip()
        check("can't reach another agent", run_in(claude, "sh", "-c", f"curl -fsS -m 3 http://{ip}:80/").returncode != 0, ip)
        code = lambda path, *extra: run_in(claude, "curl", "-s", "-m", "5", "-o", "/dev/null", "-w", "%{http_code}", *extra, "http://10.77.0.1:8081" + path).stdout
        check("MCP refuses no token", code("/mcp", "-X", "POST", "-d", "{}") == "401", code("/mcp", "-X", "POST", "-d", "{}"))
        check("no other routes on the agent port", code("/api/state") == "404", code("/api/state"))
        check("can't reach the UI port from an agent", run_in(claude, "curl", "-fsS", "-m", "3", "http://10.77.0.1:18080/").returncode != 0)
        cp = sh("docker", "inspect", CONTROL, "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}").stdout.split()
        check("can't reach the control plane directly", all(run_in(claude, "curl", "-fsS", "-m", "3", f"http://{i}:8080/").returncode != 0 for i in cp), cp)
        inspect = sh("docker", "inspect", container(claude)).stdout
        check("keys aren't in docker inspect", claude_key not in inspect)
        sh("docker", "cp", f"{CONTROL}:/data/claudia.db", "/tmp/claudia-e2e.db", check_rc=True)
        sh("docker", "cp", f"{CONTROL}:/data/claudia.db-wal", "/tmp/claudia-e2e.db-wal")
        raw = b""
        for f in ("/tmp/claudia-e2e.db", "/tmp/claudia-e2e.db-wal"):
            if os.path.exists(f):
                raw += open(f, "rb").read()
                os.remove(f)
        check("keys aren't readable in the database", claude_key.encode() not in raw and b"gittoken" not in raw and len(raw) > 1000)
        check("password isn't stored in the clear", PASSWORD.encode() not in raw)
        logs = sh("docker", "logs", CONTROL).stderr + sh("docker", "logs", CONTROL).stdout
        check("keys aren't in the logs", claude_key not in logs and "gittoken" not in logs)
        env_ps = sh("ps", "auxww").stdout
        check("keys aren't on any command line", claude_key not in env_ps)

        # A bad repo config can't hijack the next run.
        run_in(claude, "sh", "-c", "mkdir -p ~/work/chat/.claude && printf '%s' '{\"hooks\":{\"SessionStart\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"touch /tmp/hook-ran\"}]}]}}' > ~/work/chat/.claude/settings.json")
        say(claude, "hello again")
        idle(claude)
        check("a repo's hooks don't run", run_in(claude, "test", "-e", "/tmp/hook-ran").returncode != 0)

        # Secret files go on tmpfs, never on the volume.
        c_file = api("/credentials", "POST", {"name": "with a file", "preset": "anthropic", "env": {"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": FAKE},
                                              "files": {"GOOGLE_APPLICATION_CREDENTIALS": "file-secret-e2e"}})["id"]
        withfile = new_agent("e2e-file", "claude", c_file, model="claude-sonnet-4-5")
        say(withfile, "TOOL:echo $GOOGLE_APPLICATION_CREDENTIALS > /tmp/where")
        idle(withfile)
        path = run_in(withfile, "cat", "/tmp/where").stdout.strip()
        check("a secret file's path is in the environment", path == "/run/claudia/GOOGLE_APPLICATION_CREDENTIALS", path)
        check("the secret file has its contents", run_in(withfile, "cat", path).stdout == "file-secret-e2e")
        check("the secret file is owner-only", run_in(withfile, "stat", "-c", "%a", path).stdout.strip() == "600")
        check("the secret file is on tmpfs", "tmpfs" in run_in(withfile, "df", "--output=fstype", path).stdout)
        check("the secret isn't on the volume", run_in(withfile, "grep", "-r", "file-secret-e2e", "/home/agent").returncode != 0)

        # An agent killing its own main process.
        sh("docker", "exec", container(claude), "pkill", "-x", "sleep")
        time.sleep(4)
        say(claude, "still alive?")
        idle(claude)
        check("survives killing its own main process", last_run("e2e-claude")["status"] == "succeeded", last_run("e2e-claude"))

    if section("image"):
        say(claude, "before the image changes")
        idle(claude)
        sess = agent_state(claude)["session"]
        run_in(claude, "sh", "-c", "echo kept > ~/kept-on-volume; echo gone > /tmp/gone-with-container")
        before = sh("docker", "inspect", container(claude), "--format", "{{.Id}}").stdout
        sh("docker", "build", "-t", "claudia-agent:latest", "--label", "e2e=" + str(time.time()), "-", input="FROM claudia-agent:latest\n", check_rc=True)
        say(claude, "after the image changes")
        idle(claude)
        check("a changed image recreates the container", sh("docker", "inspect", container(claude), "--format", "{{.Id}}").stdout != before)
        check("the run works on the new container", last_run("e2e-claude")["status"] == "succeeded", last_run("e2e-claude"))
        check("the volume survives", run_in(claude, "cat", "/home/agent/kept-on-volume").stdout.strip() == "kept")
        check("the session survives", agent_state(claude)["session"] == sess)
        check("the old container's files are gone", run_in(claude, "test", "-e", "/tmp/gone-with-container").returncode != 0)

    if section("restart"):
        worker = next((a for a in created_agents if a["name"] == "e2e-worker"), None) or new_agent("e2e-worker", "claude", c_anthropic, model="claude-sonnet-4-5", accepts_queue=True)
        before_events, last = feed(claude)
        say(claude, "SLOW")
        running(claude)
        time.sleep(3)
        task_ = api("/tasks", "POST", {"repo": REPO, "git_credential_id": c_git, "title": "survives a restart", "prompt": "SLOW TOOL:echo r > r.txt"})
        wait("the task to run", lambda: next(x for x in api("/tasks")["tasks"] if x["id"] == task_["id"])["status"] == "running", 60)
        time.sleep(4)
        sh("docker", "restart", CONTROL, check_rc=True)
        wait("the control plane", lambda: _up(), 60)
        api("/login", "POST", {"password": PASSWORD})
        m = api("/metrics")
        r = next(x for x in m["recent"] if x["agent"] == "e2e-claude")
        check("the run is marked interrupted", r["status"] == "interrupted", r)
        check("usage survives as an estimate", r["usd_state"] == "estimated", r)
        check("the CLI was stopped", wait("no claude", lambda: run_in(claude, "pgrep", "-x", "claude").returncode != 0, 30))
        evs, _ = feed(claude, after=last)
        check("the feed says what happened, with no gaps", [e for e in evs if e["kind"] == "user" and e["text"] == "SLOW"] and [e for e in evs if "restarted" in e.get("text", "")], evs[-3:])
        say(claude, "after the restart")
        idle(claude)
        check("the conversation carries on", last_run("e2e-claude")["status"] == "succeeded", last_run("e2e-claude"))
        t = wait("the task to finish", lambda: (lambda x: x if x["status"] in ("done", "failed", "no_changes") else None)(next(x for x in api("/tasks")["tasks"] if x["id"] == task_["id"])), 240)
        check("the task resumes and finishes", t["status"] == "done", t)
        check("a restart doesn't use up a retry", t["attempts"] == 1, t)
        files = sh("git", "-C", GIT_ROOT + "/repo.git", "ls-tree", "--name-only", t["head_sha"]).stdout.split() if t["head_sha"] else []
        check("its work was pushed", "r.txt" in files, files)

    print(f"\nRESULT pass={passed} fail={failed}")


def _up():
    try:
        with urllib.request.urlopen(BASE + "/", timeout=2) as r:
            return r.status == 200
    except Exception:
        return False


if __name__ == "__main__":
    try:
        main()
    finally:
        try:
            cleanup()
        except Exception as e:
            print("cleanup failed:", e)
    sys.exit(1 if failed else 0)
