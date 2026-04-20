package aiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"remittance-poc/internal/models"
)

const (
	claudeAPIURL  = "https://api.anthropic.com/v1/messages"
	claudeModel   = "claude-opus-4-5"
	claudeVersion = "2023-06-01"

	openAIAPIURL = "https://api.openai.com/v1/chat/completions"
	openAIModel  = "gpt-5.4-nano"

	// Cost per million tokens (approximate, used for reporting only).
	claudeInputCostPerM  = 3.0
	claudeOutputCostPerM = 15.0
	openAIInputCostPerM  = 0.20
	openAIOutputCostPerM = 1.25
)

// Provider selects which LLM backend to use.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderOpenAI Provider = "openai"
)

// Client makes LLM API calls to extract remittance data.
type Client struct {
	provider   Provider
	apiKey     string
	httpClient *http.Client
	DryRun     bool
}

// New creates a live Client for the given provider.
// It reads the appropriate API key from the environment.
func New(provider Provider) (*Client, error) {
	var key string
	switch provider {
	case ProviderClaude:
		key = os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY not set — copy .env.example to .env and add your key")
		}
	case ProviderOpenAI:
		key = os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not set — copy .env.example to .env and add your key")
		}
	default:
		return nil, fmt.Errorf("unknown provider %q — choose 'claude' or 'openai'", provider)
	}
	return &Client{
		provider:   provider,
		apiKey:     key,
		httpClient: &http.Client{},
	}, nil
}

// NewDryRun creates a Client that skips real API calls.
func NewDryRun(provider Provider) *Client {
	return &Client{provider: provider, DryRun: true}
}

// Extract sends the assembled context to the LLM and returns structured remittance data.
func (c *Client) Extract(context string) (*models.RemittanceData, models.TokenUsage, error) {
	if c.DryRun {
		fmt.Printf("  [DRY-RUN] Would call %s API\n", c.provider)
		return nil, models.TokenUsage{}, nil
	}

	switch c.provider {
	case ProviderClaude:
		return c.extractClaude(context)
	case ProviderOpenAI:
		return c.extractOpenAI(context)
	default:
		return nil, models.TokenUsage{}, fmt.Errorf("unknown provider: %s", c.provider)
	}
}

// CalculateCost returns the estimated USD cost for the given token usage.
func (c *Client) CalculateCost(usage models.TokenUsage) float64 {
	var inM, outM float64
	switch c.provider {
	case ProviderOpenAI:
		inM, outM = openAIInputCostPerM, openAIOutputCostPerM
	default:
		inM, outM = claudeInputCostPerM, claudeOutputCostPerM
	}
	return float64(usage.InputTokens)/1_000_000*inM +
		float64(usage.OutputTokens)/1_000_000*outM
}

// ModelName returns the model identifier used by this client.
func (c *Client) ModelName() string {
	switch c.provider {
	case ProviderOpenAI:
		return openAIModel
	default:
		return claudeModel
	}
}

// ── Claude ────────────────────────────────────────────────────────────────────

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) extractClaude(context string) (*models.RemittanceData, models.TokenUsage, error) {
	reqBody := claudeRequest{
		Model:     claudeModel,
		MaxTokens: 8192,
		System:    systemPrompt(),
		Messages: []claudeMessage{
			{Role: "user", Content: userPrompt(context)},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("marshalling Claude request: %w", err)
	}

	req, err := http.NewRequest("POST", claudeAPIURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("creating Claude request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", claudeVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("Claude API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("reading Claude response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, models.TokenUsage{}, fmt.Errorf("Claude API status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp claudeResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("parsing Claude response: %w", err)
	}
	if apiResp.Error != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("Claude API error: %s", apiResp.Error.Message)
	}

	usage := models.TokenUsage{
		InputTokens:  apiResp.Usage.InputTokens,
		OutputTokens: apiResp.Usage.OutputTokens,
	}

	var text string
	for _, block := range apiResp.Content {
		if block.Type == "text" {
			text = block.Text
			break
		}
	}

	data, err := parseJSONResponse(text)
	if err != nil {
		return nil, usage, fmt.Errorf("parsing Claude JSON output: %w\nRaw response:\n%s", err, text)
	}
	return data, usage, nil
}

// ── OpenAI ────────────────────────────────────────────────────────────────────

type openAIRequest struct {
	Model               string          `json:"model"`
	ResponseFormat      map[string]any  `json:"response_format"`
	Messages            []openAIMessage `json:"messages"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) extractOpenAI(context string) (*models.RemittanceData, models.TokenUsage, error) {
	reqBody := openAIRequest{
		Model:               openAIModel,
		ResponseFormat:      map[string]any{"type": "json_object"},
		MaxCompletionTokens: 8192,
		Messages: []openAIMessage{
			{Role: "system", Content: systemPrompt()},
			{Role: "user", Content: userPrompt(context)},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("marshalling OpenAI request: %w", err)
	}

	req, err := http.NewRequest("POST", openAIAPIURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("creating OpenAI request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("OpenAI API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("reading OpenAI response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, models.TokenUsage{}, fmt.Errorf("OpenAI API status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp openAIResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("parsing OpenAI response: %w", err)
	}
	if apiResp.Error != nil {
		return nil, models.TokenUsage{}, fmt.Errorf("OpenAI API error: %s", apiResp.Error.Message)
	}

	usage := models.TokenUsage{
		InputTokens:  apiResp.Usage.PromptTokens,
		OutputTokens: apiResp.Usage.CompletionTokens,
	}

	var text string
	if len(apiResp.Choices) > 0 {
		text = apiResp.Choices[0].Message.Content
	}

	data, err := parseJSONResponse(text)
	if err != nil {
		return nil, usage, fmt.Errorf("parsing OpenAI JSON output: %w\nRaw response:\n%s", err, text)
	}
	return data, usage, nil
}

// ── Prompts ───────────────────────────────────────────────────────────────────

func systemPrompt() string {
	return `You are a remittance data extraction assistant for an accounts receivable team.
Your job is to read email content and attached PDF remittance advices and extract structured payment information.

Return ONLY a single valid JSON object — no markdown fences, no explanation, no extra text.

The JSON must match this exact schema:
{
  "payer":                 string  (company or person name sending the payment),
  "payment_date":          string  (ISO 8601 date YYYY-MM-DD, or empty if not found),
  "total_amount":          number  (total payment amount as a float, 0 if not found),
  "currency":              string  (3-letter ISO currency code, default "USD" if not stated),
  "bank_reference_number": string  (wire/ACH/check reference number, or empty if not found),
  "invoices": [
    {
      "invoice_number": string  (invoice ID/number),
      "amount":         number  (amount for this invoice),
      "description":    string  (optional line item description)
    }
  ],
  "invoice_count": number  (total number of invoices identified),
  "notes":         string  (any additional details worth capturing, or empty)
}

Rules:
- Extract information only from what is provided — do not guess or hallucinate.
- If a field cannot be found, use an empty string or 0 as appropriate.
- invoice_count must equal the length of the invoices array.
- Amounts must be numeric (no currency symbols or commas).`
}

func userPrompt(context string) string {
	return "Extract all remittance payment data from the following email and attachment content:\n\n" + context
}

// ── JSON parsing ──────────────────────────────────────────────────────────────

func parseJSONResponse(raw string) (*models.RemittanceData, error) {
	text := strings.TrimSpace(raw)

	// Strip markdown code fences if the model added them anyway.
	if strings.HasPrefix(text, "```") {
		lines := strings.SplitN(text, "\n", 2)
		if len(lines) == 2 {
			text = lines[1]
		}
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}

	var data models.RemittanceData
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	// Reconcile invoice_count with actual slice length.
	data.InvoiceCount = len(data.Invoices)

	return &data, nil
}
