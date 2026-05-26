# Configuration Reference

The service reads `config.yaml` from the working directory (or set `AGENT_CONFIG` env var).

## Full example with defaults

```yaml
listen_addr: ":9423"
webhook_path: "/webhook"
max_workers: 5

github:
  app_id: 0
  installation_id: 0
  private_key_file: "/etc/agent/github-app.pem"
  webhook_secret: ""

queue:
  type: memory

pi:
  settings:
    default_provider: llamacpp
    default_model: "Jackrong--Qwopus3.6-27B-v2-MTP-GGUF"
    default_thinking_level: medium
    theme: ""
    skills:
      - skills/agent-browser
  models: ""
```

## Top-level

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `:9423` | TCP address for the HTTP server |
| `webhook_path` | string | `/webhook` | Path that GitHub delivers webhooks to |
| `max_workers` | int | `5` | Max concurrent containers; queued jobs wait |

## `github`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `app_id` | int | `0` | GitHub App ID (from app settings page) |
| `installation_id` | int | `0` | Installation ID (from install URL) |
| `private_key_file` | string | — | Path to the `.pem` private key |
| `webhook_secret` | string | — | Shared secret for HMAC verification |

All four must be set for the bot to function.

## `queue`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | `memory` | Queue backend: `memory` (in-process FIFO) or `redis` |

When `type` is `redis`, a `redis` block is required:

```yaml
queue:
  type: redis
  redis:
    addr: "localhost:6379"
    password: ""
    db: 0
```

## `pi`

Controls the pi agent running inside worker containers.

### `pi.settings`

Written to `~/.pi/agent/settings.json` inside the worker.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `default_provider` | string | `llamacpp` | Provider id matching a key in models.json |
| `default_model` | string | — | Model id for the default provider |
| `default_thinking_level` | string | `medium` | Thinking budget: `low`, `medium`, `high` |
| `theme` | string | `""` | pi TUI theme name |
| `skills` | []string | `[skills/agent-browser]` | Skill directories (relative to `~/.pi/agent`) |

The `skills/agent-browser` skill is always included by default. Add more skills
as needed (e.g., `skills/jina-reader` for web scraping).

### `pi.models`

Raw JSON string written to `~/.pi/agent/models.json`. Must be valid JSON
containing a `providers` map.

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
            { "id": "Jackrong--Qwopus3.6-27B-v2-MTP-GGUF" }
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

**Example — Anthropic (cloud):**

```yaml
pi:
  models: |
    {
      "providers": {
        "anthropic": {
          "apiKey": "sk-ant-...",
          "models": [
            { "id": "claude-sonnet-4-20250514" }
          ]
        }
      }
    }
```

## Worker container

Workers are spawned via `docker run`:

```
docker run --network host \
  -e GITHUB_TOKEN=... \
  -e REPO_URL=... \
  -e ISSUE_NUMBER=... \
  -e AGENT_PI_SETTINGS=... \
  -e AGENT_PI_MODELS=... \
  agent-worker:latest
```

Key details:

- **`--network host`**: needed to reach local LLM on `localhost`
- **No `--rm`**: containers are manually removed after completion/failure
- **GitHub token**: short-lived installation token, generated via JWT

Build the worker image:

```bash
docker build -f Dockerfile.worker -t agent-worker .
```

## CLI

```bash
./agent serve              # Start the webhook server
./agent config             # Print current config (with masked secrets)
./agent queue              # List queue status
```

## `classifier`

Optional automatic issue triage. When enabled, every new `issues: opened` event
is analyzed by an LLM to decide whether the pi agent can handle it or it needs
human intervention.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable automatic triage of new issues |
| `provider` | string | falls back to `pi.settings.default_provider` | LLM provider id (e.g. `openai`, `anthropic`) |
| `model` | string | falls back to `pi.settings.default_model` | Model name for classification |
| `base_url` | string | auto-detected from provider | API base URL (falls back to known URLs for well-known providers) |
| `api_key` | string | falls back to `pi.auth` | API key for the provider |
| `human_assistance_label` | string | `needs-human-assistance` | Label applied to complex issues |
| `auto_assign` | bool | `true` | Auto-assign simple issues to the bot user |

**Known provider base URLs** (used when `base_url` is empty):
`openai`, `anthropic`, `google`, `groq`, `deepseek`, `mistral`, `xai`, `opencode`.

**Example:**

```yaml
classifier:
  enabled: true
  provider: anthropic
  model: claude-haiku-4-20250514
  human_assistance_label: "needs-human-assistance"
  auto_assign: true
```

### How it works

```
issues: opened webhook
        │
 classifier enabled? ──No──▶ (skip, normal behavior)
        │
       Yes
        │
   LLM classifies issue
        │
   ┌────┴────┐
 Simple    Complex
   │         │
   ▼         ▼
Assign to  Add label
bot user   (no assignment)
   │
   ▼
issues: assigned webhook
   → main agent picks it up
```

Classification is **best-effort**: if the LLM call fails, the webhook falls
through to normal behavior (the issue is left untouched until a human assigns
it or mentions `@agent`).

## Environment variables

| Variable | Description |
|----------|-------------|
| `AGENT_CONFIG` | Path to config file (default: `config.yaml`) |
