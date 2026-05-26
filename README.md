# Agent — AI GitHub Agent

Agent is a GitHub App service that automatically solves GitHub issues by
spawning isolated Docker containers running the [pi coding
agent](https://github.com/earendil-works/pi) with a local LLM.

**Trigger:** assign an issue to the bot user (e.g. `@agent-bot`) and Agent
picks it up automatically. You can also mention `@agent` in a comment for
legacy support or continuation mode.

## How it works

```
Assign issue to @agent-bot
         │
         ▼
   GitHub webhook (issues: assigned)
         │
         ▼
   Agent service (Go)
    │         │
    ▼         ▼
  Queue   Worker manager
            │
            ▼
      docker run agent-worker
       ├── gh auth, git clone
       ├── devcontainer setup (mise)
       ├── pi agent ──▶ local LLM
       ├── git commit / push / PR
       └── cleanup
```

## Features

- **Assignment-driven**: assign an issue to the bot user and Agent starts working
- **Mention fallback**: `@agent` in comments still works for legacy and PR feedback
- **Isolated**: each job runs in its own Docker container
- **Local LLM**: no cloud API keys needed (uses llamacpp)
- **Devcontainer-aware**: reads `.devcontainer/devcontainer.json` for tool versions
- **Continuation**: comment `@agent` on a PR to trigger further changes
- **Screenshots**: agent-browser artifacts are committed to the PR branch
- **FIFO queue** with configurable concurrency
- **Deduplication**: won't double-process the same issue/PR
- **Preview containers**: automatic Nixpacks builds with REST API lifecycle management (create/stop/start/restart/destroy)
- **Reverse proxy**: `/preview/{repo}/{pr}/` → running container or static fallback

## Requirements

- **Go 1.24+** for the service
- **Docker** with socket access
- **pi agent** (Node.js) in the worker image
- **llamacpp** or any OpenAI-compatible local LLM
- **GitHub App credentials** (see [setup](docs/setup.md))

## Quick start

```bash
# Build
go build -o agent ./cmd/agent

# Configure
cp config.example.yaml config.yaml
$EDITOR config.yaml   # fill in GitHub App credentials

# Run
./agent serve
```

## Documentation

- [Setup: creating a GitHub App](docs/setup.md)
- [Configuration reference](docs/config.md)

## Preview API

Previews are containerized builds of PR branches, served at `/preview/{repo}/{pr}/`.
Manage them via REST API:

```bash
# List all previews
curl http://localhost:9423/api/previews

# Create preview for PR #42
curl -X POST http://localhost:9423/api/preview/owner/repo/42 \
  -H "Content-Type: application/json" \
  -d '{"repo_url": "https://github.com/owner/repo.git"}'

# Check status
curl http://localhost:9423/api/preview/owner/repo/42

# Stop / start / restart
curl -X POST http://localhost:9423/api/preview/owner/repo/42/stop
curl -X POST http://localhost:9423/api/preview/owner/repo/42/start
curl -X POST http://localhost:9423/api/preview/owner/repo/42/restart

# Destroy
curl -X DELETE http://localhost:9423/api/preview/owner/repo/42
```
