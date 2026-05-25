package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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

			// Serve project previews
			os.MkdirAll(cfg.PreviewDir, 0755)
			mux.Handle("/preview/", http.StripPrefix("/preview/", http.FileServer(http.Dir(cfg.PreviewDir))))

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
	payload, err := webhook.Parse(r, h.cfg.GitHub.WebhookSecret)
	if err != nil {
		log.Printf("webhook parse error: %v", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// PR closed → clean up preview
	if eventType == webhook.EventPullRequest && payload.Action == "closed" && payload.PullRequest != nil && payload.Repo != nil {
		repoName := payload.Repo.Name
		prNum := payload.PullRequest.Number
		previewDir := os.Getenv("OTTO_PREVIEW_DIR")
		if previewDir == "" {
			previewDir = os.TempDir() + "/otto-preview"
		}
		previewPath := filepath.Join(previewDir, repoName, fmt.Sprintf("%d", prNum))
		if err := os.RemoveAll(previewPath); err != nil {
			log.Printf("preview cleanup failed for %s/#%d: %v", repoName, prNum, err)
		} else {
			log.Printf("preview cleaned up: %s/#%d (%s)", repoName, prNum, previewPath)
		}
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
