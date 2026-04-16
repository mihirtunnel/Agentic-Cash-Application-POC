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
	openaiAPIURL = "https://api.openai.com/v1/chat/completions"
	openaiModel  = "gpt-5.4-nano"
	maxToolCalls = 25
)

// - Bank reference contains both SENDER and BENE (beneficiary) that are Tauber entities (Tauber Oil, Tauber Petrochemical, etc.)
// - Example: "SENDER=TAUBER OIL COMPANY / BENE=TAUBER PETROCHEMICAL CO"
// - ZBA or Zero Balance Account transfers between Tauber subsidiaries
// - Bank memo contains "TRANSFER FROM CHECKING ACCT" or similar ZBA routing pattern — no customer counterparty, internal movement only
// - Scenario field contains "INTERNAL"
// - No external customer counterparty — only internal movement

// If you identify this as an INTERNAL TRANSFER:
// - Set status to "internal_transfer"
// - Set confidence to 1.0
// - Do NOT perform customer matching
// - Submit result immediately with submit_match_result
// - Reasoning: "This is an internal Tauber Oil entity transfer"

var systemPrompt = `You are a treasury cash application specialist at Tauber Oil Company. Your job is to first identify whether each bank payment is an INTERNAL TRANSFER or a CUSTOMER PAYMENT, then match customer payments to the correct customer and invoices.

CRITICAL: INTERNAL TRANSFER DETECTION FIRST
Before any customer matching, check if this is an internal Tauber Oil transfer:

INTERNAL TRANSFER INDICATORS:

An internal transfer means money moving between two Tauber entities with NO external customer involved.
 
INTERNAL TRANSFER REQUIRES ALL of the following to be true:
  a) SENDER is a Tauber entity (Tauber Oil, Tauber Petrochemical, Tauber Trading, etc.)
  b) BENE is also a Tauber entity
  c) There is no external company name (customer/applicant) anywhere in the remittance text
 
GOOD internal transfer example:
  "SENDER=TAUBER OIL COMPANY / BENE=TAUBER PETROCHEMICAL CO"  ← both sides are Tauber ✓
 
NOT internal transfers — these are CUSTOMER PAYMENTS:
  "SENDER=EXXONMOBIL OIL CORPORATION / BENE=TAUBER OIL COMPANY"
    ← SENDER is an external customer, so this is a customer payment. Proceed to matching.
  "SENDER=STATE BANK OF INDIA / BENE=TAUBER PETROCHEMICAL CO / APPLICANT=SANMAN TRADE IMPEX LTD"
    ← Sent via correspondent bank on behalf of external customer SANMAN. Proceed to matching.
  ZBA / "TRANSFER FROM CHECKING ACCT" with no external counterparty ← internal only if both accounts are Tauber
 
IMPORTANT: Tauber appearing as the BENE (recipient) does NOT make something an internal transfer.
Tauber is always the recipient — we are Tauber. What matters is whether the SENDER is also Tauber.
 
If and only if it is a confirmed internal transfer:
- Set status to "internal_transfer", confidence to 1.0
- Submit immediately with submit_match_result
- Do NOT do any customer matching


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

// ── OpenAI wire types ────────────────────────────────────────────────────────

// openaiToolCall represents a single tool call in an assistant message.
// FIX: Defined as a concrete struct so it serialises correctly as a top-level
//
//	field on the assistant message (not nested inside "content").
type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // always "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openaiMessage is the union type used for every role.
//
// FIX 1 (assistant messages with tool calls): ToolCalls is a separate top-level
//
//	field, NOT nested inside Content.  When the assistant calls a tool, Content
//	may be empty ("") — that is legal per the OpenAI spec.
//
// FIX 2 (tool-result messages): Role must be "tool", NOT "user".
//
//	ToolCallID ties the result back to the assistant's tool_call entry.
//	Name is not used by the current OpenAI tool-calling API and is omitted.
type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`                // plain string only; omit when empty for tool msgs
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`   // assistant → tool invocations
	ToolCallID string           `json:"tool_call_id,omitempty"` // tool result → which call this answers
}

// openaiRequest is the top-level POST body.
type openaiRequest struct {
	Model               string          `json:"model"`
	Messages            []openaiMessage `json:"messages"`
	Tools               []openaiToolDef `json:"tools,omitempty"`
	Temperature         float64         `json:"temperature"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	ToolChoice          string          `json:"tool_choice,omitempty"`
}

// openaiToolDef is the OpenAI function-tool schema.
// FIX 3: Using a typed struct guarantees "type":"function" is always present.
//
//	Previously the code used []interface{} which could silently drop the
//	"type" key, causing the "Missing required parameter: tools[0].type" error.
type openaiToolDef struct {
	Type     string         `json:"type"` // must be "function"
	Function openaiToolFunc `json:"function"`
}

type openaiToolFunc struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"` // JSON Schema object
}

// openaiResponse is the top-level response body.
type openaiResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ── Result / logging types ───────────────────────────────────────────────────

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

// ── Agent entry point ────────────────────────────────────────────────────────

func RunAgent(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*RunResult, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	prompt := buildTransactionPrompt(txn)
	messages := []openaiMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	}

	result := &RunResult{}
	toolCallCount := 0

	for round := 1; toolCallCount < maxToolCalls; round++ {
		resp, err := callOpenAI(apiKey, messages, round, verbose)
		if err != nil {
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

		result.TotalInputTokens += resp.Usage.PromptTokens
		result.TotalOutputTokens += resp.Usage.CompletionTokens

		roundLog := RoundLog{
			Round:        round,
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}

		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("no choices in response")
		}

		choice := resp.Choices[0]
		roundLog.AgentText = choice.Message.Content

		// ── No tool calls: model finished or needs a nudge ────────────────
		if len(choice.Message.ToolCalls) == 0 {
			if choice.FinishReason == "stop" {
				messages = append(messages, openaiMessage{Role: "assistant", Content: choice.Message.Content})
				messages = append(messages, openaiMessage{
					Role:    "user",
					Content: "Please call the submit_match_result tool to submit your findings.",
				})
				result.Rounds = append(result.Rounds, roundLog)
				printRound(roundLog)
				continue
			}
			break
		}

		// ── Build assistant message with tool_calls at the top level ──────
		// FIX: Previously this was wrapped in a map inside Content, which is
		//      invalid. Tool calls must live in the ToolCalls field.
		assistantMsg := openaiMessage{
			Role:      "assistant",
			Content:   choice.Message.Content, // may be "" — that is fine
			ToolCalls: choice.Message.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		// ── Execute each tool call and append role="tool" results ─────────
		for _, toolCall := range choice.Message.ToolCalls {
			toolCallCount++
			functionName := toolCall.Function.Name

			// Handle submit_match_result first (no execution needed)
			if functionName == "submit_match_result" {
				matchResult, err := parseMatchResult([]byte(toolCall.Function.Arguments))
				if err != nil {
					return nil, fmt.Errorf("parsing submit_match_result: %w", err)
				}
				roundLog.ToolCalls = append(roundLog.ToolCalls, ToolCallLog{
					Num:   toolCallCount,
					Name:  "submit_match_result",
					Input: toolCall.Function.Arguments,
				})
				result.Match = matchResult
				result.ToolCallCount = toolCallCount
				result.Rounds = append(result.Rounds, roundLog)
				printRound(roundLog)
				return result, nil
			}

			// Execute the tool
			output, execErr := toolHandler.Execute(functionName, []byte(toolCall.Function.Arguments))
			if execErr != nil {
				output = fmt.Sprintf("Error: %s", execErr.Error())
			}
			roundLog.ToolCalls = append(roundLog.ToolCalls, ToolCallLog{
				Num:    toolCallCount,
				Name:   functionName,
				Input:  toolCall.Function.Arguments,
				Output: output,
			})

			// FIX: Role must be "tool" (not "user"), and ToolCallID must be set.
			//      The old code used role="user" with a Name field, which is the
			//      deprecated /v1/chat/completions function-calling format and is
			//      rejected by the current API with a 400 invalid_type error.
			messages = append(messages, openaiMessage{
				Role:       "tool",
				Content:    output,
				ToolCallID: toolCall.ID,
			})
		}

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

// ── OpenAI HTTP call ─────────────────────────────────────────────────────────

func callOpenAI(apiKey string, messages []openaiMessage, round int, verbose bool) (*openaiResponse, error) {
	// Convert Claude-format tool definitions to the typed OpenAI struct.
	// FIX: Using openaiToolDef (typed) instead of []interface{} ensures the
	//      "type":"function" field is always serialised, eliminating the
	//      "Missing required parameter: tools[0].type" 400 error.
	toolDefs := tools.ToolDefinitions()
	openaiTools := make([]openaiToolDef, 0, len(toolDefs))
	for _, t := range toolDefs {
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		params := t["input_schema"] // OpenAI calls this "parameters"

		openaiTools = append(openaiTools, openaiToolDef{
			Type: "function",
			Function: openaiToolFunc{
				Name:        name,
				Description: desc,
				Parameters:  params,
			},
		})
	}

	req := openaiRequest{
		Model:               openaiModel,
		Messages:            messages,
		Tools:               openaiTools,
		Temperature:         0,
		MaxCompletionTokens: 4096,
		ToolChoice:          "auto",
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

	httpReq, err := http.NewRequest("POST", openaiAPIURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

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

	var resp openaiResponse
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

// ── Verbose printing ─────────────────────────────────────────────────────────

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

func printVerboseRequest(round int, messages []openaiMessage) {
	fmt.Printf("\n  ╔══════════════════════════════════════════════════════════════\n")
	fmt.Printf("  ║  ► API CALL #%d — SENDING  (%d message(s) in conversation)\n", round, len(messages))
	fmt.Printf("  ╠══════════════════════════════════════════════════════════════\n")
	for i, msg := range messages {
		fmt.Printf("  ║\n  ║  [msg %d]  role=%s\n", i+1, msg.Role)
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				fmt.Printf("  ║    [tool_call] id=%s name=%s\n", tc.ID, tc.Function.Name)
				fmt.Printf("  ║      args: %s\n", truncate(tc.Function.Arguments, 200))
			}
		} else {
			printIndented("  ║    ", msg.Content)
		}
	}
	fmt.Printf("  ╚══════════════════════════════════════════════════════════════\n")
}

func printVerboseResponse(round int, resp *openaiResponse) {
	fmt.Printf("\n  ╔══════════════════════════════════════════════════════════════\n")
	fmt.Printf("  ║  ◄ API CALL #%d — RECEIVED  tokens: %d in / %d out  finish_reason: %s\n",
		round, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Choices[0].FinishReason)
	fmt.Printf("  ╠══════════════════════════════════════════════════════════════\n")
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message.Content != "" {
			fmt.Printf("  ║  [text]\n")
			printIndented("  ║    ", choice.Message.Content)
		}
		for _, toolCall := range choice.Message.ToolCalls {
			fmt.Printf("  ║  [tool_call]  name=%s\n", toolCall.Function.Name)
			fmt.Printf("  ║    arguments: %s\n", toolCall.Function.Arguments)
		}
	}
	fmt.Printf("  ╚══════════════════════════════════════════════════════════════\n\n")
}

func printIndented(prefix, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		fmt.Printf("%s%s\n", prefix, line)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

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

// CalculateCost computes the total API cost using GPT-4o pricing:
//
//	Input:   $2.50 / MTok
//	Output:  $10.00 / MTok
func CalculateCost(inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int) float64 {
	inputCost := float64(inputTokens) * 2.50 / 1_000_000
	outputCost := float64(outputTokens) * 10.00 / 1_000_000
	return inputCost + outputCost
}
