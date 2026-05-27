# Agent — AI Agent for GitHub & Gitea

Agent is a Go service that automatically solves issues on **GitHub** and **Gitea**
forges by spawning isolated Docker containers running the
[pi coding agent](https://github.com/earendil-works/pi) with a local LLM.

**Trigger:** mention the configured bot user (e.g. `@agent-bot`) in an issue or
PR comment, or assign the issue to the bot user — Agent picks it up and starts
working.

**Auto-triage (optional):** an LLM-based classifier analyzes new issues on
creation — simple ones are auto-assigned to the bot, complex ones get a
`needs-human-assistance` label.

## How it works

```
Issue opened                        @agent mention or assignment
    │                                         │
    ▼                                         │
Classifier enabled? (optional)                │
 ├─ Simple → auto-assign to bot               │
 │   └─────────────────────────────────────┐  │
 └─ Complex → triage label (no action)     │  │
                                           ▼  ▼
                          GitHub / Gitea webhook
                                    │
                                    ▼
                          agent serve (Go, :9423)
                          ├── HMAC verification
                          ├── Platform detection (X-Gitea-Event / X-GitHub-Event)
                          ├── FIFO queue (in-memory, max N concurrent)
                          ├── Worker manager
                          │       ├── docker run agent-worker:latest
                          │       │     ├── gh / tea auth, git clone
                          │       │     ├── devcontainer setup (mise)
                          │       │     ├── pi --print @/tmp/prompt.md
                          │       │     ├── git commit / push / PR
                          │       │     └── cleanup
                          │       └── container lifecycle (monitor, stream logs)
                          ├── Preview manager
                          │     ├── REST API (create/stop/start/restart/destroy)
                          │     └── auto-cleanup on PR close
                          ├── Reverse proxy: /preview/{platform}/{owner}/{repo}/{pr}/
                          ├── Subdomain routing: pr{N}-{repo}.{domain}
                          └── /media/ static file server
```

The handler (`handleWebhook`) checks both assignment (`IsAssignedTo`) and
@mentions (`IsMentioned`). Either triggers a job. The worker container runs
`worker-entrypoint.sh` which handles auth, devcontainer setup, pi invocation,
git operations, screenshot uploads, and preview builds.

## Features

- **Dual-platform**: GitHub (App + JWT) and Gitea (PAT) with automatic detection via webhook headers
- **Auto-triage**: LLM classifier → simple issues auto-assigned to bot, complex ones labeled for humans
- **Flexible trigger**: @mention in issues/comments/PR reviews, or issue assignment to the bot
- **Isolated workers**: each job in its own `--network host` Docker container, destroyed when done
- **Local LLM**: uses llamacpp or any OpenAI-compatible endpoint — no cloud API keys required
- **Devcontainer-aware**: reads `.devcontainer/devcontainer.json` for Node.js version (via mise)
- **Continuation mode**: comment `@agent` on an existing PR → pi picks up feedback and pushes to the same branch
- **Self-assignment**: mention-triggered jobs auto-assign the issue to the bot user
- **Screenshots**: agent-browser artifacts (png/jpg/gif/webp) copied to `/agent-media/` and posted as comment
- **Deduplication**: won't double-process the same issue/PR
- **Preview containers**: Nixpacks / Dockerfile / Docker Compose builds with REST API lifecycle management
- **Reverse proxy**: path-based (`/preview/{platform}/{owner}/{repo}/{pr}/`) and subdomain-based routing
- **Automatic preview cleanup**: PR close webhook destroys the preview container
- **Per-repo config**: env vars, build commands, preview type overrides per `owner/repo`

## Repository layout

```
cmd/agent/main.go              # CLI entrypoint (cobra), webhook handler, preview proxy
internal/
  classifier/classifier.go     # LLM issue classifier (simple vs complex)
  config/config.go             # YAML config, defaults, pi settings serialization
  github/client.go             # GitHub REST client (JWT → installation token, labels, assign)
  gitea/client.go              # Gitea REST client (PAT, parallel API shape)
  preview/preview.go           # Preview container lifecycle + build script
  queue/queue.go               # FIFO in-memory queue with mutex-guarded dispatch
  webhook/webhook.go           # Payload parsing, HMAC verification, platform detection
  worker/worker.go             # Docker container spawn, monitor, cleanup
Dockerfile                     # Service image: golang:1.23-alpine build → alpine:3.21 runtime
Dockerfile.worker              # Worker image: Ubuntu + pi + gh + tea + nixpacks + chromium
worker-entrypoint.sh           # Worker entrypoint: auth, clone, pi, screenshots, previews
config.example.yaml            # Documented config template (safe to commit)
config.yaml                    # Active config (gitignored, contains secrets)
Makefile                       # Targets: build, run, devcontainer, worker-image
mise.toml                      # mise tool version: go=latest
docs/setup.md                  # GitHub App registration + installation guide
docs/config.md                 # Configuration reference
```

## Requirements

- **Go 1.23+** (see `go.mod`)
- **Docker** with `/var/run/docker.sock` access
- **pi agent** (Node.js) installed in the worker image
- **llamacpp** or any OpenAI-compatible LLM endpoint
- **GitHub App credentials** and/or **Gitea instance** (see [setup](docs/setup.md))

## Quick start

```bash
# Build service binary (go 1.23, cmd/agent/main.go)
go build -o agent ./cmd/agent

# Build worker image (Ubuntu + pi + gh + tea + nixpacks + chromium)
docker build -f Dockerfile.worker -t agent-worker .

# Configure
cp config.example.yaml config.yaml
$EDITOR config.yaml   # fill in GitHub App / Gitea credentials + LLM settings

# Run (listens on :9423 by default)
./agent serve
```

## CLI commands

```
agent serve      Start the webhook server (:9423 default)
agent queue      Manage the job queue (stub — not yet implemented)
agent config     Show current configuration
```

## Configuration

Loaded from `$AGENT_CONFIG` or `./config.yaml`. Defaults are defined in
`internal/config/config.go`:

| Field | Default |
|-------|---------|
| `listen_addr` | `:9423` |
| `webhook_path` | `/webhook` |
| `max_workers` | `5` |
| `media_dir` | `./media` |
| `preview_dir` | `./preview` |
| `base_url` | `http://localhost:9423` |
| `queue.type` | `memory` |
| `pi.settings.default_provider` | `anthropic` |
| `pi.settings.default_model` | `claude-sonnet-4-20250514` |
| `pi.settings.default_thinking_level` | `medium` |
| `pi.settings.skills` | `["skills/agent-browser"]` |

### Key config sections

| Section | Purpose |
|---------|---------|
| `github` | App auth: `app_id`, `installation_id`, `private_key_file` (PEM), `webhook_secret` |
| `gitea` | Gitea auth: `base_url`, `token` (PAT), `webhook_secret`, `bot_user`, `bot_token` |
| `pi.settings` | JSON → `~/.pi/agent/settings.json` in worker (provider, model, thinking, theme, skills) |
| `pi.models` | Raw JSON → `~/.pi/agent/models.json` (provider definitions — use `\|` YAML literal) |
| `pi.auth` | Raw JSON → `~/.pi/agent/auth.json` (API keys per provider) |
| `classifier` | LLM triage: `enabled`, `provider`, `model`, `base_url`, `api_key`, `human_assistance_label`, `auto_assign` |
| `repos` | Per-repo overrides: `platform`, `env`, `nixpacks`, `preview` |
| `preview_domain` | Enables subdomain-based previews (e.g. `pr42-repo.example.com`) |

### LLM configuration

Pi config is injected into workers at startup via env vars set from config:

- `AGENT_PI_SETTINGS` → `~/.pi/agent/settings.json`
- `AGENT_PI_MODELS` → `~/.pi/agent/models.json`
- `AGENT_PI_AUTH` → `~/.pi/agent/auth.json` (if set)

Example `pi` block in `config.yaml`:
```yaml
pi:
  settings:
    default_provider: llamacpp
    default_model: qwen-coder
    default_thinking_level: high
    skills:
      - skills/agent-browser
  models: |
    {
      "providers": {
        "llamacpp": {
          "baseUrl": "http://localhost:1298/v1",
          "api": "openai-completions",
          "apiKey": "ollama",
          "models": [{ "id": "qwen-coder" }]
        }
      }
    }
  auth: |
    {
      "llamacpp": {
        "type": "api_key",
        "key": "ollama"
      }
    }
```

## Gitea support

Gitea is a first-class platform. The webhook handler detects the forge via headers
(`X-Gitea-Event` checked before `X-GitHub-Event` since Gitea sends both).
The Gitea client (`internal/gitea/client.go`) mirrors the GitHub client API but
uses PAT auth instead of JWT.

Requirements for Gitea:
- PAT with `read:repository`, `write:issue`, `write:repository` scopes
- Webhook set up in the Gitea repo settings pointing to `{agent_url}/webhook`
- Bot user account with its own PAT (for self-assignment)

```yaml
gitea:
  base_url: "https://gitea.example.com"
  token: ""             # main PAT for API access
  webhook_secret: ""    # HMAC secret for webhook verification
  bot_user: "agent-bot"
  bot_token: ""         # PAT for bot user (falls back to token if empty)
```

The worker uses `tea` CLI for Gitea operations (PR creation, comments) and
`gh` CLI for GitHub. Auth URL format differs: `x-access-token:` for GitHub,
`oauth2:` for Gitea.

## Preview API

Preview containers are built from PR branches and served at
`/preview/{platform}/{owner}/{repo}/{pr}/`. The preview manager
(`internal/preview/preview.go`) handles lifecycle via `docker` CLI.

### Build fallback chain

| Priority | Method | When |
|----------|--------|------|
| 0 | Dockerfile | `preview.type: dockerfile` in per-repo config |
| 1 | Docker Compose | `preview.type: docker-compose` in per-repo config |
| 2 | Nixpacks auto-detect | `package.json` with `start` script |
| 3 | Nginx static serve | `dist/`, `build/`, `out/`, or `public/` directory exists |
| 4 | File copy | `preview/` directory in repo root |

### Endpoints

All endpoints include the platform segment (`github` or `gitea`). The handler
parses 5 path segments: `platform`, `owner`, `repo`, `pr`, `[action]`.

| Method | Path | Handler |
|--------|------|---------|
| `GET` | `/api/previews` | `handlePreviewsList` — docker ps + static dirs |
| `GET` | `/api/preview/{platform}/{owner}/{repo}/{pr}` | `handlePreviewStatus` |
| `POST` | `/api/preview/{platform}/{owner}/{repo}/{pr}` | `handlePreviewCreate` — spawns build container |
| `DELETE` | `/api/preview/{platform}/{owner}/{repo}/{pr}` | `handlePreviewDestroy` |
| `POST` | `/api/preview/{platform}/{owner}/{repo}/{pr}/stop` | `handlePreviewStop` |
| `POST` | `/api/preview/{platform}/{owner}/{repo}/{pr}/start` | `handlePreviewStart` — waits for TCP reachability |
| `POST` | `/api/preview/{platform}/{owner}/{repo}/{pr}/restart` | `handlePreviewRestart` — stop + start, refreshes port |

### Example

```bash
# Create preview for PR #42 (GitHub)
curl -X POST http://localhost:9423/api/preview/github/owner/repo/42 \
  -H "Content-Type: application/json" \
  -d '{"repo_url": "https://github.com/owner/repo.git"}'

# Check status
curl http://localhost:9423/api/preview/github/owner/repo/42

# List all
curl http://localhost:9423/api/previews

# Destroy
curl -X DELETE http://localhost:9423/api/preview/github/owner/repo/42
```

**Create request body** (`CreateRequest`):
```json
{
  "repo_url": "https://github.com/owner/repo.git",
  "branch": "agent/issue-42",
  "github_token": "ghs_...",
  "install_cmd": "npm ci",
  "build_cmd": "npm run build",
  "start_cmd": "node server.js",
  "env": { "NODE_ENV": "production" },
  "preview_type": "dockerfile",
  "dockerfile_path": "Dockerfile.prod",
  "docker_compose_file": "docker-compose.prod.yml",
  "docker_compose_service": "web",
  "port": 3000,
  "build_args": { "NODE_ENV": "production" }
}
```

All fields optional. If `github_token` is omitted, the handler generates one
via the forge client.

### Subdomain previews

When `preview_domain` is set in config, the `previewSubdomainHandler` middleware
routes requests like `pr42-owner-repo.example.com` directly to the container
(no path prefix needed). On service start, `reconcilePreviews()` restarts any
exited `agent-preview-*` containers and rewrites port files.

### Automatic cleanup

The webhook handler checks `pull_request.closed` events and calls
`previewMgr.Destroy()` to remove the container, port file, and static directory.

### Per-repo preview config

```yaml
repos:
  "owner/repo-name":
    preview:
      type: dockerfile                              # or docker-compose, nixpacks
      dockerfile_path: Dockerfile.prod
      port: 3000
      build_args:
        NODE_ENV: production
```

## Worker container

The worker image (`Dockerfile.worker`) is based on `mcr.microsoft.com/devcontainers/base:ubuntu`
and includes:

- **gh** (GitHub CLI) — PRs, comments, issue operations
- **tea** (Gitea CLI v0.9.2) — Gitea PRs, comments
- **pi** (`@earendil-works/pi-coding-agent`) — AI coding agent
- **agent-browser** — browser automation for testing/screenshots
- **nixpacks** — containerized preview builds
- **mise** — version manager (Node.js)
- **chromium-browser** — for agent-browser
- **Docker CLI** — for preview builds from inside workers

The entrypoint (`worker-entrypoint.sh`):
1. Auth: sets up `gh` / `tea` with the bot token
2. Clone: clones the repo using authenticated URL (`x-access-token` for GitHub, `oauth2` for Gitea)
3. Devcontainer: reads `.devcontainer/devcontainer.json`, installs Node.js via mise, runs `npm install`
4. Prompt: generates `/tmp/prompt.md` with the full conversation + instructions
5. Self-assign: if not already assigned, assigns the issue to the bot user
6. Pi: runs `pi --print @/tmp/prompt.md`
7. Screenshots: finds new images, copies to `/agent-media/`, posts comment with URLs
8. Preview: builds preview container via Nixpacks, posts preview URL
9. Cleanup: deletes the "working…" comment

### Environment variables passed to workers

| Variable | Source |
|----------|--------|
| `GITHUB_TOKEN` | Installation token / PAT |
| `REPO_URL`, `ISSUE_NUMBER`, `ISSUE_TITLE`, `ISSUE_BODY` | Webhook payload |
| `LLM_API_KEY` | Host env |
| `AGENT_PI_SETTINGS`, `AGENT_PI_MODELS`, `AGENT_PI_AUTH` | Config pi section |
| `AGENT_CONTINUE`, `AGENT_FEEDBACK`, `AGENT_BRANCH` | Continuation mode |
| `AGENT_BASE_URL`, `AGENT_BOT_USER`, `AGENT_BOT_TOKEN` | Config / env |
| `AGENT_TRIGGER_TYPE` | `"assign"` or `"mention"` |
| `AGENT_PLATFORM` | `"github"` or `"gitea"` |
| `AGENT_GITEA_BOT_USER`, `AGENT_GITEA_BOT_TOKEN`, `AGENT_GITEA_BASE_URL` | Gitea config |
| `AGENT_PREVIEW_DOMAIN` | Config `preview_domain` |
| `AGENT_NIXPACKS_*`, `AGENT_PREVIEW_*` | Per-repo config overrides |
| Per-repo env vars + `PREVIEW_BUILD_ARG_*` | `repos.{name}.env` and build args |

The container runs with `--network host` and mounts:
- `/var/run/docker.sock` (for preview builds and Docker operations)
- `{media_dir}:/agent-media` (screenshots)
- `{preview_dir}:/agent-preview` (port files, static fallbacks)

## Documentation

- [Setup: creating a GitHub App](docs/setup.md)
- [Configuration reference](docs/config.md)
- [Preview containers](docs/preview.md)
