package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-github/agent/internal/classifier"
	"github.com/agent-github/agent/internal/config"
	"github.com/agent-github/agent/internal/gitea"
	"github.com/agent-github/agent/internal/github"
	"github.com/agent-github/agent/internal/preview"
	"github.com/agent-github/agent/internal/queue"
	"github.com/agent-github/agent/internal/webhook"
	"github.com/agent-github/agent/internal/worker"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "agent",
		Short: "Agent — GitHub issue solver powered by pi agent",
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

			previewMgr := preview.NewManager(cfg.PreviewDir, cfg.BaseURL)

			if cfg.Pi.Settings != nil {
				os.Setenv("AGENT_PI_SETTINGS", cfg.Pi.Settings.SettingsJSON())
			}
			if cfg.Pi.Models != "" {
				os.Setenv("AGENT_PI_MODELS", cfg.Pi.Models)
			if cfg.Pi.Auth != "" {
				os.Setenv("AGENT_PI_AUTH", cfg.Pi.Auth)
			}
			}
			os.Setenv("AGENT_BASE_URL", cfg.BaseURL)
			os.Setenv("AGENT_MEDIA_DIR", cfg.MediaDir)
			os.Setenv("AGENT_PREVIEW_DIR", cfg.PreviewDir)
			if cfg.GitHub.BotUser != "" {
				os.Setenv("AGENT_BOT_USER", cfg.GitHub.BotUser)
			}
			if cfg.GitHub.BotToken != "" {
				os.Setenv("AGENT_BOT_TOKEN", cfg.GitHub.BotToken)
			}
			if cfg.Gitea.BotUser != "" {
				os.Setenv("AGENT_GITEA_BOT_USER", cfg.Gitea.BotUser)
			}
			if cfg.Gitea.BotToken != "" {
				os.Setenv("AGENT_GITEA_BOT_TOKEN", cfg.Gitea.BotToken)
			} else if cfg.Gitea.Token != "" {
				os.Setenv("AGENT_GITEA_BOT_TOKEN", cfg.Gitea.Token)
			}
			if cfg.Gitea.BaseURL != "" {
				os.Setenv("AGENT_GITEA_BASE_URL", cfg.Gitea.BaseURL)
			}

			var ghClient *github.Client
			if cfg.GitHub.AppID != 0 && cfg.GitHub.InstallationID != 0 && cfg.GitHub.PrivateKeyFile != "" {
				ghClient, err = github.NewClient(cfg.GitHub.AppID, cfg.GitHub.InstallationID, cfg.GitHub.PrivateKeyFile)
				if err != nil {
					log.Printf("WARNING: GitHub client init failed: %v", err)
					ghClient = nil
				}
			}

			var giteaClient *gitea.Client
			if cfg.Gitea.BaseURL != "" && cfg.Gitea.Token != "" {
				giteaClient = gitea.NewClient(cfg.Gitea.BaseURL, cfg.Gitea.Token)
				log.Printf("Gitea client configured for %s", cfg.Gitea.BaseURL)
			}

			// Initialize classifier if enabled
		var clf *classifier.Classifier
		if cfg.Classifier.Enabled {
			classCfg := classifier.Config{
				Provider: cfg.Classifier.Provider,
				Model:    cfg.Classifier.Model,
				BaseURL:  cfg.Classifier.BaseURL,
				APIKey:   cfg.Classifier.APIKey,
			}
			// Fall back to pi settings if classifier fields are empty
			if classCfg.Provider == "" && cfg.Pi.Settings != nil {
				classCfg.Provider = cfg.Pi.Settings.DefaultProvider
			}
			if classCfg.Model == "" && cfg.Pi.Settings != nil {
				classCfg.Model = cfg.Pi.Settings.DefaultModel
			}
			if classCfg.BaseURL == "" && classCfg.Provider != "" {
				classCfg.BaseURL = classifier.DefaultBaseURLForProvider(classCfg.Provider)
			}
			if classCfg.APIKey == "" && cfg.Pi.Auth != "" {
				classCfg.APIKey = extractAPIKeyFromPiAuth(cfg.Pi.Auth, classCfg.Provider)
			}
			clf = classifier.New(classCfg)
			log.Printf("classifier: initialized (model=%s, base_url=%s, label=%s, auto_assign=%v)",
				classCfg.Model, classCfg.BaseURL, humanLabelOrDefault(cfg), cfg.Classifier.AutoAssign)
		}

		h := &handler{cfg: cfg, queue: q, ghClient: ghClient, giteaClient: giteaClient, previewMgr: previewMgr, classifier: clf, labelCache: make(map[string]bool)}

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

			// Preview management API
			mux.HandleFunc("/api/previews", h.handlePreviewsList)
			mux.HandleFunc("/api/preview/", h.handlePreviewAPI)

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

			log.Printf("Agent listening on %s (media: %s/media/)", cfg.ListenAddr, cfg.BaseURL)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}
}

type handler struct {
	cfg          *config.Config
	queue        *queue.Queue
	ghClient     *github.Client
	giteaClient  *gitea.Client
	previewMgr   *preview.Manager
	classifier   *classifier.Classifier
	labelCache   map[string]bool // set of "owner/repo" that already have the classification label
	labelCacheMu sync.Mutex
}

func (h *handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Detect platform: check Gitea headers first (Gitea also sends X-GitHub-Event).
	eventType := r.Header.Get(webhook.EventHeaderGitea)
	platform := webhook.PlatformGitea
	if eventType == "" {
		eventType = r.Header.Get(webhook.EventHeaderGH)
		platform = webhook.PlatformGitHub
	}
	log.Printf("webhook: received %s event (platform=%s)", eventType, platform)

	secret := h.webhookSecret(platform)
	parsedPlatform, payload, err := webhook.Parse(r, secret)
	if err != nil {
		log.Printf("webhook parse error: %v", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	// Use the platform detected inside Parse (more reliable)
	platform = parsedPlatform
	log.Printf("webhook: platform=%s", platform)

	// PR closed → clean up preview
	if eventType == webhook.EventPullRequest && payload.Action == "closed" && payload.PullRequest != nil && payload.Repo != nil {
		log.Printf("webhook: PR closed event for %s/#%d", payload.Repo.FullName, payload.PullRequest.Number)
		repoFull := payload.Repo.FullName
		prNum := payload.PullRequest.Number
		if err := h.previewMgr.Destroy(repoFull, prNum); err != nil {
			log.Printf("preview destroy error: %v", err)
		} else {
			log.Printf("preview cleaned up: %s/#%d", repoFull, prNum)
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

	// ---- Issue Classification (auto-triage for newly opened issues) ----
	// Runs for both GitHub and Gitea when a forge client is configured.
	if eventType == webhook.EventIssue && payload.Action == "opened" &&
		h.classifier != nil && payload.Repo != nil && payload.Issue != nil {

		// Determine the right forge client and bot user for this platform.
		var forgeClient classifierClient
		switch platform {
		case webhook.PlatformGitea:
			if h.giteaClient != nil {
				forgeClient = h.giteaClient
			}
		default:
			if h.ghClient != nil {
				forgeClient = h.ghClient
			}
		}
		if forgeClient == nil {
			goto skipClassify
		}

		botUser := os.Getenv("AGENT_BOT_USER")
		if platform == webhook.PlatformGitea {
			if gb := os.Getenv("AGENT_GITEA_BOT_USER"); gb != "" {
				botUser = gb
			}
		}

		// Don't classify if already assigned to the bot or mentioned
		alreadyTargeted := false
		if botUser != "" {
			if payload.IsAssignedTo(botUser) {
				alreadyTargeted = true
			}
			var texts []string
			if payload.Issue.Body != "" {
				texts = append(texts, payload.Issue.Body)
			}
			if webhook.IsMentioned(botUser, texts...) {
				alreadyTargeted = true
			}
		}

		if !alreadyTargeted {
			log.Printf("classifier: analyzing issue %s/#%d", payload.Repo.FullName, payload.Issue.Number)

			result, err := h.classifier.Classify(r.Context(), payload.Issue.Title, payload.Issue.Body)
			if err != nil {
				log.Printf("classifier: classification failed: %v (will skip)", err)
				// On classification failure, do nothing — fall through to normal behavior
			} else {
				owner, repo := splitRepoName(payload.Repo.FullName)

				if result.Classification == classifier.ClassificationSimple && h.cfg.Classifier.AutoAssign && botUser != "" {
					// Simple → auto-assign to bot user, which triggers the main agent
					log.Printf("classifier: issue %s/#%d classified as SIMPLE, assigning to %s",
						payload.Repo.FullName, payload.Issue.Number, botUser)
					if err := forgeClient.AssignIssue(r.Context(), owner, repo, payload.Issue.Number, botUser); err != nil {
						log.Printf("classifier: failed to assign issue: %v", err)
					}
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{"classified":"simple","assigned":"` + botUser + `"}`))
					return
				}

				if result.Classification == classifier.ClassificationComplex {
					label := humanLabelOrDefault(h.cfg)
					log.Printf("classifier: issue %s/#%d classified as COMPLEX, adding label %q",
						payload.Repo.FullName, payload.Issue.Number, label)

					// Ensure the label exists on the repo (cached)
					h.ensureLabelCachedForge(r.Context(), platform, owner, repo, label)

					if err := forgeClient.AddLabels(r.Context(), owner, repo, payload.Issue.Number, []string{label}); err != nil {
						log.Printf("classifier: failed to add label %q: %v", label, err)
					}

					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{"classified":"complex","label":"` + label + `"}`))
					return
				}

				// Default: fall through to normal behavior
				log.Printf("classifier: issue classified as %s, but no action configured", result.Classification)
			}
		}
	}
skipClassify:

	// Determine bot user based on platform
	botUser := os.Getenv("AGENT_BOT_USER")
	if platform == webhook.PlatformGitea {
		if gb := os.Getenv("AGENT_GITEA_BOT_USER"); gb != "" {
			botUser = gb
		}
	}
	assigned := botUser != "" && payload.IsAssignedTo(botUser)

	log.Printf("webhook: action=%s botUser=%q assigned=%v issue=%v repo=%v",
		payload.Action, botUser, assigned, payload.Issue != nil, payload.Repo != nil)

	if !assigned {
		mentionUser := botUser
		if mentionUser == "" {
			mentionUser = "agent"
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

	if repoCfg, ok := h.cfg.Repos[payload.Repo.FullName]; ok {
		job.EnvVars = repoCfg.Env
		if repoCfg.Nixpacks != nil {
			job.NixpacksInstallCmd = repoCfg.Nixpacks.InstallCmd
			job.NixpacksBuildCmd = repoCfg.Nixpacks.BuildCmd
			job.NixpacksStartCmd = repoCfg.Nixpacks.StartCmd
		}
		if repoCfg.Preview != nil {
			job.PreviewType = repoCfg.Preview.Type
			job.PreviewDockerfilePath = repoCfg.Preview.DockerfilePath
			job.PreviewComposeFile = repoCfg.Preview.DockerComposeFile
			job.PreviewComposeService = repoCfg.Preview.DockerComposeService
			job.PreviewPort = repoCfg.Preview.Port
			job.PreviewBuildArgs = repoCfg.Preview.BuildArgs
		}
	}

	if assigned {
		job.TriggerType = "assign"
	} else {
		job.TriggerType = "mention"
	}

	if eventType == webhook.EventIssueComment || eventType == webhook.EventPullRequestReview {
		if payload.Issue.IsPullRequest() {
			job.Continue = true
		job.ID = job.ID + "-feedback"
			if payload.Comment != nil {
				job.Feedback = payload.Comment.Body
			}
			owner, repo := splitRepoName(payload.Repo.FullName)
			var branch string
			var branchErr error
			if platform == webhook.PlatformGitea && h.giteaClient != nil {
				branch, branchErr = h.giteaClient.GetPRHead(context.Background(), owner, repo, payload.Issue.Number)
			} else if h.ghClient != nil {
				branch, branchErr = h.ghClient.GetPRHead(context.Background(), owner, repo, payload.Issue.Number)
			}
			if branchErr != nil {
				log.Printf("failed to get PR head branch: %v", branchErr)
			} else {
				job.Branch = branch
			}
			log.Printf("continuation mode for %s (branch: %s)", job.ID, job.Branch)
		}
	}

	// Generate token based on platform
	switch platform {
	case webhook.PlatformGitea:
		if h.giteaClient != nil {
			token, err := h.giteaClient.InstallationToken(context.Background())
			if err != nil {
				log.Printf("failed to get gitea token: %v", err)
				http.Error(w, "auth failed", http.StatusInternalServerError)
				return
			}
			job.GitHubToken = token
			job.Platform = "gitea"
		} else {
			log.Printf("gitea event but no gitea client configured")
			http.Error(w, "gitea not configured", http.StatusInternalServerError)
			return
		}
	default:
		if h.ghClient != nil {
			token, err := h.ghClient.InstallationToken(context.Background())
			if err != nil {
				log.Printf("failed to generate installation token: %v", err)
				http.Error(w, "auth failed", http.StatusInternalServerError)
				return
			}
			job.GitHubToken = token
			job.Platform = "github"
		}
	}

	if h.queue.IsActive(job.ID) {
		log.Printf("job %s already active, skipping", job.ID)
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

	log.Printf("enqueued %s (position %d)", job.ID, pos)

	w.WriteHeader(http.StatusAccepted)
	resp := map[string]interface{}{"accepted": true, "job": job.ID}
	if pos > 1 {
		resp["queue_position"] = pos
	}
	json.NewEncoder(w).Encode(resp)
}

// ---- Preview API handlers ----

func (h *handler) handlePreviewsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	previews, err := h.previewMgr.List()
	if err != nil {
		log.Printf("preview list error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, previews)
}

func (h *handler) handlePreviewAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/preview/")
	if path == "" || path == "/" {
		http.Error(w, "missing platform, owner, repo and pr", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(path, "/", 5)
	if len(parts) < 4 {
		http.Error(w, "path must be /api/preview/{platform}/{owner}/{repo}/{pr}[/{action}]", http.StatusBadRequest)
		return
	}
	platform, owner, repoName, prStr := parts[0], parts[1], parts[2], parts[3]
	repo := owner + "/" + repoName
	action := ""
	if len(parts) > 4 {
		action = parts[4]
	}

	pr, err := strconv.Atoi(prStr)
	if err != nil {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		h.handlePreviewStatus(w, r, repo, pr)
	case r.Method == http.MethodPost && action == "":
		h.handlePreviewCreate(w, r, platform, repo, pr)
	case r.Method == http.MethodDelete && action == "":
		h.handlePreviewDestroy(w, r, repo, pr)
	case r.Method == http.MethodPost && action == "stop":
		h.handlePreviewStop(w, r, repo, pr)
	case r.Method == http.MethodPost && action == "start":
		h.handlePreviewStart(w, r, repo, pr)
	case r.Method == http.MethodPost && action == "restart":
		h.handlePreviewRestart(w, r, repo, pr)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *handler) handlePreviewCreate(w http.ResponseWriter, r *http.Request, platform, repo string, pr int) {
	var req preview.CreateRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			req = preview.CreateRequest{}
		}
	}

	isGitea := platform == "gitea"

	if req.GitHubToken == "" {
		if isGitea && h.giteaClient != nil {
			token, err := h.giteaClient.InstallationToken(r.Context())
			if err == nil {
				req.GitHubToken = token
			}
		} else if h.ghClient != nil {
			token, err := h.ghClient.InstallationToken(r.Context())
			if err == nil {
				req.GitHubToken = token
			}
		}
	}

	if req.RepoURL == "" {
		if isGitea && h.cfg.Gitea.BaseURL != "" {
			req.RepoURL = strings.TrimRight(h.cfg.Gitea.BaseURL, "/") + "/" + repo + ".git"
		} else {
			req.RepoURL = "https://github.com/" + repo + ".git"
		}
	}

	applyRepoConfig(h.cfg, &req, repo)

	info, err := h.previewMgr.Create(req, repo, pr)
	if err != nil {
		log.Printf("preview create error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (h *handler) handlePreviewStatus(w http.ResponseWriter, r *http.Request, repo string, pr int) {
	info, err := h.previewMgr.Status(repo, pr)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *handler) handlePreviewDestroy(w http.ResponseWriter, r *http.Request, repo string, pr int) {
	if err := h.previewMgr.Destroy(repo, pr); err != nil {
		log.Printf("preview destroy error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
}

func (h *handler) handlePreviewStop(w http.ResponseWriter, r *http.Request, repo string, pr int) {
	if err := h.previewMgr.Stop(repo, pr); err != nil {
		log.Printf("preview stop error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (h *handler) handlePreviewStart(w http.ResponseWriter, r *http.Request, repo string, pr int) {
	info, err := h.previewMgr.Start(repo, pr)
	if err != nil {
		log.Printf("preview start error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *handler) handlePreviewRestart(w http.ResponseWriter, r *http.Request, repo string, pr int) {
	info, err := h.previewMgr.Restart(repo, pr)
	if err != nil {
		log.Printf("preview restart error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// ---- Path rewriting for preview proxy ----

// injectBaseTag rewrites HTML responses: inserts <base> tag and rewrites
// src="/..." and href="/..." to use the preview prefix.
func injectBaseTag(resp *http.Response, prefix string) {
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(body) == 0 {
		return
	}

	prefixSlash := prefix + "/"

	// FIRST: rewrite bare /_next/ paths used by Next.js runtime and JSON payloads.
	// These aren't caught by HTML attribute rewriting (they're in <script> code).
	body = bytes.ReplaceAll(body, []byte("\"/_next/"), []byte("\""+prefixSlash+"_next/"))
	body = bytes.ReplaceAll(body, []byte("'/_next/"), []byte("'"+prefixSlash+"_next/"))
	body = bytes.ReplaceAll(body, []byte("/_next/"), []byte(prefixSlash+"_next/"))

	// THEN rewrite remaining absolute paths in attributes (skip protocol-relative //)
	body = rewriteHTMLAttr(body, "src=\"", "/", prefixSlash)
	body = rewriteHTMLAttr(body, "src='", "/", prefixSlash)
	body = rewriteHTMLAttr(body, "href=\"", "/", prefixSlash)
	body = rewriteHTMLAttr(body, "href='", "/", prefixSlash)

	// THEN inject <base> tag for relative URLs (won't be caught by rewrite)
	baseTag := []byte("<base href=\"" + prefixSlash + "\">")
	if idx := bytes.Index(body, []byte("<head>")); idx != -1 {
		insertAt := idx + len("<head>")
		body = append(body[:insertAt], append(baseTag, body[insertAt:]...)...)
	} else if idx := bytes.Index(body, []byte("<body")); idx != -1 {
		body = append(body[:idx], append(baseTag, body[idx:]...)...)
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
}

// rewriteCSS rewrites url(/...) in CSS to use the preview prefix.
func rewriteCSS(resp *http.Response, prefix string) {
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(body) == 0 {
		return
	}

	prefixSlash := prefix + "/"

	// FIRST: rewrite /_next/ in CSS (e.g. @font-face src)
	body = bytes.ReplaceAll(body, []byte("/_next/"), []byte(prefixSlash+"_next/"))

	// THEN handle url("/..."), url('/...'), url(/...)

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
}

// rewriteHTMLAttr replaces attr="/path" with attr="/prefix/path"
// but skips protocol-relative URLs (attr="//).
func rewriteHTMLAttr(body []byte, attr, slash, prefix string) []byte {
	search := []byte(attr + slash)
	doubleSlash := []byte(attr + "//")
	replace := []byte(attr + prefix)

	var result []byte
	for len(body) > 0 {
		idx := bytes.Index(body, search)
		if idx == -1 {
			result = append(result, body...)
			break
		}
		// Skip protocol-relative (//...)
		if bytes.HasPrefix(body[idx:], doubleSlash) {
			result = append(result, body[:idx+len(doubleSlash)]...)
			body = body[idx+len(doubleSlash):]
			continue
		}
		result = append(result, body[:idx]...)
		result = append(result, replace...)
		body = body[idx+len(search):]
	}
	return result
}

// ---- Reconcile & preview proxy ----

func reconcilePreviews(previewDir string) {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "name=agent-preview-",
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

		trimmed := strings.TrimPrefix(name, "agent-preview-")
		lastDash := strings.LastIndex(trimmed, "-")
		if lastDash < 0 {
			continue
		}
		repo := trimmed[:lastDash]
		pr := trimmed[lastDash+1:]

		if strings.HasPrefix(status, "Exited") || strings.HasPrefix(status, "Created") {
			log.Printf("preview reconcile: starting %s", name)
			exec.Command("docker", "start", name).Run()
			time.Sleep(500 * time.Millisecond)
		}

		var port string
		if strings.Contains(ports, "->") {
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

func previewHandler(previewDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/preview/")
		parts := strings.SplitN(path, "/", 5)
		if len(parts) < 4 {
			http.NotFound(w, r)
			return
		}
		platform, owner, repo, pr := parts[0], parts[1], parts[2], parts[3]
		_ = platform // reserved for future routing
		safeRepo := strings.ReplaceAll(owner+"/"+repo, "/", "-")

		if len(parts) == 4 && !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}

		portFile := filepath.Join(previewDir, safeRepo+"-"+pr+".port")
		portBytes, err := os.ReadFile(portFile)
		if err == nil {
			port := strings.TrimSpace(string(portBytes))
			target, _ := url.Parse("http://localhost:" + port)
			proxy := httputil.NewSingleHostReverseProxy(target)
			prefix := "/preview/" + platform + "/" + owner + "/" + repo + "/" + pr
			proxy.ModifyResponse = func(resp *http.Response) error {
				if loc := resp.Header.Get("Location"); loc != "" && strings.HasPrefix(loc, "/") {
					resp.Header.Set("Location", prefix+loc)
				}
				ct := resp.Header.Get("Content-Type")
				switch {
				case strings.Contains(ct, "text/html"):
					injectBaseTag(resp, prefix)
				case strings.Contains(ct, "text/css"):
					rewriteCSS(resp, prefix)
				}
				return nil
			}
			r.URL.Path = "/" + strings.Join(parts[4:], "/")
			proxy.ServeHTTP(w, r)
			return
		}

		staticDir := filepath.Join(previewDir, safeRepo, pr)
		http.StripPrefix("/preview/"+platform+"/"+owner+"/"+repo+"/"+pr, http.FileServer(http.Dir(staticDir))).ServeHTTP(w, r)
	})
}

// ---- Helpers ----

func (h *handler) webhookSecret(platform webhook.Platform) string {
	if platform == webhook.PlatformGitea {
		return h.cfg.Gitea.WebhookSecret
	}
	return h.cfg.GitHub.WebhookSecret
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
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

func findFullRepoName(repos map[string]config.RepoConfig, shortName string) string {
	if _, ok := repos[shortName]; ok {
		return shortName
	}
	suffix := "/" + shortName
	for key := range repos {
		if strings.HasSuffix(key, suffix) {
			return key
		}
	}
	return ""
}

func applyRepoConfig(cfg *config.Config, req *preview.CreateRequest, repo string) {
	fullName := findFullRepoName(cfg.Repos, repo)
	if fullName == "" {
		return
	}
	repoCfg := cfg.Repos[fullName]
	if req.InstallCmd == "" && repoCfg.Nixpacks != nil {
		req.InstallCmd = repoCfg.Nixpacks.InstallCmd
	}
	if req.BuildCmd == "" && repoCfg.Nixpacks != nil {
		req.BuildCmd = repoCfg.Nixpacks.BuildCmd
	}
	if req.StartCmd == "" && repoCfg.Nixpacks != nil {
		req.StartCmd = repoCfg.Nixpacks.StartCmd
	}
	if req.Env == nil && len(repoCfg.Env) > 0 {
		req.Env = make(map[string]string)
		for k, v := range repoCfg.Env {
			req.Env[k] = v
		}
	}
	if repoCfg.Preview != nil {
		if req.PreviewType == "" {
			req.PreviewType = repoCfg.Preview.Type
		}
		if req.DockerfilePath == "" {
			req.DockerfilePath = repoCfg.Preview.DockerfilePath
		}
		if req.DockerComposeFile == "" {
			req.DockerComposeFile = repoCfg.Preview.DockerComposeFile
		}
		if req.DockerComposeService == "" {
			req.DockerComposeService = repoCfg.Preview.DockerComposeService
		}
		if req.Port == 0 {
			req.Port = repoCfg.Preview.Port
		}
		if req.BuildArgs == nil && len(repoCfg.Preview.BuildArgs) > 0 {
			req.BuildArgs = make(map[string]string)
			for k, v := range repoCfg.Preview.BuildArgs {
				req.BuildArgs[k] = v
			}
		}
	}
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
			fmt.Printf("  preview dir: %s\n", cfg.PreviewDir)
			fmt.Printf("  base URL:    %s\n", cfg.BaseURL)
			fmt.Printf("  workers:     %d\n", cfg.MaxWorkers)
			fmt.Printf("  app ID:      %d\n", cfg.GitHub.AppID)
			fmt.Printf("  install ID:  %d\n", cfg.GitHub.InstallationID)
			if cfg.Pi.Settings != nil {
				fmt.Printf("  pi provider: %s\n", cfg.Pi.Settings.DefaultProvider)
				fmt.Printf("  pi model:    %s\n", cfg.Pi.Settings.DefaultModel)
			}
			fmt.Printf("  classifier:  %v\n", cfg.Classifier.Enabled)
			if cfg.Classifier.Enabled {
				fmt.Printf("  classifier label: %s\n", humanLabelOrDefault(cfg))
				fmt.Printf("  classifier auto-assign: %v\n", cfg.Classifier.AutoAssign)
			}
			return nil
		},
	}
}

// ---- Classifier helpers ----

const defaultLabelColor = "e99695"

// classifierClient is the subset of forge API methods needed by the classifier.
// Both github.Client and gitea.Client satisfy this.
type classifierClient interface {
	AssignIssue(ctx context.Context, repoOwner, repoName string, issueNum int, assignee string) error
	AddLabels(ctx context.Context, repoOwner, repoName string, issueNum int, labels []string) error
	EnsureLabel(ctx context.Context, repoOwner, repoName, name, color, description string) error
}

// humanLabelOrDefault returns the configured human-assistance label, or the default.
func humanLabelOrDefault(cfg *config.Config) string {
	if cfg.Classifier.HumanAssistanceLabel != "" {
		return cfg.Classifier.HumanAssistanceLabel
	}
	return "needs-human-assistance"
}

// ensureLabelCachedForge calls EnsureLabel via the right forge client, once per repo.
// Subsequent calls for the same repo are no-ops.
func (h *handler) ensureLabelCachedForge(ctx context.Context, platform webhook.Platform, owner, repo, label string) {
	key := owner + "/" + repo
	h.labelCacheMu.Lock()
	if h.labelCache[key] {
		h.labelCacheMu.Unlock()
		return
	}
	h.labelCacheMu.Unlock()

	var err error
	switch platform {
	case webhook.PlatformGitea:
		if h.giteaClient != nil {
			err = h.giteaClient.EnsureLabel(ctx, owner, repo, label, defaultLabelColor, "Issue needs human assistance")
		}
	default:
		if h.ghClient != nil {
			err = h.ghClient.EnsureLabel(ctx, owner, repo, label, defaultLabelColor, "Issue needs human assistance")
		}
	}
	if err != nil {
		log.Printf("classifier: failed to ensure label %q: %v", label, err)
		return
	}

	h.labelCacheMu.Lock()
	h.labelCache[key] = true
	h.labelCacheMu.Unlock()
}

// extractAPIKeyFromPiAuth attempts to parse the Pi auth JSON to find an API key
// for the given provider. Pi auth is a JSON map keyed by provider name:
//
//	{"anthropic": {"type": "api_key", "key": "sk-ant-..."}}
func extractAPIKeyFromPiAuth(authJSON, provider string) string {
	if authJSON == "" || provider == "" {
		return ""
	}
	var authMap map[string]interface{}
	if err := json.Unmarshal([]byte(authJSON), &authMap); err != nil {
		return ""
	}
	providerAuth, ok := authMap[provider]
	if !ok {
		return ""
	}
	pa, ok := providerAuth.(map[string]interface{})
	if !ok {
		return ""
	}
	key, _ := pa["key"].(string)
	return key
}
