package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/ratelimit"
	"cashapp-agent-poc/internal/tools"
)

const (
	claudeAPIURL = "https://api.anthropic.com/v1/messages"
	claudeModel  = "claude-haiku-4-5"
	maxToolCalls = 25
)

var systemPrompt = `You are a treasury cash application specialist at Tauber Oil Company. Your job is to first identify whether each bank payment is an INTERNAL TRANSFER or a CUSTOMER PAYMENT, then match customer payments to the correct customer and invoices.

CRITICAL: INTERNAL TRANSFER DETECTION FIRST
Before any customer matching, check if this is an internal Tauber Oil transfer:

INTERNAL TRANSFER INDICATORS:
- Bank reference contains both SENDER and BENE (beneficiary) that are Tauber entities (Tauber Oil, Tauber Petrochemical, etc.)
- Example: "SENDER=TAUBER OIL COMPANY / BENE=TAUBER PETROCHEMICAL CO"
- ZBA or Zero Balance Account transfers between Tauber subsidiaries
- Bank memo contains "TRANSFER FROM CHECKING ACCT" or similar ZBA routing pattern — no customer counterparty, internal movement only
- Scenario field contains "INTERNAL"
- No external customer counterparty — only internal movement

If you identify this as an INTERNAL TRANSFER:
- Set status to "internal_transfer"
- Set confidence to 1.0
- Do NOT perform customer matching
- Submit result immediately with submit_match_result
- Reasoning: "This is an internal Tauber Oil entity transfer"

CUSTOMER PAYMENT MATCHING (only if NOT internal transfer):
For customer payments, investigate systematically:

INVESTIGATION STRATEGY (follow this order):
1. ANALYZE the bank text — does it contain a company name, account number, invoice reference, or is it just a wire network code?
2. If the bank reference or memo contains what looks like an invoice number or account code, use lookup_reference first — this is the fastest path to a match.
3. Search by AMOUNT — use search_customers_by_amount. This is often the strongest signal. If only one customer's invoices sum to the payment amount, that's very likely the payer.
4. Search by NAME — if the bank text contains what looks like a company name, use search_customers_by_name.
5. Search REMITTANCE EMAILS — use search_remittance_emails with the payment amount and date range. Customers often email remittance advice around the time of payment.
6. For each candidate customer, get their invoices with get_customer_invoices and check if invoice amounts align with the payment.
7. VERIFY your top candidate with get_customer_payment_history — does this customer typically pay this way?
8. Once you have enough evidence, submit your result with submit_match_result.

RULES FOR CUSTOMER MATCHING:
- You have a maximum of 10 tool calls. Be efficient — don't repeat searches.
- Confidence scoring for customer matches:
  - 0.95+: Multiple confirming signals (amount match + name match + remittance email, or amount match + payment history pattern)
  - 0.85-0.94: Strong single signal confirmed by one other (e.g., amount match + name match)
  - 0.70-0.84: Reasonable match but only one signal (e.g., only amount match, no name confirmation)
  - Below 0.70: Weak evidence — submit as unresolved unless you have a specific reason
- Account for early-pay discounts (typically 1-2% of invoice amount) and short-payments
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If the bank counterparty/wire name differs from the matched customer's registered name, optionally set suggested_alias to that exact bank text (audit trail only; there is no separate alias directory to query)
- Do NOT guess. If the evidence is insufficient, submit status "unresolved" with a clear summary of what you tried.
- You MUST call submit_match_result to end your investigation.`

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

type RunResult struct {
	Match                    *models.AgentMatchResult
	ToolCallCount            int
	TotalInputTokens         int
	TotalOutputTokens        int
	TotalCacheCreationTokens int
	TotalCacheReadTokens     int
	Rounds                   []RoundLog
}

type ToolCallLog struct {
	Num    int
	Name   string
	Input  string
	Output string
}

type RoundLog struct {
	Round               int
	AgentText           string
	ToolCalls           []ToolCallLog
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

func RunAgent(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*RunResult, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	prompt := buildTransactionPrompt(txn)
	messages := []claudeMessage{
		{Role: "user", Content: prompt},
	}

	result := &RunResult{}
	toolCallCount := 0

	for round := 1; toolCallCount < maxToolCalls; round++ {
		resp, err := callClaude(apiKey, messages, round, verbose)
		if err != nil {
			// Return whatever we accumulated so far so the caller can still report partial progress.
			result.ToolCallCount = toolCallCount
			if result.Match == nil {
				result.Match = &models.AgentMatchResult{
					Status:               "unresolved",
					Reasoning:            fmt.Sprintf("API error on round %d: %v", round, err),
					InvestigationSummary: "Run aborted by API error.",
				}
			}
			return result, fmt.Errorf("round %d: %w", round, err)
		}

		result.TotalInputTokens += resp.Usage.InputTokens
		result.TotalOutputTokens += resp.Usage.OutputTokens
		result.TotalCacheCreationTokens += resp.Usage.CacheCreationInputTokens
		result.TotalCacheReadTokens += resp.Usage.CacheReadInputTokens

		roundLog := RoundLog{
			Round:               round,
			InputTokens:         resp.Usage.InputTokens,
			OutputTokens:        resp.Usage.OutputTokens,
			CacheCreationTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadTokens:     resp.Usage.CacheReadInputTokens,
		}

		// Collect agent text and tool_use blocks from this response.
		var toolUseBlocks []struct {
			ID    string
			Name  string
			Input json.RawMessage
		}
		for _, block := range resp.Content {
			if block.Type == "text" && block.Text != "" {
				roundLog.AgentText = block.Text
			}
			if block.Type == "tool_use" {
				toolUseBlocks = append(toolUseBlocks, struct {
					ID    string
					Name  string
					Input json.RawMessage
				}{block.ID, block.Name, block.Input})
			}
		}

		// Check for submit_match_result before executing other tools.
		for _, tu := range toolUseBlocks {
			if tu.Name == "submit_match_result" {
				matchResult, err := parseMatchResult(tu.Input)
				if err != nil {
					return nil, fmt.Errorf("parsing submit_match_result: %w", err)
				}
				toolCallCount++
				roundLog.ToolCalls = append(roundLog.ToolCalls, ToolCallLog{
					Num:   toolCallCount,
					Name:  "submit_match_result",
					Input: string(tu.Input),
				})
				result.Match = matchResult
				result.ToolCallCount = toolCallCount
				result.Rounds = append(result.Rounds, roundLog)
				printRound(roundLog)
				return result, nil
			}
		}

		if len(toolUseBlocks) == 0 {
			if resp.StopReason == "end_turn" {
				messages = append(messages, claudeMessage{Role: "assistant", Content: resp.Content})
				messages = append(messages, claudeMessage{
					Role:    "user",
					Content: "Please submit your result using the submit_match_result tool.",
				})
				result.Rounds = append(result.Rounds, roundLog)
				printRound(roundLog)
				continue
			}
			break
		}

		// Execute tool calls and collect results.
		assistantContent := make([]interface{}, 0, len(resp.Content))
		for _, block := range resp.Content {
			assistantContent = append(assistantContent, block)
		}
		messages = append(messages, claudeMessage{Role: "assistant", Content: assistantContent})

		toolResults := make([]interface{}, 0, len(toolUseBlocks))
		for _, tu := range toolUseBlocks {
			toolCallCount++
			output, execErr := toolHandler.Execute(tu.Name, tu.Input)
			if execErr != nil {
				output = fmt.Sprintf("Error: %s", execErr.Error())
			}
			roundLog.ToolCalls = append(roundLog.ToolCalls, ToolCallLog{
				Num:    toolCallCount,
				Name:   tu.Name,
				Input:  string(tu.Input),
				Output: output,
			})
			toolResults = append(toolResults, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": tu.ID,
				"content":     output,
			})
		}
		messages = append(messages, claudeMessage{Role: "user", Content: toolResults})

		result.Rounds = append(result.Rounds, roundLog)
		printRound(roundLog)
	}

	result.ToolCallCount = toolCallCount
	if result.Match == nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Confidence:           0,
			Reasoning:            "Agent reached maximum tool call limit without submitting a result.",
			InvestigationSummary: "Agent exhausted tool call budget.",
		}
	}
	return result, nil
}

// printRound writes a structured summary of one agent→tool round to stdout immediately,
// so output is visible even if a later round fails.
func printRound(r RoundLog) {
	cacheInfo := ""
	if r.CacheCreationTokens > 0 || r.CacheReadTokens > 0 {
		cacheInfo = fmt.Sprintf(" | cache_write: %d, cache_read: %d", r.CacheCreationTokens, r.CacheReadTokens)
	}
	fmt.Printf("  ┌─ Round %d ── tokens: %d in / %d out%s\n", r.Round, r.InputTokens, r.OutputTokens, cacheInfo)

	if r.AgentText != "" {
		fmt.Printf("  │ Agent: %s\n", truncate(r.AgentText, 200))
	}

	for _, tc := range r.ToolCalls {
		fmt.Printf("  │ [Tool %d] %s\n", tc.Num, tc.Name)
		if tc.Input != "" && tc.Name != "submit_match_result" {
			fmt.Printf("  │   Input:  %s\n", truncate(tc.Input, 150))
		}
		if tc.Output != "" {
			fmt.Printf("  │   Result: %s\n", truncate(tc.Output, 200))
		}
	}
	fmt.Printf("  └──────────────────────────────────────────────────\n")
}

// printVerboseRequest shows the full conversation being sent to Claude for this API call.
func printVerboseRequest(round int, messages []claudeMessage) {
	fmt.Printf("\n  ╔══════════════════════════════════════════════════════════════\n")
	fmt.Printf("  ║  ► API CALL #%d — SENDING  (%d message(s) in conversation)\n", round, len(messages))
	fmt.Printf("  ╠══════════════════════════════════════════════════════════════\n")
	for i, msg := range messages {
		fmt.Printf("  ║\n  ║  [msg %d]  role=%s\n", i+1, msg.Role)
		printVerboseContent(msg.Content)
	}
	fmt.Printf("  ╚══════════════════════════════════════════════════════════════\n")
}

// printVerboseResponse shows exactly what Claude returned for this API call.
func printVerboseResponse(round int, resp *claudeResponse) {
	cacheInfo := ""
	if resp.Usage.CacheCreationInputTokens > 0 || resp.Usage.CacheReadInputTokens > 0 {
		cacheInfo = fmt.Sprintf("  cache_write=%d  cache_read=%d",
			resp.Usage.CacheCreationInputTokens, resp.Usage.CacheReadInputTokens)
	}
	fmt.Printf("\n  ╔══════════════════════════════════════════════════════════════\n")
	fmt.Printf("  ║  ◄ API CALL #%d — RECEIVED  tokens: %d in / %d out%s  stop_reason: %s\n",
		round, resp.Usage.InputTokens, resp.Usage.OutputTokens, cacheInfo, resp.StopReason)
	fmt.Printf("  ╠══════════════════════════════════════════════════════════════\n")
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			fmt.Printf("  ║  [text]\n")
			printIndented("  ║    ", block.Text)
		case "tool_use":
			fmt.Printf("  ║  [tool_use]  name=%s\n", block.Name)
			fmt.Printf("  ║    input: %s\n", string(block.Input))
		}
	}
	fmt.Printf("  ╚══════════════════════════════════════════════════════════════\n\n")
}

// printVerboseContent formats a message's content field for verbose output.
// Content can be a plain string (initial user prompt) or an array of content blocks
// (assistant responses with tool_use blocks, or user messages with tool_result blocks).
func printVerboseContent(content interface{}) {
	switch v := content.(type) {
	case string:
		printIndented("  ║    ", v)
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
				printIndented("  ║      ", text)
			case "tool_use":
				name, _ := m["name"].(string)
				inputB, _ := json.Marshal(m["input"])
				fmt.Printf("  ║    [tool_use]  name=%s\n", name)
				fmt.Printf("  ║      input: %s\n", string(inputB))
			case "tool_result":
				toolID, _ := m["tool_use_id"].(string)
				resultContent, _ := m["content"].(string)
				fmt.Printf("  ║    [tool_result]  for tool_use_id=%s\n", toolID)
				printIndented("  ║      ", resultContent)
			default:
				fmt.Printf("  ║    %s\n", truncate(string(b), 200))
			}
		}
	default:
		b, _ := json.Marshal(content)
		fmt.Printf("  ║    %s\n", truncate(string(b), 200))
	}
}

// printIndented prints multi-line text with a consistent prefix on each line.
func printIndented(prefix, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		fmt.Printf("%s%s\n", prefix, line)
	}
}

func buildTransactionPrompt(txn models.BankTransaction) string {
	var sb fmt.Stringer = &bytes.Buffer{}
	buf := sb.(*bytes.Buffer)

	fmt.Fprintf(buf, "MATCH THIS PAYMENT:\n\n")
	fmt.Fprintf(buf, "  Transaction ID: %s\n", txn.TxnID)
	fmt.Fprintf(buf, "  Date: %s\n", txn.Date)
	fmt.Fprintf(buf, "  Amount: $%.2f\n", txn.Amount)
	fmt.Fprintf(buf, "  Direction: %s\n", txn.Direction)
	fmt.Fprintf(buf, "  Counterparty text: \"%s\"\n", txn.CounterpartyName)
	if txn.CounterpartyAccount != "" {
		fmt.Fprintf(buf, "  Counterparty account: \"%s\"\n", txn.CounterpartyAccount)
	}
	if txn.BankReference != "" {
		fmt.Fprintf(buf, "  Bank reference: \"%s\"\n", txn.BankReference)
	}
	if txn.RemittanceInfo != "" {
		fmt.Fprintf(buf, "  Remittance info: \"%s\"\n", txn.RemittanceInfo)
	}
	fmt.Fprintf(buf, "\nInvestigate this payment. Determine which customer sent it and which invoices it covers.")

	return buf.String()
}

func callClaude(apiKey string, messages []claudeMessage, round int, verbose bool) (*claudeResponse, error) {
	// System prompt as a content block — marked for caching since it never changes across transactions.
	system := []map[string]interface{}{
		{
			"type":          "text",
			"text":          systemPrompt,
			"cache_control": map[string]string{"type": "ephemeral"},
		},
	}

	// Tool definitions are also static; mark the last one so the entire list is cached.
	toolDefs := tools.ToolDefinitions()
	toolsInterface := make([]interface{}, len(toolDefs))
	for i, t := range toolDefs {
		toolsInterface[i] = t
	}
	if len(toolDefs) > 0 {
		lastTool := make(map[string]interface{})
		for k, v := range toolDefs[len(toolDefs)-1] {
			lastTool[k] = v
		}
		lastTool["cache_control"] = map[string]string{"type": "ephemeral"}
		toolsInterface[len(toolsInterface)-1] = lastTool
	}

	req := claudeRequest{
		Model:       claudeModel,
		MaxTokens:   4096,
		Temperature: 0,
		System:      system,
		Tools:       toolsInterface,
		Messages:    messages,
	}

	if verbose {
		printVerboseRequest(round, messages)
	}

	// Block until a rate-limit slot is available (shared with multi-agent goroutines).
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
		printVerboseResponse(round, &resp)
	}

	return &resp, nil
}

func parseMatchResult(input json.RawMessage) (*models.AgentMatchResult, error) {
	var result models.AgentMatchResult
	if err := json.Unmarshal(input, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// CalculateCost computes the total API cost using Haiku pricing with prompt-caching rates:
//
//	Input (uncached):   $0.80 / MTok
//	Cache write:        $1.00 / MTok  (+25% vs normal input)
//	Cache read:         $0.08 / MTok  (90% cheaper than normal input)
//	Output:             $4.00 / MTok
func CalculateCost(inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int) float64 {
	inputCost := float64(inputTokens) * 0.80 / 1_000_000
	outputCost := float64(outputTokens) * 4.00 / 1_000_000
	cacheWriteCost := float64(cacheCreationTokens) * 1.00 / 1_000_000
	cacheReadCost := float64(cacheReadTokens) * 0.08 / 1_000_000
	return inputCost + outputCost + cacheWriteCost + cacheReadCost
}
