package aiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"cashapp-poc/internal/models"
)

const apiURL = "https://api.anthropic.com/v1/messages"
const model = "claude-sonnet-4-20250514"
const apiVersion = "2023-06-01"

// Cost per million tokens (Claude Sonnet)
const inputCostPerMillion = 3.0
const outputCostPerMillion = 15.0

type Client struct {
	apiKey     string
	httpClient *http.Client
	DryRun     bool
}

func New() (*Client, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set. Copy .env.example to .env and add your key")
	}
	return &Client{
		apiKey:     key,
		httpClient: &http.Client{},
	}, nil
}

func NewDryRun() *Client {
	return &Client{DryRun: true}
}

type apiRequest struct {
	Model      string                   `json:"model"`
	MaxTokens  int                      `json:"max_tokens"`
	System     string                   `json:"system,omitempty"`
	Tools      []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice map[string]interface{}   `json:"tool_choice,omitempty"`
	Messages   []message                `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiResponse struct {
	Content []contentBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (c *Client) CallCustomerID(systemPrompt, userPrompt string, tool map[string]interface{}) (*models.CustomerIDResult, models.TokenUsage, error) {
	if c.DryRun {
		fmt.Println("  [DRY-RUN] Would call Claude API for customer identification")
		return nil, models.TokenUsage{}, nil
	}

	resp, err := c.call(systemPrompt, userPrompt, tool, "identify_customer", 512)
	if err != nil {
		return nil, models.TokenUsage{}, err
	}

	tokens := models.TokenUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}

	for _, block := range resp.Content {
		if block.Type == "tool_use" && block.Name == "identify_customer" {
			var result models.CustomerIDResult
			if err := json.Unmarshal(block.Input, &result); err != nil {
				return nil, tokens, fmt.Errorf("parsing tool result: %w", err)
			}
			return &result, tokens, nil
		}
	}

	return nil, tokens, fmt.Errorf("no tool_use block in response")
}

func (c *Client) CallInvoiceMatch(systemPrompt, userPrompt string, tool map[string]interface{}) (*models.InvoiceMatchResult, models.TokenUsage, error) {
	if c.DryRun {
		fmt.Println("  [DRY-RUN] Would call Claude API for invoice matching")
		return nil, models.TokenUsage{}, nil
	}

	resp, err := c.call(systemPrompt, userPrompt, tool, "submit_invoice_match", 1024)
	if err != nil {
		return nil, models.TokenUsage{}, err
	}

	tokens := models.TokenUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}

	for _, block := range resp.Content {
		if block.Type == "tool_use" && block.Name == "submit_invoice_match" {
			var result models.InvoiceMatchResult
			if err := json.Unmarshal(block.Input, &result); err != nil {
				return nil, tokens, fmt.Errorf("parsing tool result: %w", err)
			}
			return &result, tokens, nil
		}
	}

	return nil, tokens, fmt.Errorf("no tool_use block in response")
}

func (c *Client) call(systemPrompt, userPrompt string, tool map[string]interface{}, toolName string, maxTokens int) (*apiResponse, error) {
	reqBody := apiRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Tools:     []map[string]interface{}{tool},
		ToolChoice: map[string]interface{}{
			"type": "tool",
			"name": toolName,
		},
		Messages: []message{
			{Role: "user", Content: userPrompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", apiResp.Error.Message)
	}

	return &apiResp, nil
}

func CalculateCost(tokens models.TokenUsage) float64 {
	inputCost := float64(tokens.InputTokens) / 1_000_000 * inputCostPerMillion
	outputCost := float64(tokens.OutputTokens) / 1_000_000 * outputCostPerMillion
	return inputCost + outputCost
}
