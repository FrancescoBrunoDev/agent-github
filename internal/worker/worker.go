package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	workerImage = "otto-worker:latest"
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
	// Continue indicates this is a continuation of an existing Otto PR (comment feedback).
	Continue bool
	// Branch is the git branch to work on (for continuation, this is the existing PR head).
	Branch string
	// Feedback is the new comment text to feed to pi agent (continuation mode).
	Feedback string
	// EnvVars are extra environment variables injected into the worker container.
	// Populated from config repos.<owner/repo>.env.
	EnvVars map[string]string
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

	containerName := sanitizeName("otto-" + strings.ReplaceAll(job.ID, "/", "-"))

	// Clean up any previous container with the same name
	exec.Command("docker", "rm", "-f", containerName).Run()

	log.Printf("worker: starting %s", containerName)

	// Pi settings & models from env (set by otto serve from config)
	piSettings := os.Getenv("OTTO_PI_SETTINGS")
	if piSettings == "" {
		piSettings = "{}"
	}
	piModels := os.Getenv("OTTO_PI_MODELS")
	if piModels == "" {
		piModels = "{}"
	}

	// docker run --rm -d --name <name> \
	//   -e GITHUB_TOKEN=... -e REPO_URL=... -e ISSUE_NUMBER=... \
	//   -e ISSUE_TITLE=... -e ISSUE_BODY=... -e LLM_API_KEY=... \
	//   -v /var/run/docker.sock:/var/run/docker.sock \
	//   otto-worker:latest
	mediaDir := os.Getenv("OTTO_MEDIA_DIR")
	if mediaDir == "" {
		mediaDir = os.TempDir() + "/otto-media"
	}
	os.MkdirAll(mediaDir, 0755)

	args := []string{
		"run",
		"--name", containerName,
		"-e", "GITHUB_TOKEN=" + job.GitHubToken,
		"-e", "REPO_URL=" + job.RepoURL,
		"-e", "ISSUE_NUMBER=" + fmt.Sprintf("%d", job.IssueNum),
		"-e", "ISSUE_TITLE=" + job.IssueTitle,
		"-e", "ISSUE_BODY=" + job.IssueBody,
		"-e", "LLM_API_KEY=" + envOrEmpty("LLM_API_KEY"),
		"-e", "OTTO_PI_SETTINGS=" + piSettings,
		"-e", "OTTO_PI_MODELS=" + piModels,
		"-e", fmt.Sprintf("OTTO_CONTINUE=%v", job.Continue),
		"-e", "OTTO_FEEDBACK=" + job.Feedback,
		"-e", "OTTO_BRANCH=" + job.Branch,
		"-e", "OTTO_BASE_URL=" + os.Getenv("OTTO_BASE_URL"),
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", mediaDir + ":/otto-media",
		"--network", "host",
		workerImage,
	}

	// Inject per-repo environment variables
	for k, v := range job.EnvVars {
		args = append(args, "-e", k+"="+v)
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

// monitor waits for the container to exit, then updates status.
func (m *Manager) monitor(name, containerID string) {
	// Wait for container to exit (docker wait returns container exit code as its own exit code)
	cmd := exec.Command("docker", "wait", containerID)
	waitOut, waitErr := cmd.Output()
	exitCode := strings.TrimSpace(string(waitOut))
	if waitErr != nil && exitCode == "" {
		log.Printf("worker: %s wait error: %v", name, waitErr)
		exitCode = "1"
	}
	log.Printf("worker: %s exited with code %s", name, exitCode)

	// Grab logs before removing
	logCmd := exec.Command("docker", "logs", "--tail", "200", containerID)
	logs, _ := logCmd.Output()

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

	log.Printf("worker: %s LOGS:\n%s", name, logs)

	// Clean up (no --rm, so we remove manually)
	m.clientRemove(containerID)
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
	containerName := sanitizeName("otto-" + strings.ReplaceAll(jobID, "/", "-"))
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
