# Claudia

A self-hosted control plane for a farm of coding agents. It runs Claude Code, Codex and Antigravity CLI in containers on one Docker server, and you control them from a phone or laptop.

- **Chat and terminal:** talk to any agent through a live feed, or open a shell in its container.
- **Task queue:** queue work with dependencies. Idle agents pick it up, and each task ends as a pushed git branch.
- **Approvals:** agents can wait for you to tap Allow or Deny, with a webhook alert to your phone.
- **Cost and performance:** spend by day, model, agent and provider, with budgets that stop work.

One owner per deployment. Claudia is an independent project, not made or endorsed by Anthropic, OpenAI or Google.

## Quick start

You need a server with Docker and Compose.

```bash
git clone https://github.com/alexcolls/claudia && cd claudia
CLAUDIA_PASSWORD='choose-a-long-password' docker compose up -d --build
```

Claudia listens on `127.0.0.1:8080`. Keep that private and reach it through one of these:

- **Tailscale or an SSH tunnel:** nothing else to set up.
- **Your own reverse proxy:** terminate TLS there.
- **Built-in HTTPS:** point a domain at the server, open ports 80 and 443, and run:
  ```bash
  CLAUDIA_DOMAIN=farm.example.com CLAUDIA_PASSWORD='...' \
    docker compose -f compose.yaml -f compose.https.yaml up -d --build
  ```

Then, in the web UI:

1. In **Settings**, add a credential.
2. In **Agents**, create an agent and send it a message.
3. In **Settings**, set a daily budget and a webhook URL.

## Do these before running real work

These are limits of the design, so please take them seriously.

**Use dedicated API keys with a spend limit at the provider.** Agents run code and read untrusted content, and a prompt-injected agent can read any key in its container and send it out. A key stays usable until you revoke it. Claudia's budgets are estimates, so the provider's limit is your real backstop.

**Use a fine-grained git token for one repository, and turn on branch protection for `main`.** The git token is only present while Claudia runs git, never in the agent's environment. That stops casual leaks but isn't a hard boundary, because an agent can plant git config that runs during a push. Claudia never merges, but only branch protection actually enforces that.

**On a cloud VM, block the metadata address** so agents can't take the VM's instance credentials:

```bash
sudo iptables -I DOCKER-USER -s 10.77.0.0/24 -d 169.254.169.254 -j DROP
```

Make the rule persistent using your distribution's tooling (for example `iptables-persistent`).

**Protect the UI.** The control plane holds the Docker socket, so anyone who gets into the UI effectively has root on the server. Use a long password and keep the port private.

Agents have open internet access. For untrusted repositories or sensitive data, use a separate server.

## Providers and permissions

| CLI | Providers | Permission modes |
|---|---|---|
| Claude Code | Anthropic API, Amazon Bedrock, Google Cloud, Microsoft Foundry, Anthropic-format gateway | Ask first, reviewed (not on gateways), allowlist, full auto |
| Codex | OpenAI API | Full auto |
| Antigravity (experimental) | Gemini API key, Application Default Credentials | Reviewed, allowlist, full auto |

- **Non-Claude models through a gateway:** this works, but Anthropic doesn't support running Claude Code that way, so tool use and cost figures can break.
- **Codex:** full auto only. Its own sandbox keeps the network off and may not work in a container, and OpenAI's docs advise `danger-full-access` inside Docker, so the container is the boundary.
- **Claude subscription:** create an agent with no credential, then run `claude auth login` in its terminal tab. The login stays in that agent's volume, and Claudia never sees it. Anthropic's terms bar collecting Claude.ai tokens, so Claudia refuses to store them. Plan limits assume ordinary individual use, so these agents stay out of the queue by default, and their usage is shown in tokens rather than USD.
- **Antigravity:** API key or Application Default Credentials only. Google's FAQ says wrapping `agy` isn't supported on consumer plans, and it doesn't clearly settle use inside another tool with an API key, so check that this fits your use.

If you offer this to other people rather than running it for yourself, Anthropic's [terms](https://code.claude.com/docs/en/legal-and-compliance) add conditions: each user must bring their own credentials, and you can't resell their usage.

## How it behaves

- **Cost figures are estimates.** Claude Code's figure is computed on the client, and other CLIs are priced as tokens times the table in Settings. Prices go stale, so check the dates shown there. Runs flagged `estimated` were interrupted before reporting a result, and subagent spend can't be recovered for those. `unpriced` means there's no price for that model, and Claudia refuses such agents while a USD budget is active.
- **Budgets can overshoot.** A cap is checked as usage arrives. Claude Code and Antigravity report as they go, but Codex reports only at the end of a turn, so its real ceiling is the run's time limit.
- **Restarting the control plane interrupts running agents.** Runs are marked `interrupted`, those containers restart, and pending approvals expire. Tasks then resume from the CLI's saved session.
- **Repo config doesn't run.** Claude Code gets `--setting-sources user --strict-mcp-config`, so a repo's hooks, MCP servers and skills don't load. Otherwise one agent's branch could run code in the next agent's turn. CLAUDE.md doesn't load either, so task prompts tell the agent to read it.
- **Approvals:** Claude Code only, because the other CLIs have no way to ask a person when running headless. A request waits up to 23 hours, then it's denied.

## Backups and upgrades

- **Back up** the `claudia-data` volume. It holds the database and `master.key`, which decrypts your credentials. Keep the two apart if your backups leave the server.
- **Agents' files** live in volumes named `claudia-agent-<id>`. Deleting an agent deletes its volume.
- **CLI versions** are pinned in `Dockerfile.agent`. Change them there and run `docker compose up -d --build`. Agent containers are recreated on their next run, and their home volumes are kept.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `CLAUDIA_PASSWORD` | generated and logged once | Sets the login password at startup |
| `CLAUDIA_DOMAIN` | | Enables built-in HTTPS, with `compose.https.yaml` |
| `CLAUDIA_BIND`, `CLAUDIA_PORT` | `127.0.0.1`, `8080` | Where plain HTTP is published |
| `CLAUDIA_PUBLIC_URL` | | Base URL for links in alerts |
| `CLAUDIA_AGENT_MEMORY`, `CLAUDIA_AGENT_CPUS` | `4g`, `2` | Limits per agent container |

## Development

```bash
go test ./...          # parsers against recorded CLI streams, cost, queue, approvals, auth
python3 testdata/e2e.py <fake-log> <git-root>    # the whole stack, with no keys and no spend
```

`testdata/fakemodel` stands in for the model APIs and a git server, so the real CLIs can run for free. See the top of `testdata/e2e.py` for setup.

## License

MIT
