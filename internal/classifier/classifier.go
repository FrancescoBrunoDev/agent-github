package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// ClassificationSimple means the issue is well-defined and can be handled by the agent.
	ClassificationSimple = "simple"
	// ClassificationComplex means the issue needs human assistance.
	ClassificationComplex = "complex"

	defaultTimeout = 30 * time.Second
)

// Result holds the classification outcome for an issue.
type Result struct {
	Classification string // "simple" or "complex"
	Label          string // The label to apply
}

// Config holds the classification LLM configuration.
type Config struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

// Classifier analyzes GitHub issues to determine if they can be handled
// autonomously or need human intervention.
type Classifier struct {
	config Config
	client *http.Client
}

// New creates a new classifier with the given configuration.
func New(cfg Config) *Classifier {
	return &Classifier{
		config: cfg,
		client: &http.Client{Timeout: defaultTimeout},
	}
}

// Classify determines whether an issue is simple (agent can handle it)
// or complex (needs human assistance).
func (c *Classifier) Classify(ctx context.Context, title, body string) (Result, error) {
	prompt := buildClassificationPrompt(title, body)

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": c.config.Model,
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "You are a GitHub issue classifier. Your job is to determine whether an issue can be solved autonomously by an AI coding agent or needs human assistance.\n\nRespond with exactly one word: \"simple\" or \"complex\". No explanation, no punctuation, no markdown.",
			},
			{
				"role":    "user",
				"content": prompt,
			},
		},
		"temperature": 0.1,
		"max_tokens":  10,
	})
	if err != nil {
		return Result{}, fmt.Errorf("marshalling request: %w", err)
	}

	baseURL := c.config.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	url := baseURL + "/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return Result{}, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("calling LLM: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("LLM API: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return Result{}, fmt.Errorf("decoding response: %w", err)
	}

	if len(result.Choices) == 0 {
		return Result{}, fmt.Errorf("LLM returned no choices")
	}

	classification := strings.TrimSpace(strings.ToLower(result.Choices[0].Message.Content))

	switch {
	case strings.Contains(classification, ClassificationSimple):
		return Result{Classification: ClassificationSimple, Label: ""}, nil
	case strings.Contains(classification, ClassificationComplex):
		return Result{Classification: ClassificationComplex, Label: ""}, nil
	default:
		return Result{}, fmt.Errorf("unexpected classification: %q", classification)
	}
}

func buildClassificationPrompt(title, body string) string {
	var b strings.Builder
	b.WriteString("Classify this GitHub issue as either \"simple\" or \"complex\".\n\n")
	b.WriteString("A SIMPLE issue:\n")
	b.WriteString("- Has a clear, well-defined task\n")
	b.WriteString("- Can be solved with code changes in a single PR\n")
	b.WriteString("- Has enough context to start work immediately\n")
	b.WriteString("- Does not require architectural decisions or cross-team coordination\n")
	b.WriteString("- The problem and expected solution are obvious\n\n")
	b.WriteString("A COMPLEX issue:\n")
	b.WriteString("- Is vague, ambiguous, or lacks detail\n")
	b.WriteString("- Requires architectural decisions or significant refactoring\n")
	b.WriteString("- Needs coordination with other systems or teams\n")
	b.WriteString("- Involves multiple PRs or is a feature request needing design discussion\n")
	b.WriteString("- Requires human judgement about business logic or trade-offs\n\n")
	b.WriteString("Issue title: ")
	b.WriteString(title)
	b.WriteString("\n")
	if body != "" {
		b.WriteString("Issue body:\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString("\nRespond with exactly one word: \"simple\" or \"complex\"")

	return b.String()
}

// LabelForClassification returns the label to apply based on classification result.
func LabelForClassification(result Result, humanLabel string) string {
	if result.Classification == ClassificationComplex {
		if humanLabel == "" {
			return "needs-human-assistance"
		}
		return humanLabel
	}
	return ""
}
