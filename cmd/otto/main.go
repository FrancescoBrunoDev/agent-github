package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agent-github/otto/internal/config"
	"github.com/agent-github/otto/internal/github"
	"github.com/agent-github/otto/internal/queue"
	"github.com/agent-github/otto/internal/webhook"
	"github.com/agent-github/otto/internal/worker"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "otto",
		Short: "Otto — GitHub issue solver powered by pi agent",
	}

	root.AddCommand(serveCmd())
	root.AddCommand(queueCmd())
	root.AddCommand(configCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the webhook server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			wm, err := worker.NewManager(cfg.MaxWorkers)
			if err != nil {
				return fmt.Errorf("worker manager: %w", err)
			}
			q := queue.New(wm)

			if cfg.Pi.Settings != nil {
				os.Setenv("OTTO_PI_SETTINGS", cfg.Pi.Settings.SettingsJSON())
			}
			if cfg.Pi.Models != "" {
				os.Setenv("OTTO_PI_MODELS", cfg.Pi.Models)
			if cfg.Pi.Auth != "" {
				os.Setenv("OTTO_PI_AUTH", cfg.Pi.Auth)
			}
			}
			os.Setenv("OTTO_BASE_URL", cfg.BaseURL)
			os.Setenv("OTTO_MEDIA_DIR", cfg.MediaDir)
			os.Setenv("OTTO_PREVIEW_DIR", cfg.PreviewDir)
			if cfg.GitHub.BotUser != "" {
				os.Setenv("OTTO_BOT_USER", cfg.GitHub.BotUser)
			}
			if cfg.GitHub.BotToken != "" {
				os.Setenv("OTTO_BOT_TOKEN", cfg.GitHub.BotToken)
			}

			var ghClient *github.Client
			if cfg.GitHub.AppID != 0 && cfg.GitHub.InstallationID != 0 && cfg.GitHub.PrivateKeyFile != "" {
				ghClient, err = github.NewClient(cfg.GitHub.AppID, cfg.GitHub.InstallationID, cfg.GitHub.PrivateKeyFile)
				if err != nil {
					log.Printf("WARNING: GitHub client init failed: %v", err)
					ghClient = nil
				}
			}

			h := &handler{cfg: cfg, queue: q, ghClient: ghClient}

			mux := http.NewServeMux()
			mux.HandleFunc(cfg.WebhookPath, h.handleWebhook)
			mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			})

			// Serve uploaded media
			os.MkdirAll(cfg.MediaDir, 0755)
			mux.Handle("/media/", http.StripPrefix("/media/", http.FileServer(http.Dir(cfg.MediaDir))))

			// Serve project previews (containerized or static fallback)
			os.MkdirAll(cfg.PreviewDir, 0755)
			mux.Handle("/preview/", previewHandler(cfg.PreviewDir))

			// Reconcile existing preview containers on startup
			reconcilePreviews(cfg.PreviewDir)

			srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				log.Println("shutting down...")
				srv.Shutdown(context.Background())
			}()

			log.Printf("Otto listening on %s (media: %s/media/)", cfg.ListenAddr, cfg.BaseURL)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}
}

type handler struct {
	cfg      *config.Config
	queue    *queue.Queue
	ghClient *github.Client
}

func (h *handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	log.Printf("webhook: received %s event", eventType)
	payload, err := webhook.Parse(r, h.cfg.GitHub.WebhookSecret)
	if err != nil {
		log.Printf("webhook parse error: %v", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// PR closed → clean up preview
	if eventType == webhook.EventPullRequest && payload.Action == "closed" && payload.PullRequest != nil && payload.Repo != nil {
		log.Printf("webhook: PR closed event for %s/#%d", payload.Repo.FullName, payload.PullRequest.Number)
		repoName := payload.Repo.Name
		prNum := payload.PullRequest.Number
		previewDir := os.Getenv("OTTO_PREVIEW_DIR")
		if previewDir == "" {
			previewDir = os.TempDir() + "/otto-preview"
		}

		// Use Docker to clean up root-owned files
		dockerCleanup := func(paths ...string) {
			for _, p := range paths {
				exec.Command("docker", "run", "--rm",
					"-v", previewDir+":/preview",
					"alpine", "rm", "-rf", "/preview/"+p,
				).Run()
			}
		}

		// Stop and remove preview container
		containerName := fmt.Sprintf("otto-preview-%s-%d", strings.ToLower(repoName), prNum)
		if out, err := exec.Command("docker", "rm", "-f", containerName).CombinedOutput(); err != nil {
			log.Printf("preview container cleanup failed for %s: %v\n%s", containerName, err, string(out))
		} else {
			log.Printf("preview container removed: %s", containerName)
		}

		// Remove port file + static files (all root-owned)
		prStr := fmt.Sprintf("%d", prNum)
		dockerCleanup(repoName+"-"+prStr+".port", repoName+"/"+prStr)
		log.Printf("preview cleaned up: %s/#%d", repoName, prNum)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"cleaned":"preview removed"}`))
		return
	}

	if eventType != webhook.EventIssue &&
		eventType != webhook.EventIssueComment &&
		eventType != webhook.EventPullRequestReview {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"skipped":"not a relevant event"}`))
		return
	}

	if !isActionRelevant(payload.Action) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"skipped":"action not relevant"}`))
		return
	}

	// Check if this is an assignment to our bot user (primary trigger)
	botUser := os.Getenv("OTTO_BOT_USER")
	assigned := botUser != "" && payload.IsAssignedTo(botUser)

	if !assigned {
		// Fallback: check for @mention of our bot user
		mentionUser := botUser
		if mentionUser == "" {
			mentionUser = "otto"
		}
		var texts []string
		if payload.Issue != nil && payload.Issue.Body != "" {
			texts = append(texts, payload.Issue.Body)
		}
		if payload.Comment != nil && payload.Comment.Body != "" {
			texts = append(texts, payload.Comment.Body)
		}

		if !webhook.IsMentioned(mentionUser, texts...) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"skipped":"@` + mentionUser + ` not mentioned"}`))
			return
		}
	}

	if payload.Repo == nil || payload.Issue == nil {
		http.Error(w, "incomplete payload", http.StatusBadRequest)
		return
	}

	job := worker.Job{
		ID:         fmt.Sprintf("%s#%d", payload.Repo.FullName, payload.Issue.Number),
		RepoURL:    payload.Repo.CloneURL,
		RepoName:   payload.Repo.FullName,
		IssueNum:   payload.Issue.Number,
		IssueTitle: payload.Issue.Title,
		IssueBody:  payload.Issue.Body,
	}

	// Inject per-repo environment variables from config
	if repoCfg, ok := h.cfg.Repos[payload.Repo.FullName]; ok {
		job.EnvVars = repoCfg.Env
	}

	// Set trigger type for the worker
	if assigned {
		os.Setenv("OTTO_TRIGGER_TYPE", "assign")
	} else {
		os.Setenv("OTTO_TRIGGER_TYPE", "mention")
	}

	if eventType == webhook.EventIssueComment || eventType == webhook.EventPullRequestReview {
		if payload.Issue.IsPullRequest() {
			job.Continue = true
			if payload.Comment != nil {
				job.Feedback = payload.Comment.Body
			}
			if h.ghClient != nil {
				owner, repo := splitRepoName(payload.Repo.FullName)
				branch, err := h.ghClient.GetPRHead(context.Background(), owner, repo, payload.Issue.Number)
				if err != nil {
					log.Printf("failed to get PR head branch: %v", err)
				} else {
					job.Branch = branch
				}
			}
			log.Printf("continuation mode for %s (branch: %s)", job.ID, job.Branch)
		}
	}

	if h.ghClient != nil {
		token, err := h.ghClient.InstallationToken(context.Background())
		if err != nil {
			log.Printf("failed to generate installation token: %v", err)
			http.Error(w, "auth failed", http.StatusInternalServerError)
			return
		}
		job.GitHubToken = token
	}

	if h.queue.IsActive(job.ID) {
		log.Printf("job %s already active", job.ID)
		if h.ghClient != nil && job.Feedback != "" {
			owner, repo := splitRepoName(payload.Repo.FullName)
			go h.ghClient.PostComment(context.Background(), owner, repo, payload.Issue.Number,
				"👋 I'm already working on this! I'll process your feedback after the current run finishes.")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"skipped":"already working on this"}`))
		return
	}

	pos := h.queue.Len() + 1
	if err := h.queue.Enqueue(job); err != nil {
		log.Printf("enqueue error: %v", err)
		http.Error(w, "failed to enqueue", http.StatusInternalServerError)
		return
	}

	if h.ghClient != nil && pos > 1 {
		owner, repo := splitRepoName(payload.Repo.FullName)
		go h.ghClient.PostComment(context.Background(), owner, repo, payload.Issue.Number,
			fmt.Sprintf("👋 I'm on it! You're #%d in the queue.", pos))
	}

	w.WriteHeader(http.StatusAccepted)
	resp := map[string]interface{}{"accepted": true, "job": job.ID}
	if pos > 1 {
		resp["queue_position"] = pos
	}
	json.NewEncoder(w).Encode(resp)
}



// reconcilePreviews ensures all existing otto-preview-* containers are running
// and their port mappings are written to {previewDir}/{repo}-{pr}.port.
func reconcilePreviews(previewDir string) {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "name=otto-preview-",
		"--format", "{{.Names}}\t{{.Status}}\t{{.Ports}}").Output()
	if err != nil {
		log.Printf("preview reconcile: docker ps failed: %v", err)
		return
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		name, status, ports := parts[0], parts[1], ""
		if len(parts) > 2 {
			ports = parts[2]
		}

		// Extract repo and PR from container name: otto-preview-{repo}-{pr}
		// Name format: otto-preview-test-otto-23
		trimmed := strings.TrimPrefix(name, "otto-preview-")
		lastDash := strings.LastIndex(trimmed, "-")
		if lastDash < 0 {
			continue
		}
		repo := trimmed[:lastDash]
		pr := trimmed[lastDash+1:]

		// Start container if stopped
		if strings.HasPrefix(status, "Exited") || strings.HasPrefix(status, "Created") {
			log.Printf("preview reconcile: starting %s", name)
			exec.Command("docker", "start", name).Run()
			// Refresh status after start
			time.Sleep(500 * time.Millisecond)
		}

		// Get port mapping
		var port string
		if strings.Contains(ports, "->") {
			// Parse "0.0.0.0:32768->3000/tcp"
			for _, mapping := range strings.Split(ports, ", ") {
				parts := strings.SplitN(mapping, "->", 2)
				if len(parts) == 2 {
					hostPart := strings.Split(parts[0], ":")
					if len(hostPart) == 2 {
						port = hostPart[1]
						break
					}
				}
			}
		} else {
			// Try docker port command
			for _, containerPort := range []string{"3000", "8080", "80"} {
				portOut, _ := exec.Command("docker", "port", name, containerPort).Output()
				if len(portOut) > 0 {
					parts := strings.SplitN(strings.TrimSpace(string(portOut)), ":", 2)
					if len(parts) == 2 {
						port = parts[1]
						break
					}
				}
			}
		}

		if port != "" {
			portFile := filepath.Join(previewDir, repo+"-"+pr+".port")
			os.WriteFile(portFile, []byte(port), 0644)
			log.Printf("preview reconcile: %s → port %s (%s)", name, port, portFile)
		}
	}
}

// previewHandler serves containerized previews via reverse proxy,
// falling back to static files if no container is running.
func previewHandler(previewDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse /preview/{repo}/{pr}/...
		path := strings.TrimPrefix(r.URL.Path, "/preview/")
		parts := strings.SplitN(path, "/", 3)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		repo, pr := parts[0], parts[1]

		// Redirect /preview/{repo}/{pr} → /preview/{repo}/{pr}/ (relative links fix)
		if len(parts) == 2 && !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}

		// Check for containerized preview
		portFile := filepath.Join(previewDir, repo+"-"+pr+".port")
		portBytes, err := os.ReadFile(portFile)
		if err == nil {
			port := strings.TrimSpace(string(portBytes))
			target, _ := url.Parse("http://localhost:" + port)
			proxy := httputil.NewSingleHostReverseProxy(target)
			prefix := "/preview/" + repo + "/" + pr
			proxy.ModifyResponse = func(resp *http.Response) error {
				if loc := resp.Header.Get("Location"); loc != "" && strings.HasPrefix(loc, "/") {
					resp.Header.Set("Location", prefix+loc)
				}
				return nil
			}
			r.URL.Path = "/" + strings.Join(parts[2:], "/")
			proxy.ServeHTTP(w, r)
			return
		}

		// Fallback: static files
		staticDir := filepath.Join(previewDir, repo, pr)
		http.StripPrefix("/preview/"+repo+"/"+pr, http.FileServer(http.Dir(staticDir))).ServeHTTP(w, r)
	})
}

func isActionRelevant(action string) bool {
	switch action {
	case "opened", "edited", "created", "assigned":
		return true
	}
	return false
}

func splitRepoName(fullName string) (owner, name string) {
	parts := splitBySlash(fullName)
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

func splitBySlash(s string) []string {
	var parts []string
	curr := ""
	for _, r := range s {
		if r == '/' {
			parts = append(parts, curr)
			curr = ""
		} else {
			curr += string(r)
		}
	}
	parts = append(parts, curr)
	return parts
}

func queueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "queue",
		Short: "Manage the job queue",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("queue management not yet implemented")
			return nil
		},
	}
}

func configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show or validate configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}
			fmt.Printf("  listen:      %s\n", cfg.ListenAddr)
			fmt.Printf("  webhook:     %s\n", cfg.WebhookPath)
			fmt.Printf("  media dir:   %s\n", cfg.MediaDir)
			fmt.Printf("  base URL:    %s\n", cfg.BaseURL)
			fmt.Printf("  workers:     %d\n", cfg.MaxWorkers)
			fmt.Printf("  app ID:      %d\n", cfg.GitHub.AppID)
			fmt.Printf("  install ID:  %d\n", cfg.GitHub.InstallationID)
			if cfg.Pi.Settings != nil {
				fmt.Printf("  pi provider: %s\n", cfg.Pi.Settings.DefaultProvider)
				fmt.Printf("  pi model:    %s\n", cfg.Pi.Settings.DefaultModel)
			}
			return nil
		},
	}
}
