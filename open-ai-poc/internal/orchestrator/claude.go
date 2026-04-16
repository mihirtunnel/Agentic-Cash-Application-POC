package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"cashapp-agent-poc/internal/ratelimit"
)

const (
	claudeAPIURL = "https://api.anthropic.com/v1/messages"
	claudeModel  = "claude-haiku-4-5"
)

type claudeRequest struct {
	Model       string                   `json:"model"`
	MaxTokens   int                      `json:"max_tokens"`
	Temperature float64                  `json:"temperature"`
	System      []map[string]interface{} `json:"system"`
	Tools       []interface{}            `json:"tools"`
	Messages    []claudeMessage          `json:"messages"`
}

type claudeMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type claudeResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text,omitempty"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

func (u *TokenUsage) Add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheCreationTokens += other.CacheCreationTokens
	u.CacheReadTokens += other.CacheReadTokens
}

func callClaudeRaw(agentName string, systemPrompt string, tools []map[string]interface{}, messages []claudeMessage, maxTokens int, round int, verbose bool) (*claudeResponse, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	system := []map[string]interface{}{
		{
			"type":          "text",
			"text":          systemPrompt,
			"cache_control": map[string]string{"type": "ephemeral"},
		},
	}

	toolsInterface := make([]interface{}, len(tools))
	for i, t := range tools {
		toolsInterface[i] = t
	}
	if len(tools) > 0 {
		last := make(map[string]interface{})
		for k, v := range tools[len(tools)-1] {
			last[k] = v
		}
		last["cache_control"] = map[string]string{"type": "ephemeral"}
		toolsInterface[len(toolsInterface)-1] = last
	}

	req := claudeRequest{
		Model:       claudeModel,
		MaxTokens:   maxTokens,
		Temperature: 0,
		System:      system,
		Tools:       toolsInterface,
		Messages:    messages,
	}

	if verbose {
		orchVerboseRequest(agentName, round, messages)
	}

	ratelimit.Wait()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", claudeAPIURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	client := &http.Client{Timeout: 60 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d: %s", httpResp.StatusCode, string(respBody))
	}

	var resp claudeResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("API error: %s — %s", resp.Error.Type, resp.Error.Message)
	}

	if verbose {
		orchVerboseResponse(agentName, round, &resp)
	}

	return &resp, nil
}

// runToolLoop executes a multi-round agent loop: sends messages to Claude, executes tool calls
// locally, appends results, and repeats until the model stops or the budget is reached.
func runToolLoop(
	agentName string,
	systemPrompt string,
	tools []map[string]interface{},
	initialMessage string,
	maxRounds int,
	toolExecutor func(name string, input json.RawMessage) (string, error),
	submitToolName string,
	verbose bool,
) (submitInput json.RawMessage, agentText string, usage TokenUsage, err error) {

	messages := []claudeMessage{
		{Role: "user", Content: initialMessage},
	}

	var allText []string

	for round := 1; round <= maxRounds; round++ {
		resp, callErr := callClaudeRaw(agentName, systemPrompt, tools, messages, 2048, round, verbose)
		if callErr != nil {
			err = fmt.Errorf("%s round %d: %w", agentName, round, callErr)
			return
		}

		usage.Add(TokenUsage{
			InputTokens:         resp.Usage.InputTokens,
			OutputTokens:        resp.Usage.OutputTokens,
			CacheCreationTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadTokens:     resp.Usage.CacheReadInputTokens,
		})

		var toolUseBlocks []struct {
			ID    string
			Name  string
			Input json.RawMessage
		}
		for _, block := range resp.Content {
			if block.Type == "text" && block.Text != "" {
				allText = append(allText, block.Text)
			}
			if block.Type == "tool_use" {
				toolUseBlocks = append(toolUseBlocks, struct {
					ID    string
					Name  string
					Input json.RawMessage
				}{block.ID, block.Name, block.Input})
			}
		}

		for _, tu := range toolUseBlocks {
			if tu.Name == submitToolName {
				submitInput = tu.Input
				agentText = strings.Join(allText, "\n")
				return
			}
		}

		if len(toolUseBlocks) == 0 {
			if resp.StopReason == "end_turn" {
				agentText = strings.Join(allText, "\n")
				return
			}
			break
		}

		assistantContent := make([]interface{}, 0, len(resp.Content))
		for _, block := range resp.Content {
			assistantContent = append(assistantContent, block)
		}
		messages = append(messages, claudeMessage{Role: "assistant", Content: assistantContent})

		toolResults := make([]interface{}, 0, len(toolUseBlocks))
		for _, tu := range toolUseBlocks {
			output, execErr := toolExecutor(tu.Name, tu.Input)
			if execErr != nil {
				output = fmt.Sprintf("Error: %s", execErr.Error())
			}
			toolResults = append(toolResults, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": tu.ID,
				"content":     output,
			})
		}
		messages = append(messages, claudeMessage{Role: "user", Content: toolResults})
	}

	agentText = strings.Join(allText, "\n")
	return
}

// --- Verbose logging (same box-drawing format as multiagent) ---

func orchVerboseRequest(agentName string, round int, messages []claudeMessage) {
	fmt.Printf("\n  ╔══════════════════════════════════════════════════════════════\n")
	fmt.Printf("  ║  ► [%s] API CALL #%d — SENDING  (%d message(s) in conversation)\n", agentName, round, len(messages))
	fmt.Printf("  ╠══════════════════════════════════════════════════════════════\n")
	for i, msg := range messages {
		fmt.Printf("  ║\n  ║  [msg %d]  role=%s\n", i+1, msg.Role)
		orchVerboseContent(msg.Content)
	}
	fmt.Printf("  ╚══════════════════════════════════════════════════════════════\n")
}

func orchVerboseResponse(agentName string, round int, resp *claudeResponse) {
	cacheInfo := ""
	if resp.Usage.CacheCreationInputTokens > 0 || resp.Usage.CacheReadInputTokens > 0 {
		cacheInfo = fmt.Sprintf("  cache_write=%d  cache_read=%d",
			resp.Usage.CacheCreationInputTokens, resp.Usage.CacheReadInputTokens)
	}
	fmt.Printf("\n  ╔══════════════════════════════════════════════════════════════\n")
	fmt.Printf("  ║  ◄ [%s] API CALL #%d — RECEIVED  tokens: %d in / %d out%s  stop_reason: %s\n",
		agentName, round, resp.Usage.InputTokens, resp.Usage.OutputTokens, cacheInfo, resp.StopReason)
	fmt.Printf("  ╠══════════════════════════════════════════════════════════════\n")
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			fmt.Printf("  ║  [text]\n")
			orchIndented("  ║    ", block.Text)
		case "tool_use":
			fmt.Printf("  ║  [tool_use]  name=%s\n", block.Name)
			fmt.Printf("  ║    input: %s\n", string(block.Input))
		}
	}
	fmt.Printf("  ╚══════════════════════════════════════════════════════════════\n\n")
}

func orchVerboseContent(content interface{}) {
	switch v := content.(type) {
	case string:
		orchIndented("  ║    ", v)
	case []interface{}:
		for _, item := range v {
			b, _ := json.Marshal(item)
			var m map[string]interface{}
			if err := json.Unmarshal(b, &m); err != nil {
				fmt.Printf("  ║    %s\n", truncate(string(b), 200))
				continue
			}
			blockType, _ := m["type"].(string)
			switch blockType {
			case "text":
				text, _ := m["text"].(string)
				fmt.Printf("  ║    [text]\n")
				orchIndented("  ║      ", text)
			case "tool_use":
				name, _ := m["name"].(string)
				inputB, _ := json.Marshal(m["input"])
				fmt.Printf("  ║    [tool_use]  name=%s\n", name)
				fmt.Printf("  ║      input: %s\n", string(inputB))
			case "tool_result":
				toolID, _ := m["tool_use_id"].(string)
				resultContent, _ := m["content"].(string)
				fmt.Printf("  ║    [tool_result]  for tool_use_id=%s\n", toolID)
				orchIndented("  ║      ", resultContent)
			default:
				fmt.Printf("  ║    %s\n", truncate(string(b), 200))
			}
		}
	default:
		b, _ := json.Marshal(content)
		fmt.Printf("  ║    %s\n", truncate(string(b), 200))
	}
}

func orchIndented(prefix, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		fmt.Printf("%s%s\n", prefix, line)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
