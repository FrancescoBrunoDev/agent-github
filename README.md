# Otto — AI GitHub Agent

Otto is a GitHub App service that automatically solves GitHub issues by
spawning isolated Docker containers running the [pi coding
agent](https://github.com/earendil-works/pi) with a local LLM.

**Trigger:** mention `@otto` in an issue or PR comment. Otto responds by
creating a branch, implementing the fix, opening a pull request, and
reporting back — fully automated.

## How it works

```
Issue comment "@otto fix this"
         │
         ▼
   GitHub webhook
         │
         ▼
   Otto service (Go)
    │         │
    ▼         ▼
  Queue   Worker manager
            │
            ▼
      docker run otto-worker
       ├── gh auth, git clone
       ├── devcontainer setup (mise)
       ├── pi agent ──▶ local LLM
       ├── git commit / push / PR
       └── cleanup
```

## Features

- **On-demand**: only acts when explicitly mentioned (`@otto`)
- **Isolated**: each job runs in its own Docker container
- **Local LLM**: no cloud API keys needed (uses llamacpp)
- **Devcontainer-aware**: reads `.devcontainer/devcontainer.json` for tool versions
- **Continuation**: comment `@otto` on a PR to trigger further changes
- **Screenshots**: agent-browser artifacts are committed to the PR branch
- **FIFO queue** with configurable concurrency
- **Deduplication**: won't double-process the same issue/PR

## Requirements

- **Go 1.24+** for the service
- **Docker** with socket access
- **pi agent** (Node.js) in the worker image
- **llamacpp** or any OpenAI-compatible local LLM
- **GitHub App credentials** (see [setup](docs/setup.md))

## Quick start

```bash
# Build
go build -o otto ./cmd/otto

# Configure
cp config.example.yaml config.yaml
$EDITOR config.yaml   # fill in GitHub App credentials

# Run
./otto serve
```

## Documentation

- [Setup: creating a GitHub App](docs/setup.md)
- [Configuration reference](docs/config.md)
