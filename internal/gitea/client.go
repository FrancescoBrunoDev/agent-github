package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client wraps Gitea REST API calls authenticated via personal access token.
// Gitea's API is largely GitHub-compatible so the same endpoints and JSON
// shapes are used; only auth and base URL differ.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient creates a Gitea API client from a base URL and personal access token.
// baseURL should be the Gitea instance root, e.g. https://gitea.example.com.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// do performs an authenticated API request.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.http.Do(req)
}

// InstallationToken returns the PAT for Gitea (no JWT exchange needed).
func (c *Client) InstallationToken(ctx context.Context) (string, error) {
	return c.token, nil
}

// PostComment posts a comment on an issue or PR.
func (c *Client) PostComment(ctx context.Context, repoOwner, repoName string, issueNum int, body string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", repoOwner, repoName, issueNum)

	payload := map[string]string{"body": body}
	jsonBody, _ := json.Marshal(payload)

	resp, err := c.do(ctx, http.MethodPost, path, strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("posting comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post comment: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// CreatePR opens a pull request. Returns the PR URL.
func (c *Client) CreatePR(ctx context.Context, repoOwner, repoName, head, base, title, body string) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls", repoOwner, repoName)

	payload := map[string]string{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  body,
	}
	jsonBody, _ := json.Marshal(payload)

	resp, err := c.do(ctx, http.MethodPost, path, strings.NewReader(string(jsonBody)))
	if err != nil {
		return "", fmt.Errorf("creating PR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create PR: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding PR response: %w", err)
	}

	return result.HTMLURL, nil
}

// GetPRHead returns the head branch name of a pull request.
func (c *Client) GetPRHead(ctx context.Context, repoOwner, repoName string, prNumber int) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", repoOwner, repoName, prNumber)

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("getting PR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get PR: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding PR response: %w", err)
	}

	return result.Head.Ref, nil
}

// AddLabels adds labels to a Gitea issue.
func (c *Client) AddLabels(ctx context.Context, repoOwner, repoName string, issueNum int, labels []string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", repoOwner, repoName, issueNum)
	payload := map[string]interface{}{"labels": labels}
	jsonBody, _ := json.Marshal(payload)

	resp, err := c.do(ctx, http.MethodPost, path, strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("adding labels: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add labels: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// AssignIssue assigns a Gitea issue to a user by login.
// Uses PATCH /repos/{owner}/{repo}/issues/{index} which is the universal
// Gitea API (the /assignees sub-endpoint is only available in very recent versions).
func (c *Client) AssignIssue(ctx context.Context, repoOwner, repoName string, issueNum int, assignee string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", repoOwner, repoName, issueNum)
	payload := map[string]interface{}{"assignees": []string{assignee}}
	jsonBody, _ := json.Marshal(payload)

	resp, err := c.do(ctx, http.MethodPatch, path, strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("assigning issue: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("assign issue: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// EnsureLabel creates a label if it doesn't exist. Returns nil if label already exists.
func (c *Client) EnsureLabel(ctx context.Context, repoOwner, repoName, name, color, description string) error {
	path := fmt.Sprintf("/repos/%s/%s/labels", repoOwner, repoName)
	payload := map[string]interface{}{
		"name":        name,
		"color":       color,
		"description": description,
	}
	jsonBody, _ := json.Marshal(payload)

	resp, err := c.do(ctx, http.MethodPost, path, strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("creating label: %w", err)
	}
	defer resp.Body.Close()

	// 201 = created, 422 = already exists (ok)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusUnprocessableEntity {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create label: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ParseRepoURL extracts owner and repo name from a clone URL.
func ParseRepoURL(raw string) (owner, name string, _ error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parsing repo URL: %w", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("unexpected repo URL path: %s", u.Path)
	}
	name = parts[1]
	name = strings.TrimSuffix(name, ".git")
	return parts[0], name, nil
}
