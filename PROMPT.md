# Agent GitHub

A service that solves GitHub issues using the pi agent — triggered on-demand when a user
explicitly mentions the bot (configured user, defaults to `@agent`) in an issue or comment.

## Architecture

- **Identity**: The service is a **GitHub App**, installed on target repos/orgs. It authenticates
  via GitHub App tokens, not personal access tokens.
- **Event source**: GitHub webhooks deliver `issues.opened`, `issues.edited`, and
  `issue_comment.created` events. The service filters to only react when the bot is
  explicitly mentioned (`@agent`).
- **Runtime**: The service itself runs inside a Docker container (written in **Go**).
  When triggered, it spawns a sibling Docker container for each accepted issue (via Docker
  socket mounting). Worker containers use Node.js (pi agent's runtime).
- **Concurrency**: Max N concurrent containers (default 5, configurable). Overflow goes into
  a queue.
- **CLI / TUI**: The entire system is controllable via a CLI (and optionally a TUI) —
  queue management, container status, configuration, etc.
- **Preview containers**: PR branches are automatically built into containerized previews
  via Nixpacks and served through a reverse proxy. A full REST API allows manual lifecycle
  management (create, stop, start, restart, destroy, list).

## Workflow

1. **A user mentions the bot** in an issue or comment (e.g. `@agent fix this`).
2. **GitHub delivers a webhook** (`issues.opened`, `issues.edited`, or `issue_comment.created`)
   to this service.
3. **The service checks** whether the bot was mentioned:
   a. If yes → accept the job (queue if at capacity).
   b. If no → ignore the event.
4. **When capacity allows, the service spawns a sibling Docker container** that:
   a. Is a Linux-based image with `git`, `gh` (GitHub CLI), Chrome, `agent-browser`,
      and the pi agent pre-installed.
   b. Clones the target repository.
   c. Launches the repo's **devcontainer** — the agent works inside it to ensure a
      consistent, repo-defined environment.
   d. Receives a prompt (via a script) explaining its purpose and the issue to solve.
5. **The pi agent works on the issue:**
   a. If more context is needed, it posts a comment on the issue asking for clarification.
   b. Once done, it creates a branch, commits changes, pushes, and opens a pull request
      via `gh`.
6. **A human reviews the pull request:**
   a. If approved and merged → the sibling container is stopped and removed.
   b. If changes are requested → the agent iterates (details TBD).
7. **Timeout**: generous limit (exact value TBD). Stale containers are killed to free
   capacity.

## Design principles

- **Declarative**: The bot only acts when explicitly invoked. The user decides when to involve it.
- **Isolated**: One Docker container per issue. No state leaks, clean teardown.
- **Self-contained**: The service is itself a Docker container — portable, reproducible.
- **Repo-native**: The agent works inside the repo's own devcontainer, not a generic environment.
- **Sandboxed**: The agent runs inside a container with no access to the host beyond what
  Docker explicitly grants. This is sufficient isolation — no extra sandboxing needed.
- **Controllable**: Everything is managed through a CLI (and optionally a TUI).

## Configuration

Two layers:

| Layer | Mechanism | Examples |
|-------|-----------|----------|
| **Service config** | Config file (YAML/TOML) | Concurrency limit, queue settings, repo allowlist, bot name, webhook secret |
| **Per-container env** | Environment variables | GitHub App credentials, LLM API keys, repo-specific settings passed into the sibling container |

## Technology decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Identity | **GitHub App** | Proper OAuth, fine-grained permissions, no personal token dependency. |
| Event ingestion | **Webhooks** | Real-time, no rate-limit risk. GitHub's own recommendation. |
| Trigger | **Explicit mention** (`@agent`) | Declarative, avoids noise, user-controlled. |
| Isolation | **Docker (sibling containers)** | Clean state per issue, easy teardown. |
| Agent environment | **Repo's devcontainer** | Consistent toolchain per project, maintained by repo owners. |
| Browser automation | **agent-browser** | pi can drive a browser for testing, QA, screenshots, and web interactions. |
| Service runtime | **Docker** | Portable, reproducible anywhere. |
| Concurrency | **Queue + configurable cap** (default 5) | Prevents resource exhaustion, tunable for different hosts. |
| Control interface | **CLI + optional TUI** | Scriptable, inspectable, human-friendly overview. |
| Agent | **pi agent** | Already selected. |
| Service language | **Go** | Single binary, native Docker SDK (same codebase), goroutines for concurrency. |
| Issue triage | **LLM classifier** | New issues are auto-classified: simple issues get assigned to the bot, complex ones get a `needs-human-assistance` label. |

## Runtime contract (happy path)

This is the trace of a single job from webhook to PR:

```
1. GitHub POST /webhook → service receives JSON payload
2. Parse event type (issues.opened | issues.edited | issue_comment.created)
3. Extract body text, check for "@agent" mention
4. If no mention → 200 OK, done
5. Check concurrent container count against configured max (default 5)
6. If at capacity:
   a. Push job to queue
   b. Optionally post a comment: "I'm on it, but I'm at capacity. You're #N in queue."
7. If capacity available:
   a. Create sibling container via Docker API
   b. Inject env vars: GITHUB_TOKEN, REPO_URL, ISSUE_NUMBER, LLM_API_KEY, etc.
   c. Run prompt script: explains purpose, passes issue title + body + context
8. Inside the container:
   a. git clone <repo>
   b. Launch repo's devcontainer
   c. pi agent reads the issue, works on a solution
   d. If more context needed → gh issue comment (posts on the issue)
   e. If needed, use `agent-browser` to test changes (open browser, take screenshots,
      interact with web UI).
   f. When done → git checkout -b, git commit, git push, gh pr create
9. Service detects container exit
10. If exit code 0 and PR created → log, remove container
11. If exit code non-zero → log error, optionally comment on issue with failure
12. If timeout reached → kill container, log, optionally comment
13. Dequeue next job if any
```

### Error states

| Scenario | Behavior |
|----------|----------|
| Repo has no devcontainer | Comment on issue: "This repo doesn't have a devcontainer. I need one to work on it." |
| `gh` auth fails | Container exits non-zero. Service logs the error. Optionally comment. |
| Agent times out | Kill container, comment: "I ran out of time working on this." |
| PR already exists for this issue | Comment linking to existing PR, skip. |
| Mention is ambiguous / no clear task | Comment asking for clarification. |

## GitHub App permissions

The app needs these scopes:

| Permission | Level | Why |
|------------|-------|-----|
| Issues | Read & write | Read issue content, post comments |
| Pull requests | Read & write | Create PRs, post PR comments |
| Contents | Read & write | Clone repo, create branches, push commits |
| Metadata | Read | Required by all GitHub Apps (implicit) |

Webhook subscriptions:
- `issues` (opened, edited)
- `issue_comment` (created)
- `pull_request` (closed) — automatic preview cleanup

## Preview containers

When the worker finishes a PR, it builds a containerized preview using Nixpacks.
The preview is served at `{base_url}/preview/{repo}/{pr}/` via a reverse proxy.

### Build pipeline
1. Worker clones the repo and checks out the PR branch.
2. Devcontainer detected → Node.js installed via mise.
3. **Nixpacks** auto-detects the project type and builds a Docker image.
4. Container started on a random host port with `--restart unless-stopped`.
5. Port written to `{previewDir}/{repo}-{pr}.port`.

### Fallbacks
If Nixpacks fails or the project has no start command:
- **Nginx static**: serve `dist/`, `build/`, `out/`, or `public/` via nginx:alpine.
- **File copy**: copy `preview/` directory to static file server.

### REST API
All endpoints under `/api/preview/{repo}/{pr}`:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/previews` | List all previews |
| `GET` | `/api/preview/{repo}/{pr}` | Container status |
| `POST` | `/api/preview/{repo}/{pr}` | Build & start preview |
| `DELETE` | `/api/preview/{repo}/{pr}` | Stop & remove container |
| `POST` | `/api/preview/{repo}/{pr}/stop` | Stop container |
| `POST` | `/api/preview/{repo}/{pr}/start` | Start stopped container |
| `POST` | `/api/preview/{repo}/{pr}/restart` | Restart container |

Create accepts JSON body with optional `repo_url`, `branch`, `github_token`,
`install_cmd`, `build_cmd`, `start_cmd`, and `env` map. Per-repo nixpacks
config from `config.yaml` is applied automatically.

### Lifecycle
- **Created**: by worker after successful PR, or via REST API.
- **Running**: served via reverse proxy. Survives service restart (reconciliation).
- **Stopped**: via API or manual `docker stop`. Can be restarted.
- **Destroyed**: on PR close (webhook), or via DELETE API. Removes container,
  port file, and static files.

## Open questions (to resolve later)

- Config file schema and env var contract.
- Queue implementation (in-memory? Redis? persisted?).
- CLI / TUI design and commands.
- Agent prompt script structure.
- PR review feedback loop (agent iteration after requested changes).
- Timeout duration.
- Multi-repo / organization-wide deployment.
- GitHub App install flow and webhook secret validation.
- Local dev setup (smee.io / ngrok for webhook delivery).
