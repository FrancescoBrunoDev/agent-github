package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

const (
	EventIssue             = "issues"
	EventIssueComment      = "issue_comment"
	EventPullRequest       = "pull_request"
	EventPullRequestReview = "pull_request_review_comment"

	// GitHub webhook headers
	SignatureHeaderGH = "X-Hub-Signature-256"
	EventHeaderGH     = "X-GitHub-Event"

	// Gitea webhook headers
	SignatureHeaderGitea = "X-Gitea-Signature"
	EventHeaderGitea     = "X-Gitea-Event"
)

// Platform indicates which forge the webhook came from.
type Platform string

const (
	PlatformGitHub Platform = "github"
	PlatformGitea  Platform = "gitea"
)

// Payload is a partial webhook payload — compatible with both GitHub and Gitea.
type Payload struct {
	Action      string           `json:"action"`
	Issue       *IssueData       `json:"issue,omitempty"`
	Comment     *CommentData     `json:"comment,omitempty"`
	Repo        *RepoData        `json:"repository,omitempty"`
	Assignee    *AssigneeData    `json:"assignee,omitempty"`
	PullRequest *PullRequestData `json:"pull_request,omitempty"`
}

type IssueData struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	URL         string    `json:"html_url"`
	PullRequest *struct{} `json:"pull_request,omitempty"` // present if this is a PR
}

type AssigneeData struct {
	Login string `json:"login"`
}

type PullRequestData struct {
	Number int  `json:"number"`
	Merged bool `json:"merged"`
}

func (i *IssueData) IsPullRequest() bool {
	return i != nil && i.PullRequest != nil
}

// IsAssignedTo returns true if the webhook payload is an assignment
// event targeting the given username.
func (p *Payload) IsAssignedTo(user string) bool {
	return p.Action == "assigned" && p.Assignee != nil &&
		p.Assignee.Login == user && p.Issue != nil
}

type CommentData struct {
	Body string `json:"body"`
}

type RepoData struct {
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Name string `json:"name"`
}

// IsMentioned returns whether @<user> appears in any of the given text fields.
func IsMentioned(user string, texts ...string) bool {
	re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(user) + `\b`)
	for _, t := range texts {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// Parse reads and validates a webhook request from either GitHub or Gitea.
// Returns the detected platform, the parsed payload, and any error.
func Parse(r *http.Request, secret string) (Platform, *Payload, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, fmt.Errorf("reading body: %w", err)
	}
	defer r.Body.Close()

	// Detect platform from event header
	eventHeader := r.Header.Get(EventHeaderGH)
	platform := PlatformGitHub
	if eventHeader == "" {
		eventHeader = r.Header.Get(EventHeaderGitea)
		if eventHeader != "" {
			platform = PlatformGitea
		}
	}

	if secret != "" {
		sigHeader := r.Header.Get(SignatureHeaderGH)
		if sigHeader == "" {
			sigHeader = r.Header.Get(SignatureHeaderGitea)
		}
		if err := verifySignature(sigHeader, body, secret); err != nil {
			return platform, nil, err
		}
	}

	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return platform, nil, fmt.Errorf("decoding payload: %w", err)
	}

	return platform, &p, nil
}

func verifySignature(sigHeader string, body []byte, secret string) error {
	if sigHeader == "" {
		return fmt.Errorf("missing signature header")
	}
	if len(sigHeader) < 7 || sigHeader[:7] != "sha256=" {
		return fmt.Errorf("invalid signature format")
	}
	expected, err := hex.DecodeString(sigHeader[7:])
	if err != nil {
		return fmt.Errorf("decoding signature: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	actual := mac.Sum(nil)

	if !hmac.Equal(expected, actual) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}
