package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// gitSetup is prepended to each git script. Hooks are off and the token comes
// from the environment, so it never reaches disk. This isn't a hard boundary: an
// agent can plant other git config that runs during a push. That's why the README
// asks for a fine-grained single-repo token and branch protection on main.
const gitSetup = `set -e
g() { GIT_TERMINAL_PROMPT=0 git -c core.hooksPath=/dev/null -c core.fsmonitor=false \
  -c credential.helper= -c 'credential.helper=!f() { echo username=x-access-token; echo "password=$GIT_TOKEN"; }; f' \
  -c user.name=claudia -c user.email=claudia@localhost "$@"; }
`

// prepareScript checks out a task's branch and prints the commit the branch
// started from. Args: url base start_sha dir branch.
const prepareScript = gitSetup + `
url=$1 base=$2 start=$3 dir=$4 branch=$5
[ -d "$dir/.git" ] || g clone --quiet "$url" "$dir"
cd "$dir"
g fetch --quiet origin
if g rev-parse --verify --quiet "refs/heads/$branch" >/dev/null; then
  # Retry on the same agent: keep the work tree, take pushed commits fast-forward only.
  g checkout --quiet "$branch"
  if g rev-parse --verify --quiet "refs/remotes/origin/$branch" >/dev/null; then
    g merge --quiet --ff-only "origin/$branch"
  fi
elif g rev-parse --verify --quiet "refs/remotes/origin/$branch" >/dev/null; then
  # Retry on another agent: start from what was pushed.
  g checkout --quiet -b "$branch" "origin/$branch"
else
  g checkout --quiet -b "$branch" "${start:-origin/$base}"
fi
if [ -n "$start" ]; then echo "$start"; else g rev-parse "origin/$base"; fi
`

// finishScript commits anything left over and prints HEAD. Args: dir message.
const finishScript = gitSetup + `
cd "$1"
g add -A
g diff --cached --quiet || g commit --quiet -m "$2"
g rev-parse HEAD
`

// pushScript pushes HEAD to the task's branch. Args: dir branch.
const pushScript = gitSetup + `
cd "$1"
g push --quiet origin "HEAD:refs/heads/$2"
`

var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validateRepo is a trust boundary: the URL reaches git, which would otherwise
// run a command for ext:: and friends. Only plain http(s) is allowed.
func validateRepo(repo, base string) error {
	u, err := url.Parse(repo)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return errors.New("the repository must be an http(s) URL with no credentials in it; add the token as a git credential")
	}
	if strings.ContainsAny(repo, " \t\n") {
		return errors.New("the repository URL can't contain spaces")
	}
	if !branchRe.MatchString(base) || strings.Contains(base, "..") {
		return errors.New("that base branch name isn't valid")
	}
	return nil
}

func taskBranch(id int64) string { return "claudia/task-" + strconv.FormatInt(id, 10) }
func taskDir(id int64) string    { return agentHome + "/work/task-" + strconv.FormatInt(id, 10) }

// gitExec runs a script in the agent's container. The token is only in this exec,
// not in the CLI's environment.
func gitExec(ctx context.Context, agentID int64, token, script string, args ...string) (string, error) {
	flags, procEnv := execEnv(map[string]string{"GIT_TOKEN": token})
	argv := append([]string{"exec", "-i"}, flags...)
	argv = append(argv, containerName(agentID), "sh", "-c", script, "git")
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, "docker", argv...)
	cmd.Env = procEnv
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		// The token is passed by name, but a remote's error could echo a URL.
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "[token]")
		}
		return "", fmt.Errorf("git: %s", msg)
	}
	return strings.TrimSpace(out.String()), nil
}
