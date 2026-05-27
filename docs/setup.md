# Setup

Agent supports **GitHub** (via GitHub App) and **Gitea** (self-hosted or
gitea.com, via personal access tokens). You can configure one or both.

## GitHub setup

### 1. Create a bot machine user (optional but recommended)

The GitHub App cannot self-assign issues. For auto-assignment and
self-assignment, create a **separate GitHub user account** (e.g. `@agent-bot`):

1. Register a new GitHub account
2. Add it as a **collaborator** on repos where Agent should work
3. Create a **classic personal access token** with `repo` scope at
   `Settings → Developer settings → Personal access tokens → Tokens (classic)`
4. Save the token — set it as `github.bot_token` in config

Without a bot user, Agent still works via @mentions but cannot auto-assign
or self-assign issues.

### 2. Register the GitHub App

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App**
   - User: <https://github.com/settings/apps/new>
   - Org: `https://github.com/organizations/YOURORG/settings/apps/new`

2. Fill in:
   - **GitHub App name**: e.g. `Agent`
   - **Homepage URL**: your domain or repo URL
   - **Webhook URL**: `https://your-domain.com/webhook`
     (must be publicly reachable; use ngrok/cloudflared for local dev)
   - **Webhook secret**: generate with `openssl rand -hex 32`, save it

3. **Repository permissions**:

   | Permission | Access | Reason |
   |------------|--------|--------|
   | Contents | Read & write | Clone repos, create branches, push commits |
   | Issues | Read & write | Read issue body, post comments, add labels |
   | Pull requests | Read & write | Create PRs, read head branch for continuation |
   | Metadata | Read-only | Always required |

4. **Subscribe to events**:
   - Issues
   - Issue comment
   - Pull request (for preview cleanup on PR close)
   - Pull request review comment

5. **Where can this app be installed?**: Any account

6. Click **Create GitHub App**

7. After creation:
   - Note the **App ID** (shown at top)
   - Scroll to **Private keys** → **Generate a private key** → download `.pem`
   - Store this file securely — it cannot be re-downloaded

### 3. Install the App

1. Go to **Install App** (left sidebar in the app settings)
2. Choose repositories: **All repositories** or **Only select repositories**
3. Click **Install**
4. Note the **Installation ID** from the URL:
   `https://github.com/settings/installations/12345678` ← that number

### 4. Webhook delivery

Your webhook URL must be publicly reachable. Options:

- **Reverse proxy**: Traefik, Nginx, Caddy → `localhost:9423`
- **Tunnel**: `ngrok http 9423` (for testing)
- **Cloudflare Tunnel**: `cloudflared tunnel`

Example Caddy config:
```
agent.example.com {
    reverse_proxy localhost:9423
}
```

GitHub requires HTTPS for webhooks; a reverse proxy with Let's Encrypt
handles this.

## Gitea setup

### 1. Create a bot user

Create a regular Gitea user account (e.g. `agent-bot`) via the Gitea web UI
or admin panel.

Generate a **personal access token** at `Settings → Applications → Generate Token`:

- **Token name**: `Agent`
- **Scopes**: `read:repository`, `write:issue`, `write:repository`

### 2. Create a webhook

In the target repository: **Settings → Webhooks → Add Webhook** (Gitea):

- **Target URL**: `https://your-domain.com/webhook`
- **HTTP Method**: POST
- **POST Content Type**: application/json
- **Secret**: generate with `openssl rand -hex 32`
- **Trigger On**: Issues, Issue Comments, Pull Requests, Pull Request Comments

### 3. Platform detection

Agent auto-detects Gitea vs GitHub via webhook headers. Gitea sends
`X-Gitea-Event` (checked first) as well as `X-GitHub-Event` for compatibility.

## Configuration

Edit `config.yaml`:

```yaml
listen_addr: ":9423"
webhook_path: "/webhook"
max_workers: 5
media_dir: "./media"
preview_dir: "./preview"
base_url: "https://agent.your-domain.com"

# GitHub App auth
github:
  app_id: 3844152
  installation_id: 135271489
  private_key_file: "/etc/agent/github-app.pem"
  webhook_secret: "your-webhook-secret"
  bot_user: "agent-bot"          # machine user login
  bot_token: "ghp_..."           # classic PAT with repo scope

# Gitea auth (optional)
gitea:
  base_url: "https://gitea.example.com"
  token: "your-gitea-pat"
  webhook_secret: "your-gitea-webhook-secret"
  bot_user: "agent-bot"
  bot_token: ""                  # falls back to token if empty

queue:
  type: memory

pi:
  settings:
    default_provider: llamacpp
    default_model: qwen-coder
    default_thinking_level: medium
  # models: |
  #   { "providers": { "llamacpp": { ... } } }

# Optional auto-triage
classifier:
  enabled: true
  provider: llamacpp
  model: qwen-coder
  base_url: "http://localhost:1298/v1"
  api_key: "ollama"
  human_assistance_label: "needs-human-assistance"
  auto_assign: true

# Optional subdomain-based previews
# preview_domain: "preview.your-domain.com"
```

For a full reference, see [Configuration](config.md). For preview containers, see [Preview](preview.md).

## Build and run

```bash
# Build the service
go build -o agent ./cmd/agent

# Build the worker image
docker build -f Dockerfile.worker -t agent-worker .

# Verify configuration
./agent config

# Run
./agent serve
```

## Test

Comment `@agent-bot` (or your configured bot user) on an issue in an installed
repository. Agent will:

1. Post a "I'm working on this…" comment
2. Clone the repo
3. Run pi agent with the issue context
4. Push changes and create a PR
5. Build and deploy a preview (if `start` script exists)
6. Delete the "working…" comment
