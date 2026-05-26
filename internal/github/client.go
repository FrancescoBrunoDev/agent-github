package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	apiBase    = "https://api.github.com"
	acceptHeader = "application/vnd.github+json"
)

// Client wraps GitHub REST API calls authenticated as the Agent GitHub App.
type Client struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	http           *http.Client
}

// NewClient creates a GitHub API client from a PEM private key file.
func NewClient(appID, installationID int64, privateKeyFile string) (*Client, error) {
	keyData, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading private key: %w", err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8
		key2, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("parsing private key (pkcs1: %v, pkcs8: %v)", err, err2)
		}
		var ok bool
		key, ok = key2.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
	}

	return &Client{
		appID:          appID,
		installationID: installationID,
		privateKey:     key,
		http:           &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// generateJWT creates a signed JWT for app authentication (valid 10 minutes).
func (c *Client) generateJWT() (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": c.appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(c.privateKey)
}

// installationToken gets an installation access token (valid 1 hour).
func (c *Client) installationToken(ctx context.Context) (string, error) {
	jwt, err := c.generateJWT()
	if err != nil {
		return "", fmt.Errorf("generating JWT: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBase, c.installationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", acceptHeader)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting installation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("installation token: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}

	return result.Token, nil
}

// do performs an authenticated API request.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	token, err := c.installationToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("Content-Type", "application/json")

	return c.http.Do(req)
}

// Issue represents a GitHub issue.
type Issue struct {
	Number    int
	Title     string
	Body      string
	HTMLURL   string
	RepoOwner string
	RepoName  string
}

// InstallationToken gets a fresh installation access token (valid 1 hour).
func (c *Client) InstallationToken(ctx context.Context) (string, error) {
	return c.installationToken(ctx)
}
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

// AddLabels adds labels to a GitHub issue.
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

// AssignIssue assigns a GitHub issue to a user by login.
func (c *Client) AssignIssue(ctx context.Context, repoOwner, repoName string, issueNum int, assignee string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/assignees", repoOwner, repoName, issueNum)
	payload := map[string]interface{}{"assignees": []string{assignee}}
	jsonBody, _ := json.Marshal(payload)

	resp, err := c.do(ctx, http.MethodPost, path, strings.NewReader(string(jsonBody)))
	if err != nil {
		return fmt.Errorf("assigning issue: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
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
