# Configuration Reference

Otto reads `config.yaml` from the working directory (or set `OTTO_CONFIG` env var).

## Full example with defaults

```yaml
listen_addr: ":9423"
webhook_path: "/webhook"
max_workers: 5

github:
  app_id: 0
  installation_id: 0
  private_key_file: "/etc/otto/github-app.pem"
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
  -e OTTO_PI_SETTINGS=... \
  -e OTTO_PI_MODELS=... \
  otto-worker:latest
```

Key details:

- **`--network host`**: needed to reach local LLM on `localhost`
- **No `--rm`**: containers are manually removed after completion/failure
- **GitHub token**: short-lived installation token, generated via JWT

Build the worker image:

```bash
docker build -f Dockerfile.worker -t otto-worker .
```

## CLI

```bash
./otto serve              # Start the webhook server
./otto config             # Print current config (with masked secrets)
./otto queue              # List queue status
```

## Environment variables

| Variable | Description |
|----------|-------------|
| `OTTO_CONFIG` | Path to config file (default: `config.yaml`) |
