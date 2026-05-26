// Package preview manages containerized project previews (DevPreview containers).
// Previews are built from PR branches using Nixpacks and served via reverse proxy.
package preview

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContainerInfo describes a preview container's state.
type ContainerInfo struct {
	Name       string `json:"name"`
	Repo       string `json:"repo"`
	PR         int    `json:"pr"`
	Status     string `json:"status"`
	Port       int    `json:"port,omitempty"`
	Image      string `json:"image,omitempty"`
	Created    string `json:"created,omitempty"`
	PreviewURL string `json:"preview_url,omitempty"`
}

// CreateRequest is the JSON body for POST /api/preview/:repo/:pr.
type CreateRequest struct {
	RepoURL     string            `json:"repo_url"`
	Branch      string            `json:"branch,omitempty"`
	GitHubToken string            `json:"github_token,omitempty"`
	Env         map[string]string `json:"env,omitempty"`

	// Nixpacks overrides
	InstallCmd string `json:"install_cmd,omitempty"`
	BuildCmd   string `json:"build_cmd,omitempty"`
	StartCmd   string `json:"start_cmd,omitempty"`

	// Build method: "nixpacks" (default), "dockerfile", "docker-compose"
	PreviewType string `json:"preview_type,omitempty"`

	// Dockerfile path (default: "Dockerfile")
	DockerfilePath string `json:"dockerfile_path,omitempty"`

	// Docker Compose file path (default: "docker-compose.yml")
	DockerComposeFile string `json:"docker_compose_file,omitempty"`

	// Docker Compose service name to expose
	DockerComposeService string `json:"docker_compose_service,omitempty"`

	// Build args for docker build
	BuildArgs map[string]string `json:"build_args,omitempty"`

	// Internal container port (default: auto-detect)
	Port int `json:"port,omitempty"`
}

// Manager handles preview container lifecycle operations.
type Manager struct {
	previewDir string
	baseURL    string
	mu         sync.Mutex
}

// NewManager creates a new preview Manager.
func NewManager(previewDir, baseURL string) *Manager {
	return &Manager{
		previewDir: previewDir,
		baseURL:    baseURL,
	}
}

// buildScript is the shell script injected into a build container that
// clones the repo, builds with Nixpacks, and starts the preview container.
const buildScript = `#!/usr/bin/env bash
set -euo pipefail
echo "==> Preview builder starting for ${PREVIEW_REPO_NAME}#${PREVIEW_PR_NUMBER}"

# ---- Clone repo ----
# Detect forge type from URL: GitHub uses x-access-token, Gitea uses oauth2
if echo "${PREVIEW_REPO_URL}" | grep -q 'gitea\|gitea-pl'; then
    AUTH_URL=$(echo "${PREVIEW_REPO_URL}" | sed "s|https://|https://oauth2:${GITHUB_TOKEN}@|")
else
    AUTH_URL=$(echo "${PREVIEW_REPO_URL}" | sed "s|https://|https://x-access-token:${GITHUB_TOKEN}@|")
fi
git clone "${AUTH_URL}" /workspace 2>&1
cd /workspace

# ---- Checkout branch ----
if [ -n "${PREVIEW_BRANCH:-}" ]; then
    echo "==> Trying branch ${PREVIEW_BRANCH}..."
    git fetch origin "${PREVIEW_BRANCH}" 2>&1 || true
    if git checkout "${PREVIEW_BRANCH}" 2>&1; then
        echo "==> Checked out ${PREVIEW_BRANCH}"
    elif git checkout -b "${PREVIEW_BRANCH}" "origin/${PREVIEW_BRANCH}" 2>&1; then
        echo "==> Checked out origin/${PREVIEW_BRANCH}"
    else
        echo "==> Branch ${PREVIEW_BRANCH} not found, staying on default branch"
    fi
fi

# ---- Devcontainer setup ----
NODE_VER=""
if [ -f .devcontainer/devcontainer.json ]; then
    echo "==> Devcontainer found, setting up environment..."
    eval "$(~/.local/bin/mise activate bash)" 2>/dev/null || true
    NODE_VER=$(grep -oP '"version":\s*"\K[^"]+' .devcontainer/devcontainer.json 2>/dev/null | head -1 || echo "")
    if [ -z "${NODE_VER}" ]; then
        NODE_VER=$(grep -oP 'typescript-node:\K[0-9]+' .devcontainer/devcontainer.json 2>/dev/null || echo "")
    fi
    if [ -n "${NODE_VER}" ]; then
        echo "==> Installing Node.js ${NODE_VER} via mise..."
        mise install node@${NODE_VER} 2>&1 || true
        mise use node@${NODE_VER} 2>&1 || true
    else
        mise install node@lts 2>&1 || true
        mise use node@lts 2>&1 || true
        NODE_VER="lts"
    fi
    if [ -f package.json ]; then
        npm install 2>&1 || true
    fi
fi

# Write .node-version so Nixpacks picks up the correct Node version
if [ -n "${NODE_VER}" ] && [ "${NODE_VER}" != "lts" ]; then
    echo "${NODE_VER}" > /workspace/.node-version
    echo "==> Wrote .node-version: ${NODE_VER}"
fi

REPO=$(echo "${PREVIEW_REPO_URL}" | sed -n 's|https://[^/]*/[^/]*/\([^.]*\).*|\1|p')
if [ -z "${REPO}" ]; then
    REPO="${PREVIEW_REPO_NAME}"
fi
# Sanitize for filesystem: replace / with -
SAFE_REPO=$(echo "${REPO}" | tr '/' '-')
PREVIEW_IMAGE="preview-${REPO,,}-${PREVIEW_PR_NUMBER}"
PREVIEW_CONTAINER="agent-preview-${REPO,,}-${PREVIEW_PR_NUMBER}"

# ---- Stop any existing preview container ----
docker rm -f "${PREVIEW_CONTAINER}" 2>/dev/null || true

# ---- Build with Dockerfile (if configured) ----
if [ "${PREVIEW_TYPE:-}" = "dockerfile" ]; then
    DOCKERFILE="${PREVIEW_DOCKERFILE:-Dockerfile}"
    if [ ! -f "/workspace/${DOCKERFILE}" ]; then
        echo "PREVIEW_FAILED: ${DOCKERFILE} not found"
        exit 1
    fi

    echo "==> Building with ${DOCKERFILE}..."
    BUILD_ARG_FLAGS=()
    for var in $(env | grep ^PREVIEW_BUILD_ARG_ | cut -d= -f1); do
        arg_name=$(echo "$var" | sed 's/^PREVIEW_BUILD_ARG_//')
        arg_val=$(printenv "$var")
        BUILD_ARG_FLAGS+=(--build-arg "${arg_name}=${arg_val}")
    done

    docker build -t "${PREVIEW_IMAGE}" -f "/workspace/${DOCKERFILE}" "${BUILD_ARG_FLAGS[@]}" /workspace 2>&1

    PORT_MAP=""
    TARGET_PORT="${PREVIEW_PORT:-0}"
    if [ "$TARGET_PORT" != "0" ] && [ -n "$TARGET_PORT" ]; then
        PORT_MAP="-p 0:${TARGET_PORT}"
    else
        PORT_MAP="-p 0:3000 -p 0:8080 -p 0:80"
    fi

    docker run -d --name "${PREVIEW_CONTAINER}" \
        ${PORT_MAP} \
        --restart unless-stopped \
        "${PREVIEW_IMAGE}" 2>&1

    if [ "$(docker inspect -f '{{.State.Running}}' "${PREVIEW_CONTAINER}" 2>/dev/null)" = "true" ]; then
        PORT=$(m.detectPort "${PREVIEW_CONTAINER}")
        # Actually call detectPort inline:
        for cp in 3000 8080 80; do
            PORT=$(docker port "${PREVIEW_CONTAINER}" $cp 2>/dev/null | head -1 | cut -d: -f2 || echo "")
            [ -n "$PORT" ] && break
        done
        if [ -n "$PORT" ]; then
            mkdir -p /agent-preview
            echo "$PORT" > "/agent-preview/${SAFE_REPO}-${PREVIEW_PR_NUMBER}.port"
            echo "==> Dockerfile preview running on port ${PORT}"
            echo "PREVIEW_OK"
            exit 0
        fi
    fi
    echo "==> Dockerfile build succeeded but container not running"
    echo "PREVIEW_FAILED: container exited"
    exit 1
fi

# ---- Build with Docker Compose (if configured) ----
if [ "${PREVIEW_TYPE:-}" = "docker-compose" ]; then
    COMPOSE_FILE="${PREVIEW_COMPOSE_FILE:-docker-compose.yml}"
    if [ ! -f "/workspace/${COMPOSE_FILE}" ]; then
        echo "PREVIEW_FAILED: ${COMPOSE_FILE} not found"
        exit 1
    fi

    echo "==> Building with ${COMPOSE_FILE}..."
    cd /workspace
    docker compose -f "${COMPOSE_FILE}" build 2>&1
    docker compose -f "${COMPOSE_FILE}" up -d 2>&1

    # Find the service to expose
    SERVICE_NAME="${PREVIEW_COMPOSE_SERVICE:-}"
    if [ -z "$SERVICE_NAME" ]; then
        # Pick the first service with exposed ports
        SERVICE_NAME=$(docker compose -f "${COMPOSE_FILE}" ps --format json 2>/dev/null | head -1 | grep -oP '"Service":"\K[^"]+' || echo "")
    fi

    if [ -n "$SERVICE_NAME" ]; then
        CONTAINER_ID=$(docker compose -f "${COMPOSE_FILE}" ps -q "${SERVICE_NAME}" 2>/dev/null || echo "")
        if [ -n "$CONTAINER_ID" ]; then
            # Re-tag the compose container as our preview container so lifecycle management works
            docker rename "${CONTAINER_ID}" "${PREVIEW_CONTAINER}" 2>/dev/null || true

            # Detect port
            for cp in 3000 8080 80; do
                PORT=$(docker port "${PREVIEW_CONTAINER}" $cp 2>/dev/null | head -1 | cut -d: -f2 || echo "")
                [ -n "$PORT" ] && break
            done
            if [ -n "$PORT" ]; then
                mkdir -p /agent-preview
                echo "$PORT" > "/agent-preview/${SAFE_REPO}-${PREVIEW_PR_NUMBER}.port"
                echo "==> Docker Compose preview running on port ${PORT}"
                echo "PREVIEW_OK"
                exit 0
            fi
        fi
    fi
    echo "==> Docker Compose build succeeded but could not detect port"
    echo "PREVIEW_FAILED: no service port found"
    exit 1
fi

# ---- Build with Nixpacks (default) ----
# Export env vars that Nixpacks reads for version detection
[ -n "${NIXPACKS_NODE_VERSION:-}" ] && export NIXPACKS_NODE_VERSION

NIXPACKS_ARGS=()
[ -n "${PREVIEW_INSTALL_CMD:-}" ] && NIXPACKS_ARGS+=(--install-cmd "${PREVIEW_INSTALL_CMD}")
[ -n "${PREVIEW_BUILD_CMD:-}" ]   && NIXPACKS_ARGS+=(--build-cmd "${PREVIEW_BUILD_CMD}")
[ -n "${PREVIEW_START_CMD:-}" ]   && NIXPACKS_ARGS+=(--start-cmd "${PREVIEW_START_CMD}")

echo "==> Nixpacks building ${PREVIEW_IMAGE}..."
BUILT=false
if nixpacks build /workspace --name "${PREVIEW_IMAGE}" "${NIXPACKS_ARGS[@]}" 2>&1; then
    echo "==> Nixpacks build succeeded"
    echo "==> Starting preview container..."
    docker run -d --name "${PREVIEW_CONTAINER}" \
        -p 0:3000 -p 0:8080 -p 0:80 \
        --restart unless-stopped \
        "${PREVIEW_IMAGE}" 2>&1

    if [ "$(docker inspect -f '{{.State.Running}}' "${PREVIEW_CONTAINER}" 2>/dev/null)" = "true" ]; then
        for i in $(seq 1 15); do
            PORT=$(docker port "${PREVIEW_CONTAINER}" 3000 2>/dev/null | head -1 | cut -d: -f2 || echo "")
            if [ -z "$PORT" ]; then
                PORT=$(docker port "${PREVIEW_CONTAINER}" 8080 2>/dev/null | head -1 | cut -d: -f2 || echo "")
            fi
            if [ -z "$PORT" ]; then
                PORT=$(docker port "${PREVIEW_CONTAINER}" 80 2>/dev/null | head -1 | cut -d: -f2 || echo "")
            fi
            [ -n "$PORT" ] && break
            sleep 1
        done
        if [ -n "$PORT" ]; then
            mkdir -p /agent-preview
            echo "$PORT" > "/agent-preview/${SAFE_REPO}-${PREVIEW_PR_NUMBER}.port"
            echo "==> Preview running on port ${PORT}"
            BUILT=true
        fi
    fi
fi

# ---- Static fallback: nginx ----
if [ "$BUILT" != true ]; then
    echo "==> Nixpacks build failed or no start command, trying static nginx..."
    STATIC_DIR=""
    for dir in dist build out public; do
        if [ -d "/workspace/$dir" ] && [ "$(ls -A /workspace/$dir 2>/dev/null)" ]; then
            STATIC_DIR="$dir"
            break
        fi
    done

    if [ -n "$STATIC_DIR" ]; then
        cat > /workspace/Dockerfile.preview <<DOCKERFILE
FROM nginx:alpine
COPY ${STATIC_DIR} /usr/share/nginx/html
DOCKERFILE
        echo "==> Building static preview from /workspace/${STATIC_DIR}..."
        cd /workspace && docker build -t "${PREVIEW_IMAGE}" -f Dockerfile.preview . 2>&1
        docker run -d --name "${PREVIEW_CONTAINER}" -p 0:80 "${PREVIEW_IMAGE}" 2>&1
        for i in $(seq 1 10); do
            PORT=$(docker port "${PREVIEW_CONTAINER}" 80 2>/dev/null | head -1 | cut -d: -f2 || echo "")
            [ -n "$PORT" ] && break
            sleep 1
        done
        if [ -n "$PORT" ]; then
            mkdir -p /agent-preview
            echo "$PORT" > "/agent-preview/${SAFE_REPO}-${PREVIEW_PR_NUMBER}.port"
            echo "==> Static preview running on port ${PORT}"
            BUILT=true
        fi
    fi
fi

# ---- File copy fallback ----
if [ "$BUILT" != true ]; then
    echo "==> Falling back to file copy..."
    if [ -d /workspace/preview ] && [ "$(ls -A /workspace/preview 2>/dev/null)" ]; then
        mkdir -p "/agent-preview/${SAFE_REPO}/${PREVIEW_PR_NUMBER}"
        cp -r /workspace/preview/* "/agent-preview/${SAFE_REPO}/${PREVIEW_PR_NUMBER}/" 2>&1
        echo "0" > "/agent-preview/${SAFE_REPO}-${PREVIEW_PR_NUMBER}.port"
        echo "==> Static files copied"
        BUILT=true
    fi
fi

if [ "$BUILT" = true ]; then
    echo "PREVIEW_OK"
else
    echo "PREVIEW_FAILED: could not build or find static output"
    exit 1
fi
`

// Create builds and starts a preview container for a repository PR.
// It spawns a one-shot build container using the agent-worker image.
func (m *Manager) Create(req CreateRequest, repoName string, prNumber int) (*ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	containerName := containerNameFor(repoName, prNumber)

	// Check if already running
	if info, _ := m.inspect(containerName); info != nil && info.Status == "running" {
		info.Repo = repoName
		info.PR = prNumber
		info.PreviewURL = m.previewURL(repoName, prNumber)
		return info, nil
	}

	// Stop existing container if any
	exec.Command("docker", "rm", "-f", containerName).Run()

	// Cleanup stale port file
	os.Remove(m.portFile(repoName, prNumber))

	log.Printf("preview: creating %s (repo=%s pr=%d branch=%s)", containerName, repoName, prNumber, req.Branch)

	branch := req.Branch
	// If no branch specified, the build script will stay on the repo's default branch.

	// Spawn build container (one-shot, uses agent-worker image)
	buildArgs := []string{
		"run",
		"--rm",
		"--name", containerName + "-build",
		"--entrypoint", "bash",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", m.previewDir + ":/agent-preview",
		"-e", "GITHUB_TOKEN=" + req.GitHubToken,
		"-e", "PREVIEW_REPO_URL=" + req.RepoURL,
		"-e", "PREVIEW_REPO_NAME=" + repoName,
		"-e", "PREVIEW_PR_NUMBER=" + strconv.Itoa(prNumber),
		"-e", "PREVIEW_BRANCH=" + branch,
		"-e", "PREVIEW_INSTALL_CMD=" + req.InstallCmd,
		"-e", "PREVIEW_BUILD_CMD=" + req.BuildCmd,
		"-e", "PREVIEW_START_CMD=" + req.StartCmd,
		"-e", "PREVIEW_TYPE=" + req.PreviewType,
		"-e", "PREVIEW_DOCKERFILE=" + req.DockerfilePath,
		"-e", "PREVIEW_COMPOSE_FILE=" + req.DockerComposeFile,
		"-e", "PREVIEW_COMPOSE_SERVICE=" + req.DockerComposeService,
		"-e", "PREVIEW_PORT=" + strconv.Itoa(req.Port),
		"--network", "host",
		"agent-worker:latest",
		"-c", buildScript,
	}

	// Inject build args for Docker
	for k, v := range req.BuildArgs {
		buildArgs = append(buildArgs, "-e", "PREVIEW_BUILD_ARG_"+k+"="+v)
	}

	// Inject custom env vars
	for k, v := range req.Env {
		buildArgs = append(buildArgs, "-e", k+"="+v)
	}

	cmd := exec.Command("docker", buildArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("build container failed: %w", err)
	}

	// Verify container is running or static fallback exists
	info, err := m.inspect(containerName)
	if err != nil || info == nil {
		// Might be static fallback
		info = &ContainerInfo{
			Name:   containerName,
			Repo:   repoName,
			PR:     prNumber,
			Status: "static",
		}
		// Check if static files exist
		staticDir := filepath.Join(m.previewDir, repoName, strconv.Itoa(prNumber))
		if _, err := os.Stat(staticDir); os.IsNotExist(err) {
			return nil, fmt.Errorf("build completed but no container or static files found")
		}
	}
	info.Repo = repoName
	info.PR = prNumber
	info.PreviewURL = m.previewURL(repoName, prNumber)
	return info, nil
}

// Stop stops a running preview container.
func (m *Manager) Stop(repoName string, prNumber int) error {
	containerName := containerNameFor(repoName, prNumber)
	log.Printf("preview: stopping %s", containerName)
	cmd := exec.Command("docker", "stop", containerName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop %s: %w\n%s", containerName, err, string(out))
	}
	return nil
}

// Start starts a stopped preview container and refreshes its port mapping.
// Waits up to 30s for the app to become reachable before returning.
func (m *Manager) Start(repoName string, prNumber int) (*ContainerInfo, error) {
	containerName := containerNameFor(repoName, prNumber)
	log.Printf("preview: starting %s", containerName)

	cmd := exec.Command("docker", "start", containerName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker start %s: %w\n%s", containerName, err, string(out))
	}

	port := m.waitForPort(containerName, 30*time.Second)
	if port > 0 {
		os.WriteFile(m.portFile(repoName, prNumber), []byte(strconv.Itoa(port)), 0644)
		log.Printf("preview: %s ready on port %d", containerName, port)
	} else {
		// Write whatever port docker reports, even if not listening yet
		port = m.detectPort(containerName)
		if port > 0 {
			os.WriteFile(m.portFile(repoName, prNumber), []byte(strconv.Itoa(port)), 0644)
		}
		log.Printf("preview: %s started but not reachable on port %d", containerName, port)
	}

	info, _ := m.inspect(containerName)
	if info == nil {
		info = &ContainerInfo{Name: containerName, Repo: repoName, PR: prNumber, Status: "unknown"}
	}
	if port > 0 {
		info.Port = port
	}
	info.Repo = repoName
	info.PR = prNumber
	info.PreviewURL = m.previewURL(repoName, prNumber)
	return info, nil
}

// Restart restarts a preview container.
// Waits up to 30s for the app to become reachable before returning.
func (m *Manager) Restart(repoName string, prNumber int) (*ContainerInfo, error) {
	containerName := containerNameFor(repoName, prNumber)
	log.Printf("preview: restarting %s", containerName)

	// docker restart changes the port mapping on -p 0:xxxx containers,
	// so stop+start to ensure we track the new port.
	exec.Command("docker", "stop", containerName).Run()
	cmd := exec.Command("docker", "start", containerName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker start %s: %w\n%s", containerName, err, string(out))
	}

	port := m.waitForPort(containerName, 30*time.Second)
	if port > 0 {
		os.WriteFile(m.portFile(repoName, prNumber), []byte(strconv.Itoa(port)), 0644)
		log.Printf("preview: %s ready on port %d", containerName, port)
	} else {
		port = m.detectPort(containerName)
		if port > 0 {
			os.WriteFile(m.portFile(repoName, prNumber), []byte(strconv.Itoa(port)), 0644)
		}
		log.Printf("preview: %s restarted but not reachable on port %d", containerName, port)
	}

	info, _ := m.inspect(containerName)
	if info == nil {
		info = &ContainerInfo{Name: containerName, Repo: repoName, PR: prNumber, Status: "unknown"}
	}
	if port > 0 {
		info.Port = port
	}
	info.Repo = repoName
	info.PR = prNumber
	info.PreviewURL = m.previewURL(repoName, prNumber)
	return info, nil
}

// Destroy stops and removes a preview container and its port file.
func (m *Manager) Destroy(repoName string, prNumber int) error {
	containerName := containerNameFor(repoName, prNumber)
	log.Printf("preview: destroying %s", containerName)

	// Remove container (force)
	exec.Command("docker", "rm", "-f", containerName).Run()

	// Cleanup port file
	os.Remove(m.portFile(repoName, prNumber))

	// Cleanup static files dir if exists
	safeRepo := strings.ReplaceAll(repoName, "/", "-")
	staticDir := filepath.Join(m.previewDir, safeRepo, strconv.Itoa(prNumber))
	os.RemoveAll(staticDir)

	return nil
}

// Status returns information about a preview container.
func (m *Manager) Status(repoName string, prNumber int) (*ContainerInfo, error) {
	containerName := containerNameFor(repoName, prNumber)
	info, err := m.inspect(containerName)
	if err != nil {
		return nil, err
	}
	if info == nil {
		// Check for static fallback
		safeRepo := strings.ReplaceAll(repoName, "/", "-")
		staticDir := filepath.Join(m.previewDir, safeRepo, strconv.Itoa(prNumber))
		if _, err := os.Stat(staticDir); err == nil {
			return &ContainerInfo{
				Name:       containerName,
				Repo:       repoName,
				PR:         prNumber,
				Status:     "static",
				PreviewURL: m.previewURL(repoName, prNumber),
			}, nil
		}
		return nil, fmt.Errorf("preview not found: %s/%d", repoName, prNumber)
	}
	info.Repo = repoName
	info.PR = prNumber
	info.PreviewURL = m.previewURL(repoName, prNumber)
	return info, nil
}

// List returns all preview containers (both running and static).
func (m *Manager) List() ([]ContainerInfo, error) {
	var results []ContainerInfo

	// Running containers
	out, err := exec.Command("docker", "ps", "-a",
		"--filter", "name=agent-preview-",
		"--format", "{{.Names}}\t{{.Status}}\t{{.Image}}\t{{.CreatedAt}}\t{{.Ports}}",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 3 {
			continue
		}
		name, status, image, created, ports := parts[0], parts[1], parts[2], "", ""
		if len(parts) > 3 {
			created = parts[3]
		}
		if len(parts) > 4 {
			ports = parts[4]
		}

		repo, pr := parseContainerName(name)
		if repo == "" {
			continue
		}

		info := ContainerInfo{
			Name:    name,
			Repo:    repo,
			PR:      pr,
			Status:  containerState(status),
			Image:   image,
			Created: created,
			Port:    parsePort(ports),
		}
		if info.Port == 0 {
			info.Port = m.readPortFile(repo, pr)
		}
		info.PreviewURL = m.previewURL(repo, pr)
		results = append(results, info)
	}

	// Static fallback previews (dirs in previewDir without containers)
	entries, _ := os.ReadDir(m.previewDir)
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.Repo+"-"+strconv.Itoa(r.PR)] = true
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoName := entry.Name()
		prDirs, _ := os.ReadDir(filepath.Join(m.previewDir, repoName))
		for _, prDir := range prDirs {
			if !prDir.IsDir() {
				continue
			}
			pr, err := strconv.Atoi(prDir.Name())
			if err != nil {
				continue
			}
			key := repoName + "-" + strconv.Itoa(pr)
			if seen[key] {
				continue
			}
			results = append(results, ContainerInfo{
				Name:       "static-" + key,
				Repo:       repoName,
				PR:         pr,
				Status:     "static",
				PreviewURL: m.previewURL(repoName, pr),
			})
		}
	}

	return results, nil
}

// ---- Helpers ----

func containerNameFor(repo string, pr int) string {
	raw := fmt.Sprintf("agent-preview-%s-%d", repo, pr)
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(raw))
}

func (m *Manager) portFile(repo string, pr int) string {
	// Replace / with - for filesystem safety (repo is owner/repo format)
	safe := strings.ReplaceAll(repo, "/", "-")
	return filepath.Join(m.previewDir, safe+"-"+strconv.Itoa(pr)+".port")
}

func (m *Manager) readPortFile(repo string, pr int) int {
	data, err := os.ReadFile(m.portFile(repo, pr))
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return port
}

func (m *Manager) inspect(containerName string) (*ContainerInfo, error) {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{.Name}}\t{{.State.Status}}\t{{.Config.Image}}\t{{.Created}}",
		containerName,
	).Output()
	if err != nil {
		return nil, nil // container not found is not an error
	}

	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 4)
	if len(parts) < 2 {
		return nil, nil
	}

	info := &ContainerInfo{
		Name:   containerName,
		Status: parts[1],
	}
	if len(parts) > 2 {
		info.Image = parts[2]
	}
	if len(parts) > 3 {
		info.Created = parts[3]
	}

	info.Port = m.detectPort(containerName)
	return info, nil
}

func (m *Manager) detectPort(containerName string) int {
	for _, containerPort := range []string{"3000", "8080", "80"} {
		out, err := exec.Command("docker", "port", containerName, containerPort).Output()
		if err != nil {
			continue
		}
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 {
			port, err := strconv.Atoi(parts[1])
			if err == nil && port > 0 {
				return port
			}
		}
	}
	return 0
}

// waitForPort polls docker port until the app is reachable via TCP, or timeout.
// Returns the port number if reachable, 0 otherwise.
func (m *Manager) waitForPort(containerName string, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	var lastPort int

	for time.Now().Before(deadline) {
		port := m.detectPort(containerName)
		if port > 0 {
			lastPort = port
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err == nil {
				conn.Close()
				return port
			}
		}
		time.Sleep(1 * time.Second)
	}
	return lastPort
}

func (m *Manager) previewURL(repo string, pr int) string {
	// repo is "owner/repo" format; extract parts for the URL
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) == 2 {
		return fmt.Sprintf("%s/preview/github/%s/%s/%d/", m.baseURL, parts[0], parts[1], pr)
	}
	return fmt.Sprintf("%s/preview/github/%s/%d/", m.baseURL, repo, pr)
}

// PreviewURLForPlatform builds a preview URL for a specific forge platform.
func (m *Manager) PreviewURLForPlatform(platform, repo string, pr int) string {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) == 2 {
		return fmt.Sprintf("%s/preview/%s/%s/%s/%d/", m.baseURL, platform, parts[0], parts[1], pr)
	}
	return fmt.Sprintf("%s/preview/%s/%s/%d/", m.baseURL, platform, repo, pr)
}

func parseContainerName(name string) (repo string, pr int) {
	trimmed := strings.TrimPrefix(name, "agent-preview-")
	lastDash := strings.LastIndex(trimmed, "-")
	if lastDash < 0 {
		return "", 0
	}
	repo = trimmed[:lastDash]
	pr, _ = strconv.Atoi(trimmed[lastDash+1:])
	return
}

func parsePort(ports string) int {
	if ports == "" {
		return 0
	}
	for _, mapping := range strings.Split(ports, ", ") {
		parts := strings.SplitN(mapping, "->", 2)
		if len(parts) != 2 {
			continue
		}
		hostPart := strings.Split(parts[0], ":")
		if len(hostPart) == 2 {
			port, err := strconv.Atoi(hostPart[1])
			if err == nil && port > 0 {
				return port
			}
		}
	}
	return 0
}

func containerState(status string) string {
	status = strings.ToLower(status)
	switch {
	case strings.HasPrefix(status, "up"):
		return "running"
	case strings.HasPrefix(status, "exited"):
		return "stopped"
	case strings.HasPrefix(status, "created"):
		return "created"
	default:
		return status
	}
}
