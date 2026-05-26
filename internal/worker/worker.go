package worker

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	workerImage = "agent-worker:latest"
)

// Job represents a single issue to be solved.
type Job struct {
	ID         string
	RepoURL    string
	RepoName   string
	IssueNum   int
	IssueTitle string
	IssueBody  string
	// GitHubToken is the installation access token for gh CLI and git operations.
	GitHubToken string
	// Continue indicates this is a continuation of an existing PR (comment feedback).
	Continue bool
	// Branch is the git branch to work on (for continuation, this is the existing PR head).
	Branch string
	// Feedback is the new comment text to feed to pi agent (continuation mode).
	Feedback string
	// EnvVars are extra environment variables injected into the worker container.
	// Populated from config repos.<owner/repo>.env.
	EnvVars map[string]string
	// Nixpacks overrides for containerized preview builds.
	NixpacksInstallCmd string
	NixpacksBuildCmd   string
	NixpacksStartCmd   string

	// Preview build method (from repo config).
	PreviewType              string
	PreviewDockerfilePath    string
	PreviewComposeFile       string
	PreviewComposeService    string
	PreviewPort              int
	PreviewBuildArgs         map[string]string
}

// Status is the current state of a job.
type Status string

const (
	StatusQueued   Status = "queued"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusTimedOut Status = "timed_out"
)

// Info holds the current state of a worker job.
type Info struct {
	Job         Job
	Status      Status
	ContainerID string
	Error       string
}

// Manager spawns and tracks Docker containers for jobs.
type Manager struct {
	mu        sync.Mutex
	maxWorkers int
	active     map[string]*Info // containerName → Info
}

// NewManager creates a worker Manager.
func NewManager(maxWorkers int) (*Manager, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker not found in PATH: %w", err)
	}

	return &Manager{
		maxWorkers: maxWorkers,
		active:     make(map[string]*Info),
	}, nil
}

// Start begins processing a job in a new Docker container.
func (m *Manager) Start(ctx context.Context, job Job) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.active) >= m.maxWorkers {
		return fmt.Errorf("at capacity (%d/%d)", len(m.active), m.maxWorkers)
	}

	containerName := sanitizeName("agent-" + strings.ReplaceAll(job.ID, "/", "-"))

	// Clean up any previous container with the same name
	exec.Command("docker", "rm", "-f", containerName).Run()

	log.Printf("worker: starting %s", containerName)

	// Pi settings & models from env (set by agent serve from config)
	piSettings := os.Getenv("AGENT_PI_SETTINGS")
	if piSettings == "" {
		piSettings = "{}"
	}
	piModels := os.Getenv("AGENT_PI_MODELS")
	if piModels == "" {
		piModels = "{}"
	}

	// docker run --rm -d --name <name> \
	//   -e GITHUB_TOKEN=... -e REPO_URL=... -e ISSUE_NUMBER=... \
	//   -e ISSUE_TITLE=... -e ISSUE_BODY=... -e LLM_API_KEY=... \
	//   -v /var/run/docker.sock:/var/run/docker.sock \
	//   -v <mediaDir>:/agent-media -v <previewDir>:/agent-preview \
	//   agent-worker:latest
	mediaDir := os.Getenv("AGENT_MEDIA_DIR")
	if mediaDir == "" {
		mediaDir = os.TempDir() + "/agent-media"
	}
	os.MkdirAll(mediaDir, 0755)

	previewDir := os.Getenv("AGENT_PREVIEW_DIR")
	if previewDir == "" {
		previewDir = os.TempDir() + "/agent-preview"
	}
	os.MkdirAll(previewDir, 0755)

	args := []string{
		"run",
		"-d",
		"--name", containerName,
		"-e", "GITHUB_TOKEN=" + job.GitHubToken,
		"-e", "REPO_URL=" + job.RepoURL,
		"-e", "ISSUE_NUMBER=" + fmt.Sprintf("%d", job.IssueNum),
		"-e", "ISSUE_TITLE=" + job.IssueTitle,
		"-e", "ISSUE_BODY=" + job.IssueBody,
		"-e", "LLM_API_KEY=" + envOrEmpty("LLM_API_KEY"),
		"-e", "AGENT_PI_SETTINGS=" + piSettings,
		"-e", "AGENT_PI_MODELS=" + piModels,
		"-e", "AGENT_PI_AUTH=" + envOrEmpty("AGENT_PI_AUTH"),
		"-e", fmt.Sprintf("AGENT_CONTINUE=%v", job.Continue),
		"-e", "AGENT_FEEDBACK=" + job.Feedback,
		"-e", "AGENT_BRANCH=" + job.Branch,
		"-e", "AGENT_BASE_URL=" + os.Getenv("AGENT_BASE_URL"),
		"-e", "AGENT_BOT_USER=" + envOrEmpty("AGENT_BOT_USER"),
		"-e", "AGENT_BOT_TOKEN=" + envOrEmpty("AGENT_BOT_TOKEN"),
		"-e", "AGENT_TRIGGER_TYPE=" + envOrEmpty("AGENT_TRIGGER_TYPE"),
		"-e", "AGENT_PLATFORM=" + envOrEmpty("AGENT_PLATFORM"),
		"-e", "AGENT_GITEA_BOT_USER=" + envOrEmpty("AGENT_GITEA_BOT_USER"),
		"-e", "AGENT_GITEA_BOT_TOKEN=" + envOrEmpty("AGENT_GITEA_BOT_TOKEN"),
		"-e", "AGENT_GITEA_BASE_URL=" + envOrEmpty("AGENT_GITEA_BASE_URL"),
		"-e", "AGENT_NIXPACKS_INSTALL_CMD=" + job.NixpacksInstallCmd,
		"-e", "AGENT_NIXPACKS_BUILD_CMD=" + job.NixpacksBuildCmd,
		"-e", "AGENT_NIXPACKS_START_CMD=" + job.NixpacksStartCmd,
		"-e", "AGENT_PREVIEW_TYPE=" + job.PreviewType,
		"-e", "AGENT_PREVIEW_DOCKERFILE=" + job.PreviewDockerfilePath,
		"-e", "AGENT_PREVIEW_COMPOSE_FILE=" + job.PreviewComposeFile,
		"-e", "AGENT_PREVIEW_COMPOSE_SERVICE=" + job.PreviewComposeService,
		"-e", fmt.Sprintf("AGENT_PREVIEW_PORT=%d", job.PreviewPort),
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", mediaDir + ":/agent-media",
		"-v", previewDir + ":/agent-preview",
		"--network", "host",
		workerImage,
	}

	// Inject per-repo environment variables
	for k, v := range job.EnvVars {
		args = append(args, "-e", k+"="+v)
	}

	// Inject preview build args
	for k, v := range job.PreviewBuildArgs {
		args = append(args, "-e", "PREVIEW_BUILD_ARG_"+k+"="+v)
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run: %w\n%s", err, string(out))
	}

	containerID := strings.TrimSpace(string(out))

	info := &Info{
		Job:         job,
		Status:      StatusRunning,
		ContainerID: containerID,
	}
	m.active[containerName] = info

	// Monitor in background
	go m.monitor(containerName, containerID)

	return nil
}

// monitor streams container logs in real-time, waits for exit, then cleans up.
func (m *Manager) monitor(name, containerID string) {
	// Stream logs in real-time
	go m.streamLogs(name, containerID)

	// Wait for container to exit
	cmd := exec.Command("docker", "wait", containerID)
	waitOut, waitErr := cmd.Output()
	exitCode := strings.TrimSpace(string(waitOut))
	if waitErr != nil && exitCode == "" {
		log.Printf("worker: %s wait error: %v", name, waitErr)
		exitCode = "1"
	}
	log.Printf("worker: %s exited with code %s", name, exitCode)

	m.mu.Lock()
	info, ok := m.active[name]
	if ok {
		if exitCode == "0" {
			info.Status = StatusDone
		} else {
			info.Status = StatusFailed
			info.Error = fmt.Sprintf("exit code %s", exitCode)
		}
	}
	m.mu.Unlock()

	// Clean up
	m.clientRemove(containerID)
}

// streamLogs follows container logs and prints them with a prefix.
func (m *Manager) streamLogs(name, containerID string) {
	cmd := exec.Command("docker", "logs", "-f", containerID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		log.Printf("[%s] %s", name, scanner.Text())
	}
	// docker logs -f exits when container exits; ignore errors
	cmd.Wait()
}

// Active returns the number of currently running workers.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Capacity returns maxWorkers.
func (m *Manager) Capacity() int {
	return m.maxWorkers
}

// IsActive checks if a job ID is currently being worked on.
func (m *Manager) IsActive(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	containerName := sanitizeName("agent-" + strings.ReplaceAll(jobID, "/", "-"))
	_, ok := m.active[containerName]
	return ok
}

// List returns a snapshot of all tracked jobs (active and recently finished).
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Info, 0, len(m.active))
	for _, info := range m.active {
		out = append(out, *info)
	}
	return out
}

// sanitizeName makes a string safe for use as a container name.
func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
}

func envOrEmpty(key string) string {
	return os.Getenv(key)
}

func (m *Manager) clientRemove(containerID string) {
	cmd := exec.Command("docker", "rm", "-f", containerID)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("worker: remove container %s: %v\n%s", containerID[:12], err, string(out))
	}
}
