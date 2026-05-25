# Setup: Creating a GitHub App

## 1. Register the GitHub App

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App**
   (on your user: <https://github.com/settings/apps/new>, or org: `https://github.com/organizations/YOURORG/settings/apps/new`)

2. Fill in:
   - **Name**: Choose a name (e.g. `Agent`)
   - **Homepage URL**: your domain or `https://github.com/YOU/agent`
   - **Webhook URL**: `https://your-domain.com/webhook`
     (must be publicly reachable; for local dev use a tunnel like ngrok)
   - **Webhook secret**: generate with `openssl rand -hex 32`, save it

3. **Permissions** (Repository):
   | Permission | Access | Why |
   |------------|--------|-----|
   | Contents | Read & write | Clone repo, create branches, push commits |
   | Issues | Read & write | Read issue body, post comments |
   | Pull requests | Read & write | Create PRs, read PR head branch for continuation |
   | Metadata | Read-only | Always required |

4. **Subscribe to events**:
   - Issues
   - Issue comment
   - Pull request (for preview cleanup on PR close)
   - Pull request review comment (optional, for future PR review support)

5. **Where can this app be installed?**: Any account

6. Click **Create GitHub App**

7. After creation:
   - Note the **App ID** (top of the page)
   - Scroll to **Private keys** → Generate a private key → download the `.pem` file
   - Keep this file safe — it's the only time you can download it

## 2. Install the App

1. Go to **Install App** (left sidebar)
2. Choose which repositories to install on:
   - **All repositories** (convenient for personal use)
   - **Only select repositories** (more secure for organizations)
3. Click **Install**
4. After installation, note the **Installation ID** from the URL:
   `https://github.com/settings/installations/12345678` ← that number

## 3. Configure the service

Edit `config.yaml`:

```yaml
listen_addr: ":9423"
webhook_path: "/webhook"
max_workers: 5

github:
  app_id: 3844152            # ← your App ID
  installation_id: 135271489  # ← your Installation ID
  private_key_file: "/path/to/your-app.private-key.pem"
  webhook_secret: "the-secret-you-generated"

queue:
  type: memory                # memory (default) or redis

pi:
  settings:
    default_provider: llamacpp
    default_model: "your-model-name"
    default_thinking_level: medium
  # models: |
  #   { "providers": { "llamacpp": { ... } } }
```

For a full reference, see [Configuration](config.md).

## 4. Webhook delivery (production)

Your webhook URL must be publicly reachable. Options:

- **Reverse proxy**: Traefik, Nginx, Caddy pointing to `localhost:9423`
- **Tunnel**: `ngrok http 9423` (for testing)
- **Cloudflare Tunnel**: `cloudflared tunnel`

Example Traefik label (Docker):

```yaml
labels:
  traefik.http.routers.agent.rule: "Host(`agent.your-domain.com`)"
  traefik.http.services.agent.loadbalancer.server.port: "9423"
```

GitHub requires HTTPS for webhooks. A reverse proxy with Let's Encrypt handles this.

## 5. Build and run

```bash
# Build the service
go build -o agent ./cmd/agent

# Build the worker image
docker build -f Dockerfile.worker -t agent-worker .

# Run
./agent serve
```

Test by commenting `@agent` (or your configured bot user) on an issue in an installed repository.
