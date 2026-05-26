# AGENT.md — Agent (agent-github)

## Project overview

Agent is a **GitHub App** service that automatically solves GitHub issues via the
[pi coding agent](https://github.com/earendil-works/pi). A user mentions the configured bot user (e.g. `@agent-bot`, defaults to `@agent`)
in an issue or PR comment, and Agent spawns an isolated Docker container that
clones the repo, runs the pi agent with a local LLM, implements the fix, and
opens a pull request — fully automated.

- **Go** service (binary `agent`) listens for GitHub webhooks, manages a FIFO
  queue, and spawns Docker sibling containers to do the actual work.
- **Worker containers** run an Ubuntu image with git, gh CLI, Node.js,
  pi agent, agent-browser, and Chromium. They execute `worker-entrypoint.sh`.

## Repository layout

```
cmd/agent/main.go              # CLI entrypoint (cobra), serve command, webhook handler
internal/
  classifier/classifier.go    # Issue classification LLM client (auto-triage)
  config/config.go            # YAML config loading, defaults
  github/client.go            # GitHub REST API client (JWT, installation tokens, comments, PRs)
  preview/preview.go          # Preview container lifecycle (create, stop, start, restart, destroy, list)
  queue/queue.go              # FIFO in-memory job queue with configurable concurrency
  webhook/webhook.go          # Webhook payload parsing, HMAC-SHA256 signature verification,
                              #   @mention regex matching
  worker/worker.go            # Docker container spawn/cleanup, job lifecycle
Dockerfile                    # Service image: builds Go binary on alpine, minimal runtime
Dockerfile.worker             # Worker image: Ubuntu base with pi agent + tools
worker-entrypoint.sh          # Entrypoint for worker containers: setup, clone, run pi, push PR
config.example.yaml           # Documented config template
config.yaml                   # Active config (gitignored, contains secrets)
PROMPT.md                     # Detailed design document / specification
docs/setup.md                 # GitHub App registration + installation guide
Makefile                      # Build targets: build, run, devcontainer, worker-image
mise.toml                     # mise tool version: go=latest
. devcontainer/               # Dev container config for VS Code
```

## Architecture

```
GitHub webhook (issue/comment)
        │
        ▼
agent serve (Go, :9423)
 ├── HMAC verification (X-Hub-Signature-256)
 ├── Issue classifier (optional LLM triage on opened issues)
 │    ├── Simple → auto-assign to bot, triggers worker
 │    └── Complex → add "needs-human-assistance" label
 ├── @mention detection (regex built from configured bot user, default @agent)
 ├── Queue (FIFO, in-memory, max N concurrent)
 ├── Worker manager
 │    │
 │    ▼
 │  docker run agent-worker:latest
 ├── Preview manager (REST API)
 │    ├── POST   /api/preview/{repo}/{pr}       — build & start preview container
 │    ├── GET    /api/preview/{repo}/{pr}       — get status
 │    ├── DELETE /api/preview/{repo}/{pr}       — destroy container
 │    ├── POST   /api/preview/{repo}/{pr}/stop   — stop
 │    ├── POST   /api/preview/{repo}/{pr}/start  — start
 │    ├── POST   /api/preview/{repo}/{pr}/restart — restart
 │    └── GET    /api/previews                  — list all
 └── Reverse proxy: /preview/{repo}/{pr}/ → container port (or static files)
 ├── gh auth (installation token)
 ├── git clone <repo>
 ├── mise / devcontainer setup (if .devcontainer/ exists)
 ├── pi --print @/tmp/prompt.md
 ├── git commit / push / gh pr create
 ├── screenshot upload
 └── cleanup + comment on issue
```

### Critical design decisions

- **Identity**: GitHub App (JWT → installation token), NOT personal access tokens.
- **Isolation**: One Docker container per job, destroyed when done.
- **Trigger**: Assignment to the configured bot user, or @mention — never acts on its own.
- **Auto-triage** (optional): LLM classifier analyzes new issues on creation — simple
  ones are auto-assigned to the bot, complex ones get a triage label.
- **Continuation**: Commenting `@agent` on an existing PR triggers
  continuation mode (checks out the PR branch, applies feedback).
- **Deduplication**: If a job ID is already active, Agent posts an
  acknowledgement comment and does not re-enqueue.
- **Devcontainer-aware**: Workers detect `.devcontainer/devcontainer.json`
  and install the repo's declared Node.js version via mise.

## Building and running

### Prerequisites

- Go 1.23+
- Docker with socket access (`/var/run/docker.sock`)
- A GitHub App registered and installed (see `docs/setup.md`)
- A running pi agent / LLM (llamacpp or any OpenAI-compatible endpoint)

### Build

```bash
go build -o agent ./cmd/agent          # Build the service binary
docker build -f Dockerfile.worker -t agent-worker .  # Build worker image
```

### Run

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml                  # Fill in GitHub App credentials + LLM settings
./agent serve                         # Start the webhook server
```

### CLI commands

```
agent serve      Start the webhook server
agent queue      Manage the job queue (stub, not yet implemented)
agent config     Show or validate configuration
```

### Dev container

```bash
make devcontainer   # Opens VS Code in the repo's dev container
```

## Configuration

Config is loaded from `$AGENT_CONFIG` or `./config.yaml`. See `config.example.yaml`
for the full schema. Key sections:

| Section | Key fields |
|---------|------------|
| github  | `app_id`, `installation_id`, `private_key_file`, `webhook_secret`, `bot_user`, `bot_token` |
| queue   | `type` (only "memory" is implemented) |
| pi      | `settings` (provider, model, thinking level, skills), `models` (raw JSON), `auth` |
| classifier | `enabled`, `provider`, `model`, `base_url`, `api_key`, `human_assistance_label`, `auto_assign` |
| repos   | per-repo `env`, `nixpacks`, `preview` (type, dockerfile_path, docker_compose_file, port, build_args) |
| Top-level | `listen_addr`, `webhook_path`, `max_workers`, `preview_dir`, `base_url` |

Pi settings and models are written to `~/.pi/agent/settings.json` and
`~/.pi/agent/models.json` inside each worker container before pi runs.

## Job flow (detailed)

1. GitHub POSTs to `/webhook` with `X-GitHub-Event: issues` | `issue_comment`
   | `pull_request_review_comment`.
2. `webhook.Parse()` reads body, verifies HMAC signature (if secret configured),
   unmarshals into `Payload`.
3. Handler checks `action` ∈ `{opened, edited, created}`, then calls
   `webhook.IsMentioned()` on issue body + comment body.
4. If `@agent` found → build a `worker.Job` struct. If the event is a comment on
   an existing PR (detected via `payload.Issue.IsPullRequest()`), set
   `Job.Continue = true`, fetch the PR head branch via `ghClient.GetPRHead()`,
   and store the comment text in `Job.Feedback`.
5. Generate an installation token via `ghClient.InstallationToken()` and attach
   it to the job.
6. Check `queue.IsActive(job.ID)` for dedup; if active, ack the comment and skip.
7. `queue.Enqueue(job)` → starts immediately if capacity available, else waits.
8. `worker.Manager.Start()` executes `docker run ... agent-worker:latest` with
   env vars: `GITHUB_TOKEN`, `REPO_URL`, `ISSUE_NUMBER`, `ISSUE_TITLE`,
   `ISSUE_BODY`, `LLM_API_KEY`, `AGENT_PI_SETTINGS`, `AGENT_PI_MODELS`,
   `AGENT_CONTINUE`, `AGENT_FEEDBACK`, `AGENT_BRANCH`.
9. Worker container runs `worker-entrypoint.sh`: auth, clone, devcontainer
   setup, pi agent invocation, git operations, screenshot handling.
10. Manager goroutine `monitor()` runs `docker wait`, collects logs, removes
    container, updates status.

### Continuation mode

When `AGENT_CONTINUE=true`:
- The worker checks out the existing PR branch (`AGENT_BRANCH`) instead of
  creating a new one.
- The prompt to pi includes the full conversation history plus the latest
  feedback comment.
- pi is instructed to commit to the existing branch; the PR updates
  automatically.
- No new PR is created.

## Worker container environment

The worker image (`Dockerfile.worker`) provides:
- `gh` (GitHub CLI) for git operations and issue/PR comments
- `pi` (pi coding agent) as the AI engine
- `agent-browser` for browser-based testing and screenshots
- `mise` for version management (Node.js)
- `chromium-browser` for agent-browser

The entrypoint (`worker-entrypoint.sh`):
1. Sets `GH_TOKEN` and verifies auth.
2. Clones the repo into `/workspace`.
3. Reads devcontainer config if present, installs Node.js version.
4. Generates a `/tmp/prompt.md` with issue details and instructions.
5. Runs `pi --print @/tmp/prompt.md`.
6. Collects screenshots (png/jpg/gif/webp from `/workspace/`), commits them
   to the branch, posts a comment listing them.
7. Posts "working…" comment at start, deletes it at end.

### Prompt structure (for pi)

The generated prompt includes:
- Full conversation: original issue + all comments
- Instructions to ask for clarification if needed (via `gh issue comment`)
- Instructions to use `agent-browser` for UI testing
- Branch/push/PR commands appropriate for the mode (new vs continuation)
- Summary comment template

## Preview containers (DevPreview)

Workers automatically build and deploy containerized previews of PR branches
using Nixpacks (with nginx and static-file fallbacks). Previews are served via
reverse proxy at `/preview/{repo}/{pr}/` and managed through a REST API.

### How previews are built

1. Worker or API triggers a one-shot build container (`agent-worker` image).
2. Build container clones the repo, checks out the PR branch.
3. If `.devcontainer/devcontainer.json` exists, Node.js version is installed via mise.
4. Nixpacks builds a Docker image and starts the container on a random port.
5. Port mapping is written to `{previewDir}/{repo}-{pr}.port`.
6. Reverse proxy reads the port file and forwards traffic.

### Build fallback chain

| Priority | Method | Requirements |
|----------|--------|--------------|
| 0 | Dockerfile / Docker Compose | If `preview.type` is set in config |
| 1 | Nixpacks (auto-detect) | `package.json` with `start` script, or overrides |
| 2 | Nginx static serve | `dist/`, `build/`, `out/`, or `public/` directory |
| 3 | File copy | `preview/` directory in repo root |

### Configuring the build method

Per-repo in `config.yaml`:

```yaml
repos:
  "owner/repo-name":
    preview:
      type: dockerfile          # "nixpacks" (default), "dockerfile", "docker-compose"
      dockerfile_path: Dockerfile.prod
      port: 3000
      build_args:
        NODE_ENV: production

  "owner/another-repo":
    preview:
      type: docker-compose
      docker_compose_file: docker-compose.prod.yml
      docker_compose_service: web
```

When `preview.type` is set to `dockerfile` or `docker-compose`, the build
script skips Nixpacks entirely and uses the specified Docker setup.

### Preview API

All endpoints are under `/api/preview/{repo}/{pr}`. The `repo` is the full
GitHub repo name (e.g. `owner/repo`).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/previews` | List all preview containers (running + static fallbacks) |
| `GET` | `/api/preview/{repo}/{pr}` | Get container status, port, image, URL |
| `POST` | `/api/preview/{repo}/{pr}` | Create/build preview (body: `CreateRequest` JSON) |
| `DELETE` | `/api/preview/{repo}/{pr}` | Destroy container + port file + static files |
| `POST` | `/api/preview/{repo}/{pr}/stop` | Stop container |
| `POST` | `/api/preview/{repo}/{pr}/start` | Start stopped container (refreshes port) |
| `POST` | `/api/preview/{repo}/{pr}/restart` | Restart container (refreshes port) |

**Create request body** (`POST /api/preview/{repo}/{pr}`):
```json
{
  "repo_url": "https://github.com/owner/repo.git",
  "branch": "agent/issue-42",
  "github_token": "ghs_...",
  "install_cmd": "npm ci",
  "build_cmd": "npm run build",
  "start_cmd": "node server.js",
  "env": { "NODE_ENV": "production" }
}
```

All fields are optional. If `repo_url` is omitted, it defaults to
`https://github.com/{repo}.git`. If `github_token` is omitted, the manager
tries to generate an installation token via the GitHub App client. Per-repo
nixpacks config from `config.yaml` (`repos.<name>.nixpacks`) is applied
automatically.

### Automatic cleanup

When a PR is closed or merged, the `pull_request.closed` webhook triggers
`preview.Manager.Destroy()` which removes the container, port file, and
any static file directories.

### Screenshots

After pi exits, the entrypoint finds image files in `/workspace/`, copies them
to `screenshots/` on the working branch, commits them, and posts a comment
listing the files.

## GitHub API client

`internal/github/client.go` provides:
- `InstallationToken(ctx)` — generate JWT, exchange for installation token (1h TTL)
- `PostComment(ctx, owner, repo, issueNum, body)` — post a comment on an issue/PR
- `CreatePR(ctx, owner, repo, head, base, title, body)` — create a pull request
- `GetPRHead(ctx, owner, repo, prNumber)` — get the head branch of a PR (for continuation)

All use `Accept: application/vnd.github+json` and authenticate via Bearer token.

### JWT flow

1. RSA private key loaded from PEM file (supports PKCS#1 and PKCS#8).
2. JWT signed with RS256, issuer = App ID, valid 10 min.
3. JWT exchanged for installation token at
   `POST /app/installations/{installation_id}/access_tokens`.
4. Installation token used as Bearer for all subsequent API calls.

## Webhook handling

`internal/webhook/webhook.go`:
- `Parse(r *http.Request, secret string)` — reads body, optionally verifies
  HMAC-SHA256 signature, unmarshals to `Payload`.
- `IsMentioned(user string, texts ...string)` — checks for `@<user>` (case-insensitive,
  word boundary) in any text.

Events accepted: `issues`, `issue_comment`, `pull_request_review_comment`.
Actions accepted: `opened`, `edited`, `created`.

## Coding conventions

- Go idiom: use `context.Context` for API calls, defer resource cleanup,
  explicit error returns.
- Worker manager uses `sync.Mutex` to protect the active jobs map.
- Queue is also mutex-protected; `tryDispatch()` is called under lock.
- Container names: `agent-<owner>-<repo>-#<number>` with sanitization
  (lowercase, non-alphanumeric chars → `-`).
- Error handling: worker-level errors (container crash, non-zero exit) are
  logged but not retried; GitHub re-delivers webhooks on failure.

## File patterns

| Pattern | Purpose |
|---------|---------|
| `config.yaml` | Active config with secrets — **gitignored, never commit** |
| `config.example.yaml` | Documented template — safe to commit |
| `*.pem` files | Private keys — **never commit** |
| `agent` binary | Built Go binary — gitignored |
| `screenshots/` | Created by workers in target repos, not in this repo |

## TODO / unfinished

- `agent queue` command is a stub (prints "not yet implemented")
- Queue type is only "memory" (config mentions Redis but not implemented)
- No TUI (mentioned in PROMPT.md but not started)
- No PR review feedback loop beyond continuation mode
- No timeout mechanism for worker containers (documented but not coded)
- Worker `Start()` passes `nil` context from the queue (minor: should plumb a
  real context with timeout)
- Preview auto-build on PR open (currently only manual API + worker builds)
- Preview image cleanup/pruning (old images accumulate)

## Important: LLM configuration

Pi configuration is injected into workers via two env vars set by the serve
command:

- `AGENT_PI_SETTINGS` — JSON, goes to `~/.pi/agent/settings.json`
- `AGENT_PI_MODELS` — raw JSON, goes to `~/.pi/agent/models.json`

These are read from `config.yaml`'s `pi:` section. The `models` field is raw
YAML literal block scalar (`|`), containing arbitrary JSON — this is
deliberate so any pi models config (nested providers) can be passed through
without the Go config needing to model every provider's schema.

In `config.yaml`, `models` is a multi-line YAML string (note the `|` pipe):
```yaml
pi:
  models: |
    {
      "providers": {
        "llamacpp": {
          "baseUrl": "http://localhost:1298/v1",
          "api": "openai-completions",
          "apiKey": "ollama",
          "models": [{ "id": "some-model" }]
        }
      }
    }
```

## Key dependencies

- `github.com/spf13/cobra` — CLI framework
- `github.com/golang-jwt/jwt/v5` — JWT signing for GitHub App auth
- `gopkg.in/yaml.v3` — YAML config parsing
- (No Docker SDK — uses `os/exec` to call `docker` CLI directly)
