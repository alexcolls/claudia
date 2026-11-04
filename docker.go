package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const agentHome = "/home/agent"

func containerName(id int64) string { return "claudia-agent-" + strconv.FormatInt(id, 10) }

// docker runs the docker CLI and returns its trimmed stdout.
func docker(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// ensureContainer starts the agent's container if needed, and recreates it if
// the image changed. The home directory lives on a volume, so sessions and
// sign-ins survive that. No secrets are set here: they would show up in docker
// inspect. They go on each exec instead.
func (a *App) ensureContainer(ctx context.Context, id int64) error {
	name := containerName(id)
	state, err := docker(ctx, "inspect", "--format", "{{.State.Running}} {{.Image}}", name)
	want, ierr := docker(ctx, "image", "inspect", "--format", "{{.Id}}", a.cfg.AgentImage)
	if ierr != nil {
		return fmt.Errorf("agent image %s: %w", a.cfg.AgentImage, ierr)
	}
	if err == nil {
		running, image, _ := strings.Cut(state, " ")
		if image == want {
			if running == "true" {
				return nil
			}
			_, err := docker(ctx, "start", name)
			return err
		}
		if _, err := docker(ctx, "rm", "-f", name); err != nil {
			return err
		}
	}
	_, err = docker(ctx, "run", "-d", "--name", name,
		"--label", "claudia.agent="+strconv.FormatInt(id, 10),
		"--init", "--restart", "unless-stopped", // an agent can kill its own main process
		"--user", "agent", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", "4096", "--memory", a.cfg.AgentMemory, "--cpus", a.cfg.AgentCPUs,
		"--network", a.cfg.AgentNetwork,
		"--mount", "type=volume,src="+name+",dst="+agentHome,
		"--tmpfs", "/run/claudia:rw,noexec,nosuid,size=1m,uid=1001,gid=1001,mode=0700",
		a.cfg.AgentImage)
	return err
}

// removeAgentContainer deletes the container and its volume.
func removeAgentContainer(ctx context.Context, id int64) error {
	name := containerName(id)
	if _, err := docker(ctx, "rm", "-f", name); err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}
	if _, err := docker(ctx, "volume", "rm", name); err != nil && !strings.Contains(err.Error(), "no such volume") {
		return err
	}
	return nil
}

// execEnv returns the -e flags and process environment for a docker exec.
// Values go through the client's environment so they never appear on a command
// line, where `ps` on the host would show them.
func execEnv(env map[string]string) (flags, procEnv []string) {
	procEnv = os.Environ()
	for k, v := range env {
		flags = append(flags, "-e", k)
		procEnv = append(procEnv, k+"="+v)
	}
	return flags, procEnv
}

// writeCredentialFiles puts a bundle's secret files on the container's tmpfs,
// and returns the env vars pointing at them.
func writeCredentialFiles(ctx context.Context, container string, b Bundle) (map[string]string, error) {
	env := map[string]string{}
	for name, content := range b.Files {
		path := "/run/claudia/" + name
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", container, "sh", "-c", `umask 077; cat > "$1"`, "--", path)
		cmd.Stdin = strings.NewReader(content)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("writing %s: %s", name, strings.TrimSpace(string(out)))
		}
		env[name] = path
	}
	return env, nil
}

// signalRun signals a run through its pid file, by way of a new exec. Killing
// the docker exec client isn't enough: the process keeps running and spending.
func signalRun(container, signal string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := docker(ctx, "exec", container, "sh", "-c", `kill -"$1" "$(cat /tmp/run.pid)"`, "--", signal)
	return err
}
