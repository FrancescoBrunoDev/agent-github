# Configuration Reference

Config is loaded from `$AGENT_CONFIG` or `./config.yaml`. Defaults are defined
in `internal/config/config.go`.

## Full example with defaults

```yaml
listen_addr: ":9423"
webhook_path: "/webhook"
max_workers: 5
media_dir: "./media"
preview_dir: "./preview"
base_url: "http://localhost:9423"
# preview_domain: "example.com"   # enables subdomain-based previews

github:
  app_id: 0
  installation_id: 0
  private_key_file: "/etc/agent/github-app.pem"
  webhook_secret: ""
  # bot_user: "agent-bot"        # machine user for assignment (not the App)
  # bot_token: "ghp_..."         # classic PAT with repo scope

gitea:
  base_url: ""
  token: ""
  webhook_secret: ""
  # bot_user: "agent-bot"        # Gitea user for self-assignment
  # bot_token: ""                # PAT (falls back to token if empty)

queue:
  type: memory

pi:
  settings:
    default_provider: llamacpp
    default_model: qwen-coder
    default_thinking_level: medium
    theme: ""
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

classifier:
  enabled: true
  provider: llamacpp
  model: qwen-coder
  base_url: "http://localhost:1298/v1"
  api_key: "ollama"
  human_assistance_label: "needs-human-assistance"
  auto_assign: true

repos: {}
```

## Top-level

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `:9423` | TCP address for the HTTP server |
| `webhook_path` | string | `/webhook` | Path that forges deliver webhooks to |
| `max_workers` | int | `5` | Max concurrent worker containers; excess jobs queue |
| `media_dir` | string | `./media` | Directory for uploaded screenshots, served at `/media/` |
| `preview_dir` | string | `./preview` | Directory for preview port files and static fallbacks |
| `base_url` | string | `http://localhost:9423` | Public base URL (used in comments and preview links) |
| `preview_domain` | string | — | Enables subdomain-based preview routing (e.g. `pr42-repo.example.com`) |

## `github`

GitHub App authentication via JWT → installation token.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `app_id` | int64 | `0` | GitHub App ID (from app settings page) |
| `installation_id` | int64 | `0` | Installation ID (from install URL) |
| `private_key_file` | string | — | Path to the `.pem` private key (PKCS#1 or PKCS#8) |
| `webhook_secret` | string | — | Shared secret for HMAC-SHA256 signature verification |
| `bot_user` | string | — | GitHub username of a **separate machine user** account |
| `bot_token` | string | — | Classic personal access token for the bot user (repo scope) |

The **bot user** is a regular GitHub account (not the App). It handles actions
the GitHub App cannot perform directly, such as **self-assigning issues**. Add
the bot user as a collaborator on target repos and create a classic PAT with
`repo` scope.

All four `app_id`, `installation_id`, `private_key_file`, `webhook_secret`
must be set for GitHub support to activate.

## `gitea`

Gitea uses personal access tokens (PATs) instead of GitHub App JWT auth.
The Gitea API (`/api/v1`) is largely GitHub-compatible.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `base_url` | string | — | Gitea instance root URL (e.g. `https://gitea.example.com`) |
| `token` | string | — | PAT with `read:repository`, `write:issue`, `write:repository` |
| `webhook_secret` | string | — | HMAC secret for `X-Gitea-Signature` verification |
| `bot_user` | string | — | Gitea username of the bot account |
| `bot_token` | string | — | PAT for the bot user (falls back to `token` if empty) |

All of `base_url`, `token`, `webhook_secret` must be set for Gitea support.

Both GitHub and Gitea can be configured simultaneously — the webhook handler
detects the platform via headers (`X-Gitea-Event` checked before
`X-GitHub-Event`).

## `queue`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | `memory` | Queue backend — only `memory` (FIFO, in-process) is implemented |

Redis is defined in the config schema but not yet implemented.

## `pi`

Controls the pi agent configuration injected into worker containers.

### `pi.settings`

Written to `~/.pi/agent/settings.json` inside the worker.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `default_provider` | string | `anthropic` | Provider id matching a key in models.json (example: `llamacpp`) |
| `default_model` | string | `claude-sonnet-4-20250514` | Model id for the default provider (example: `qwen-coder`) |
| `default_thinking_level` | string | `medium` | Thinking budget: `low`, `medium`, `high` |
| `theme` | string | `""` | pi TUI theme name |
| `skills` | []string | `["skills/agent-browser"]` | Skill directories (relative to `~/.pi/agent/`) |

The `skills/agent-browser` skill is included by default. Add more skills
as needed (e.g. `skills/jina-reader`).

### `pi.models`

Raw JSON string written to `~/.pi/agent/models.json`. Must be valid JSON
containing a `providers` map. Use the YAML `|` literal block scalar.

**Example — llamacpp (local):**

```yaml
pi:
  models: |
    {
      "providers": {
        "llamacpp": {
          "baseUrl": "http://localhost:1298/v1",
          "api": "openai-completions",
          "apiKey": "llamacpp",
          "models": [
            { "id": "qwen-coder" }
          ]
        }
      }
    }
```

**Example — ollama (local):**

```yaml
pi:
  models: |
    {
      "providers": {
        "ollama": {
          "baseUrl": "http://localhost:11434/v1",
          "api": "openai-completions",
          "apiKey": "ollama",
          "models": [
            { "id": "llama3.1:8b" }
          ]
        }
      }
    }
```

### `pi.auth`

Raw JSON string written to `~/.pi/agent/auth.json`. Maps provider names to
credentials. Falls back to this for classifier API key if not explicitly set.

```yaml
pi:
  auth: |
    {
      "llamacpp": {
        "type": "api_key",
        "key": "ollama"
      }
    }
```

## `classifier`

Optional LLM-based issue triage. When enabled, every `issues: opened` webhook
is analyzed to decide if the agent can handle it or it needs human attention.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable automatic classification of new issues |
| `provider` | string | falls back to `pi.settings.default_provider` | Provider id |
| `model` | string | falls back to `pi.settings.default_model` | Model for classification |
| `base_url` | string | auto-detected | API base URL (known: openai, anthropic, google, groq, deepseek, mistral, xai, opencode) |
| `api_key` | string | falls back to `pi.auth` | API key for the provider |
| `human_assistance_label` | string | `needs-human-assistance` | Label applied to complex issues |
| `auto_assign` | bool | `true` | Auto-assign simple issues to the bot user |

The classifier runs **before** the normal @mention/assignment check, so
classified-simple issues skip the mention requirement entirely (assignment
fires the `assigned` webhook which restarts the handler).

**Example:**

```yaml
classifier:
  enabled: true
  provider: llamacpp
  model: qwen-coder
  base_url: "http://localhost:1298/v1"
  api_key: "ollama"
  human_assistance_label: "needs-human-assistance"
  auto_assign: true
```

If the LLM call fails, the classifier logs the error and falls through to
normal behavior (the issue is left untouched).

## `repos`

Per-repository overrides keyed by `owner/repo` (exact match against the
webhook payload's `repository.full_name`).

```yaml
repos:
  "owner/repo-name":
    # Override auto-detected platform (useful for mirrors)
    platform: gitea

    # Extra env vars injected into the worker container for this repo
    env:
      API_KEY: "secret"
      DEBUG: "true"

    # Nixpacks overrides for preview builds
    nixpacks:
      install_cmd: "pip install -r requirements.txt"
      build_cmd: "npm run build"
      start_cmd: "python app.py"

    # Preview build method
    preview:
      type: dockerfile               # "nixpacks" (default), "dockerfile", "docker-compose"
      dockerfile_path: Dockerfile    # path relative to repo root
      port: 3000                     # internal container port (default: auto-detect 3000/8080/80)
      build_args:                    # --build-arg passed to docker build
        NODE_ENV: production
```

### `repos.*.preview`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | — | Build method: `nixpacks`, `dockerfile`, or `docker-compose` |
| `dockerfile_path` | string | `Dockerfile` | Path to the Dockerfile relative to repo root |
| `docker_compose_file` | string | `docker-compose.yml` | Path to the compose file |
| `docker_compose_service` | string | first with ports | Service name to expose |
| `port` | int | auto-detect | Internal container port (3000, 8080, or 80) |
| `build_args` | map | — | Build args passed to `docker build --build-arg` |

When `type` is omitted, Nixpacks auto-detection runs with the fallback chain:
Nixpacks → nginx static serve → file copy.

## Worker container

Workers are spawned via `docker run`. Environment variables are set by
`internal/worker/worker.go`. The container uses `--network host` to reach
the local LLM and mounts `/var/run/docker.sock` for preview builds.

### Environment variables injected

| Variable | Source |
|----------|--------|
| `GITHUB_TOKEN` | Installation token (GitHub JWT) or PAT (Gitea) |
| `REPO_URL`, `ISSUE_NUMBER`, `ISSUE_TITLE`, `ISSUE_BODY` | Webhook payload |
| `LLM_API_KEY` | Host env `LLM_API_KEY` |
| `AGENT_PI_SETTINGS` | `pi.settings` JSON |
| `AGENT_PI_MODELS` | `pi.models` raw JSON |
| `AGENT_PI_AUTH` | `pi.auth` raw JSON |
| `AGENT_CONTINUE` | `"true"` or `"false"` |
| `AGENT_FEEDBACK` | Comment text (continuation mode) |
| `AGENT_BRANCH` | PR head branch (continuation mode) |
| `AGENT_BASE_URL` | Top-level `base_url` |
| `AGENT_BOT_USER` | `github.bot_user` |
| `AGENT_BOT_TOKEN` | `github.bot_token` |
| `AGENT_TRIGGER_TYPE` | `"assign"` or `"mention"` |
| `AGENT_PLATFORM` | `"github"` or `"gitea"` |
| `AGENT_GITEA_BOT_USER` | `gitea.bot_user` |
| `AGENT_GITEA_BOT_TOKEN` | `gitea.bot_token` (or `gitea.token`) |
| `AGENT_GITEA_BASE_URL` | `gitea.base_url` |
| `AGENT_PREVIEW_DOMAIN` | Top-level `preview_domain` |
| `AGENT_PREVIEW_TYPE` | `repos.*.preview.type` |
| `AGENT_PREVIEW_DOCKERFILE` | `repos.*.preview.dockerfile_path` |
| `AGENT_PREVIEW_COMPOSE_FILE` | `repos.*.preview.docker_compose_file` |
| `AGENT_PREVIEW_COMPOSE_SERVICE` | `repos.*.preview.docker_compose_service` |
| `AGENT_PREVIEW_PORT` | `repos.*.preview.port` |
| `AGENT_NIXPACKS_*_CMD` | `repos.*.nixpacks` overrides |
| Per-repo `env` + `PREVIEW_BUILD_ARG_*` | `repos.*.env` and build args |

Build the worker image:

```bash
docker build -f Dockerfile.worker -t agent-worker .
```

## CLI

```bash
./agent serve       # Start the webhook server
./agent config      # Print current configuration
./agent queue       # Queue management (stub — not yet implemented)
```

## Environment variables (host)

| Variable | Description |
|----------|-------------|
| `AGENT_CONFIG` | Path to config file (default: `./config.yaml`) |
| `LLM_API_KEY` | Passed through to worker containers as `LLM_API_KEY` |
