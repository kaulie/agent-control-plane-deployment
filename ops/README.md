# Independent ops daemons (deployment domain)

These processes live under `/Users/gaolei/deployment/web-cursor/ops/` and must
**not** run from `runtime/`. They survive gateway restarts.

Source of truth: this repository
(`https://github.com/kaulie/agent-control-plane-deployment`).
Install with root `./install.sh` (or `ops/install.sh` after copy).

| Daemon | Role |
|---|---|
| `watchdog.sh` | Health-check `:4211`; call runtime `scripts/start.sh` if down |
| `deploy-agent.sh` | Watch `deploy-requests/*.json` and run `bin/deploy.sh` |

### Deploy grace (2 minutes)

Before each deploy, `deploy-agent` writes `ops/watchdog-pause-until` (unix epoch =
now + `DEPLOY_MAX_SEC`, default **120**). While that pause is active the
watchdog will **not** auto-start. If the service is still down when the pause
expires, the watchdog clears the pause flags and starts runtime itself.

`deploy-agent` also kills `bin/deploy.sh` if it exceeds the same 120s budget.
On a healthy success it clears the pause early.

## Install / start

```bash
DEPLOY_HOME=/Users/gaolei/deployment/web-cursor ./install.sh
```

## Request a deploy (no self-kill)

Prefer the gateway API on the **app** process (returns immediately).
Graceful restart is configured in the app (`GRACEFUL_RESTART`, etc.) — see
app repo docs / `BRANCHING.md`.

```bash
curl -sS -X POST http://127.0.0.1:4211/api/ops/deploy \
  -H 'content-type: application/json' \
  -d '{"deployment":"deployment-<hash>"}'

curl -sS http://127.0.0.1:4211/api/ops/restart-status
```

Or drop a JSON file:

```bash
cat > /Users/gaolei/deployment/web-cursor/deploy-requests/deploy-req-demo.json <<'EOF'
{"requestId":"deploy-req-demo","deployment":"deployment-<hash>"}
EOF
```

**Agents must not** run `bin/deploy.sh` synchronously inside a task shell — that kills the gateway mid-command.
