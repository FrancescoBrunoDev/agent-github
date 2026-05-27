# Preview Containers

Agent automatically builds and deploys containerized previews of PR branches
using Nixpacks (with Dockerfile and nginx/static fallbacks). Previews are served
via reverse proxy at `/preview/{platform}/{owner}/{repo}/{pr}/` and managed
through a REST API.

## Quick start

No configuration needed. When a worker creates a PR, it automatically:

1. Runs `nixpacks build /workspace`
2. Starts a container on a random host port
3. Posts a comment with the preview URL

```bash
# Check if it's running
curl http://localhost:9423/api/preview/github/owner/repo/42

# Preview URL (path-based)
http://localhost:9423/preview/github/owner/repo/42/

# Or with subdomain routing (if preview_domain is set)
https://pr42-owner-repo.example.com
```

## Build methods

The build system tries methods in priority order:

| Priority | Method | Activation |
|----------|--------|------------|
| 0 | **Dockerfile** | `repos."owner/repo".preview.type: dockerfile` |
| 1 | **Docker Compose** | `repos."owner/repo".preview.type: docker-compose` |
| 2 | **Nixpacks** | Auto-detected from `package.json` with `start` script |
| 3 | **Nginx static** | `dist/`, `build/`, `out/`, or `public/` directory found |
| 4 | **File copy** | `preview/` directory in repo root |

### Nixpacks (default)

Nixpacks auto-detects the language and build system from the repo structure:

```json
// package.json
{
  "scripts": {
    "build": "vite build",
    "start": "npx serve dist"
  }
}
```

Per-repo overrides for Nixpacks:

```yaml
repos:
  "owner/repo-name":
    nixpacks:
      install_cmd: "pip install -r requirements.txt"
      build_cmd: "npm run build"
      start_cmd: "python app.py"
```

### Dockerfile

Use a custom Dockerfile instead of Nixpacks:

```yaml
repos:
  "owner/repo-name":
    preview:
      type: dockerfile
      dockerfile_path: Dockerfile.prod
      port: 3000
      build_args:
        NODE_ENV: production
```

The build script runs:
```bash
docker build -t preview-owner-repo-42 \
  -f /workspace/Dockerfile.prod \
  --build-arg NODE_ENV=production \
  /workspace

docker run -d --name agent-preview-owner-repo-42 \
  -p 0:3000 preview-owner-repo-42
```

### Docker Compose

Use a compose file for multi-service previews:

```yaml
repos:
  "owner/repo-name":
    preview:
      type: docker-compose
      docker_compose_file: docker-compose.prod.yml
      docker_compose_service: web
```

The build script runs `docker compose build` then `docker compose up -d`.
If `docker_compose_service` is omitted, the first service with exposed ports
is used.

### Static nginx fallback

If Nixpacks can't find a start command, it checks for build output directories:

```
dist/  →  build/  →  out/  →  public/
```

If one is found with content, an nginx:alpine container is built serving that
directory.

### File copy fallback

If all other methods fail, files from `/workspace/preview/` are copied to the
preview static directory. No container is created; files are served directly
by the reverse proxy.

## REST API

All endpoints live under `/api/preview/{platform}/{owner}/{repo}/{pr}`.
The `platform` segment is `github` or `gitea`.

### List all previews

```bash
GET /api/previews
```

Returns running containers (via `docker ps`) and static fallback directories.

```json
[
  {
    "name": "agent-preview-owner-repo-42",
    "repo": "owner-repo",
    "pr": 42,
    "status": "running",
    "port": 32768,
    "image": "preview-owner-repo-42",
    "preview_url": "http://localhost:9423/preview/github/owner/repo/42/"
  }
]
```

### Get status

```bash
GET /api/preview/{platform}/{owner}/{repo}/{pr}
```

```json
{
  "name": "agent-preview-owner-repo-42",
  "repo": "owner/repo",
  "pr": 42,
  "status": "running",
  "port": 32768,
  "preview_url": "http://localhost:9423/preview/github/owner/repo/42/"
}
```

Status values: `running`, `stopped`, `created`, `static` (no container, static files).

### Create / build

```bash
POST /api/preview/{platform}/{owner}/{repo}/{pr}
Content-Type: application/json

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
  "port": 3000
}
```

All fields are optional:
- `repo_url` — defaults to `https://github.com/{owner}/{repo}.git` (or Gitea URL)
- `github_token` — auto-generated from the forge client if omitted
- `branch` — stays on default branch if omitted
- Preview type inherits from per-repo config if set

Returns `201` with the container info, or `200` if already running.

### Stop

```bash
POST /api/preview/{platform}/{owner}/{repo}/{pr}/stop
```

Runs `docker stop`. Container persists, can be restarted.

### Start

```bash
POST /api/preview/{platform}/{owner}/{repo}/{pr}/start
```

Runs `docker start`, detects the new port (since `-p 0:PORT` assigns a random
host port each start), writes the port file, and waits up to 30s for the app
to become TCP-reachable.

### Restart

```bash
POST /api/preview/{platform}/{owner}/{repo}/{pr}/restart
```

Stop + start with port refresh. Useful after the container crashes or the
port mapping changes.

### Destroy

```bash
DELETE /api/preview/{platform}/{owner}/{repo}/{pr}
```

Runs `docker rm -f`, deletes the port file, and removes any static file
directory. Called automatically when a PR is closed or merged.

## Reverse proxy

### Path-based routing

Requests to `/preview/{platform}/{owner}/{repo}/{pr}/` are proxied to the
container port.

```
Client                     Agent (:9423)
  │                              │
  │ GET /preview/github/o/r/42/  │
  │─────────────────────────────▶│
  │                              │ reads port file → 32768
  │                              │ reverse proxy → localhost:32768
  │◀─────────────────────────────│
  │          HTML response        │
```

The proxy rewrites HTML and CSS to handle absolute paths:
- Injects `<base href="/preview/github/o/r/42/">` for relative URLs
- Rewrites `src="/..."` and `href="/..."` to include the prefix
- Rewrites `/__next/` paths in script code and CSS for Next.js apps
- Follows `Location` redirects (adds prefix)

### Subdomain routing

When `preview_domain` is set, a middleware bypasses path-based routing:

```yaml
preview_domain: "example.com"
```

Requests to `pr42-owner-repo.example.com` are routed directly to the container
by parsing the PR number and repo from the subdomain. No path prefix needed.

```bash
# Instead of:
http://agent.example.com/preview/github/owner/repo/42/

# Use:
https://pr42-owner-repo.example.com/
```

### Subdomain format

```
pr{N}-{safe-owner}-{safe-repo}.{domain}
```

The repo name is lowercased and special characters replaced with `-`.

## Automatic lifecycle

### Build on PR creation

The worker entrypoint (`worker-entrypoint.sh`) automatically builds a preview
after creating a PR. No configuration needed.

### Cleanup on PR close

When a `pull_request.closed` webhook arrives, the handler calls
`preview.Manager.Destroy()` which removes the container, port file, and
static files.

### Reconciliation on restart

When the Agent service starts, `reconcilePreviews()` scans for existing
`agent-preview-*` containers:

- Restarts any that are `Exited` or `Created`
- Detect port mappings and rewrite port files
- Handles the case where Docker assigned new ports after restart

## Port files

Port mappings are persisted in `{preview_dir}/{safe-repo}-{pr}.port`:

```
./preview/owner-repo-42.port  →  "32768\n"
```

The file contains just the host port number. The reverse proxy and API read
this file to know where to forward traffic.

## Static fallback files

When no container can be built, static files from `/workspace/preview/` are
copied to `{preview_dir}/{safe-repo}/{pr}/` and served directly by the Go
file server:

```
./preview/owner-repo/42/
  ├── index.html
  ├── style.css
  └── app.js
```

These are also listed by `GET /api/previews` with `"status": "static"`.
