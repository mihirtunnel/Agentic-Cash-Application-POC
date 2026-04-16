package reactive

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cashapp-agent-poc/internal/agent"
	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/tools"
)

// ReactiveResult holds the full output of the reactive chain pipeline.
type ReactiveResult struct {
	Match      *models.AgentMatchResult
	TotalUsage TokenUsage
	Phase      string // "primary" or "escalation"
	PreFetch   *PreFetchData
	ToolCalls  int // total tool calls (pre-fetch + primary + escalation)
}

// PreFetchData captures the deterministic pre-fetch results from Phase 0.
type PreFetchData struct {
	AmountMatches string
	EmailMatches  string
}

// --- System prompts ---

var primaryAgentPrompt = `You are a cash application specialist for Tauber Oil Company processing incoming payments. You receive an incoming bank payment along with PRE-FETCHED DATA that has already been gathered for you.

CRITICAL: INTERNAL TRANSFER CHECK FIRST
Before analyzing pre-fetched data, check if this is an internal Tauber transfer:

INTERNAL TRANSFER INDICATORS:
- Remittance text shows SENDER and BENE are both Tauber entities
- Bank memo contains "TRANSFER FROM CHECKING ACCT" or similar ZBA routing language
- ZBA or Zero Balance Account movement
- No external customer name visible
- Scenario indicates "INTERNAL"

If this is an INTERNAL TRANSFER:
- Set status to "internal_transfer"
- Set confidence to 1.0
- Submit immediately without analyzing pre-fetched data
- Reasoning: "Internal Tauber transfer identified"

CUSTOMER PAYMENT MATCHING (only if NOT internal):
PRE-FETCHED DATA was gathered automatically for Tauber Oil payments:
1. AMOUNT MATCHES: Tauber Oil customers whose open invoices match the payment amount (±3%)
2. REMITTANCE EMAILS: Emails matching the payment amount (±5%) and date (±7 days)

YOUR APPROACH:
1. FIRST, review the pre-fetched data carefully. In many cases it contains everything you need.
2. If the pre-fetched data clearly identifies one customer with matching invoices, call submit_match_result IMMEDIATELY without using any other tools.
3. ONLY use tools if the pre-fetched data is genuinely insufficient:
   - Multiple ambiguous candidates → use search_customers_by_name to disambiguate
   - Need to verify a candidate's details → use get_customer_details
   - Need specific invoice details → use get_customer_invoices
   - Bank reference or remittance info contains a code → use lookup_reference
4. Be efficient — justify every tool call. Do not explore unless necessary.

MATCHING GUIDELINES FOR CUSTOMERS:
- Account for early-pay discounts (typically 1-2% of invoice amount)
- If the payment is 1-3% below the invoice total, the customer likely took an early-pay discount
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If the bank counterparty text doesn't resemble the customer name, include suggested_alias
- ONLY use customer IDs and invoice IDs that appear in the data (pre-fetched or tool results). NEVER invent IDs.

CONFIDENCE GUIDELINES:
- 0.95+: Exact amount match + remittance email confirms invoices
- 0.85-0.94: Strong amount match with single candidate + name corroboration
- 0.75-0.84: Amount match but needs verification
- Below 0.70: Insufficient evidence — submit as unresolved

You MUST call submit_match_result to complete. Do it as soon as you have enough evidence.`

var escalationAgentPrompt = `You are a SENIOR cash application specialist for Tauber Oil Company. A primary agent attempted to match an incoming payment but could not resolve it within its budget. You receive the original transaction, all pre-fetched data, and a summary of what the primary agent tried and found.

CRITICAL: INTERNAL TRANSFER CHECK FIRST
Before attempting customer matching, check if the primary agent's inability to find a match indicates an internal transfer:

INTERNAL TRANSFER SIGNS:
- Primary agent found NO external customers despite searching
- Remittance text shows only Tauber entities as sender/beneficiary
- Bank memo contains "TRANSFER FROM CHECKING ACCT" or ZBA routing language
- No customer ID match found after thorough search
- References indicate inter-company or ZBA movement

If this appears to be INTERNAL TRANSFER:
- Set status to "internal_transfer"
- Set confidence to 1.0
- Submit immediately with reasoning "Primary agent found no external customer — internal transfer confirmed"

CUSTOMER MATCHING (only if NOT internal):
Your job is to pick up where the primary agent left off and make a final decision. You have a wider tool set and should focus on what was missing.

STRATEGY:
1. Review what the primary agent already found — don't repeat successful searches
2. Try different angles: wider amount tolerance, different name search terms, sender-keyword email search
3. If the primary agent found candidates but couldn't confirm, use get_customer_details and get_customer_invoices to verify
4. If nothing works, it's OK to submit as "unresolved"

MATCHING GUIDELINES:
- Account for early-pay discounts (typically 1-2% of invoice amount)
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If the bank counterparty text doesn't resemble the customer name, include suggested_alias
- ONLY use customer IDs and invoice IDs that appear in the data. NEVER invent IDs.

CONFIDENCE GUIDELINES:
- 0.95+: Multiple signals agree (name + amount + email)
- 0.85-0.94: Two signals agree OR one very strong signal
- 0.75-0.84: Single strong signal
- Below 0.70: Insufficient — submit as "unresolved"

You MUST call submit_match_result to complete.`

// --- Tool sets ---

// primaryTools: interpretation-dependent tools only.
// search_customers_by_amount and search_remittance_emails are excluded (already pre-fed).
// check_credit_memos is excluded (stub, wastes a round).
func primaryTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	return filterTools(all,
		"search_customers_by_name",
		"lookup_reference",
		"get_customer_details",
		"get_customer_invoices",
		"get_customer_payment_history",
		"submit_match_result",
	)
}

// escalationTools: wider set — includes the pre-fed tools again (with different params)
// plus everything the primary agent had.
func escalationTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	return filterTools(all,
		"search_customers_by_name",
		"search_customers_by_amount",
		"search_remittance_emails",
		"lookup_reference",
		"get_customer_details",
		"get_customer_invoices",
		"get_customer_payment_history",
		"submit_match_result",
	)
}

func filterTools(all []map[string]interface{}, keep ...string) []map[string]interface{} {
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}
	var result []map[string]interface{}
	for _, t := range all {
		if name, ok := t["name"].(string); ok && keepSet[name] {
			result = append(result, t)
		}
	}
	return result
}

// --- Entry point ---

// RunReactive orchestrates the Reactive Chain pipeline:
// Phase 0: Deterministic pre-fetch — amount matches + remittance emails (0 LLM calls)
// Phase 1: Pre-fed agent with tools, strict 3-round budget (1 LLM call + 0-3 tool rounds)
// Phase 2: If Phase 1 didn't submit or confidence < 0.80, escalation agent with wider tools (1 LLM call + 0-3 tool rounds)
func RunReactive(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*ReactiveResult, error) {
	result := &ReactiveResult{}

	// ---- Phase 0: Deterministic pre-fetch (0 LLM calls) ----
	fmt.Println("  Phase 0: Deterministic pre-fetch (amount matches + remittance emails)...")

	preFetch, preFetchToolCalls := runPreFetch(txn, toolHandler, verbose)
	result.PreFetch = preFetch
	result.ToolCalls = preFetchToolCalls

	// ---- Phase 1: Primary agent (pre-fed data + tools, max 3 rounds) ----
	fmt.Println("  Phase 1: Primary agent with pre-fetched data (tools available, max 3 rounds)...")

	primaryInput := buildPrimaryPrompt(txn, preFetch)

	submitInput, agentText, primaryUsage, primaryToolCalls, primaryErr := runToolLoop(
		"ReactivePrimary",
		primaryAgentPrompt,
		primaryTools(),
		primaryInput,
		3,
		toolHandler.Execute,
		"submit_match_result",
		verbose,
	)
	result.TotalUsage.Add(primaryUsage)
	result.ToolCalls += primaryToolCalls

	fmt.Printf("    Primary agent: %d tool call(s), %d in / %d out\n",
		primaryToolCalls, primaryUsage.InputTokens, primaryUsage.OutputTokens)

	if primaryErr != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Primary agent error: %v", primaryErr),
			InvestigationSummary: "Reactive pipeline failed during primary agent phase.",
		}
		return result, primaryErr
	}

	// Check if primary agent submitted a result
	if submitInput != nil {
		var match models.AgentMatchResult
		if parseErr := json.Unmarshal(submitInput, &match); parseErr != nil {
			result.Match = &models.AgentMatchResult{
				Status:               "unresolved",
				Reasoning:            fmt.Sprintf("Failed to parse primary result: %v", parseErr),
				InvestigationSummary: "Reactive primary agent returned unparseable submit_match_result.",
			}
			return result, nil
		}

		// If confidence >= 0.80, accept it
		if match.Confidence >= 0.80 {
			fmt.Printf("    → Primary agent matched with confidence %.2f (accepted)\n", match.Confidence)
			result.Match = &match
			result.Phase = "primary"
			return result, nil
		}

		// Low confidence — escalate
		fmt.Printf("    → Primary agent matched with confidence %.2f (below 0.80 — escalating)\n", match.Confidence)
		agentText += fmt.Sprintf("\n[Primary agent submitted with low confidence %.2f: %s]", match.Confidence, match.Reasoning)
	} else {
		fmt.Println("    → Primary agent did not submit a result — escalating")
	}

	// ---- Phase 2: Escalation agent (wider tools, max 3 rounds) ----
	fmt.Println("  Phase 2: Escalation agent with wider tools (max 3 rounds)...")

	escalationInput := buildEscalationPrompt(txn, preFetch, agentText)

	escSubmitInput, _, escUsage, escToolCalls, escErr := runToolLoop(
		"ReactiveEscalation",
		escalationAgentPrompt,
		escalationTools(),
		escalationInput,
		3,
		toolHandler.Execute,
		"submit_match_result",
		verbose,
	)
	result.TotalUsage.Add(escUsage)
	result.ToolCalls += escToolCalls

	fmt.Printf("    Escalation agent: %d tool call(s), %d in / %d out\n",
		escToolCalls, escUsage.InputTokens, escUsage.OutputTokens)

	if escErr != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Escalation agent error: %v", escErr),
			InvestigationSummary: "Reactive pipeline failed during escalation phase.",
		}
		return result, escErr
	}

	if escSubmitInput != nil {
		var match models.AgentMatchResult
		if parseErr := json.Unmarshal(escSubmitInput, &match); parseErr != nil {
			result.Match = &models.AgentMatchResult{
				Status:               "unresolved",
				Reasoning:            fmt.Sprintf("Failed to parse escalation result: %v", parseErr),
				InvestigationSummary: "Reactive escalation agent returned unparseable submit_match_result.",
			}
			result.Phase = "escalation"
			return result, nil
		}
		result.Match = &match
		result.Phase = "escalation"
		fmt.Printf("    → Escalation agent produced final decision (confidence %.2f)\n", match.Confidence)
	} else {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            "Neither primary nor escalation agent submitted a result.",
			InvestigationSummary: "Reactive pipeline: both agents exhausted their budgets without a decision.",
		}
		result.Phase = "escalation"
	}

	return result, nil
}

// --- Phase 0: Deterministic pre-fetch ---

func runPreFetch(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*PreFetchData, int) {
	preFetch := &PreFetchData{}
	toolCalls := 0

	// Tool 1: search_customers_by_amount
	amountInput, _ := json.Marshal(map[string]interface{}{
		"amount":            txn.Amount,
		"tolerance_percent": 3,
	})
	amountResult, err := toolHandler.Execute("search_customers_by_amount", amountInput)
	toolCalls++
	if err != nil {
		amountResult = fmt.Sprintf("Error: %s", err.Error())
	}
	preFetch.AmountMatches = amountResult

	if verbose {
		fmt.Printf("    ┌─ Pre-fetch: search_customers_by_amount\n")
		fmt.Printf("    │ Input:  {amount: %.2f, tolerance: 3%%}\n", txn.Amount)
		fmt.Printf("    │ Result: %s\n", truncate(amountResult, 200))
		fmt.Printf("    └──────────────────────────────────────────────────\n")
	}

	// Tool 2: search_remittance_emails (amount ±5%, date ±7 days)
	txnDate, dateErr := time.Parse("2006-01-02", txn.Date)
	dateFrom := txn.Date
	dateTo := txn.Date
	if dateErr == nil {
		dateFrom = txnDate.AddDate(0, 0, -7).Format("2006-01-02")
		dateTo = txnDate.AddDate(0, 0, 1).Format("2006-01-02")
	}

	emailInput, _ := json.Marshal(map[string]interface{}{
		"amount_min": txn.Amount * 0.95,
		"amount_max": txn.Amount * 1.05,
		"date_from":  dateFrom,
		"date_to":    dateTo,
	})
	emailResult, err := toolHandler.Execute("search_remittance_emails", emailInput)
	toolCalls++
	if err != nil {
		emailResult = fmt.Sprintf("Error: %s", err.Error())
	}
	preFetch.EmailMatches = emailResult

	if verbose {
		fmt.Printf("    ┌─ Pre-fetch: search_remittance_emails\n")
		fmt.Printf("    │ Input:  {amount: %.2f–%.2f, date: %s to %s}\n",
			txn.Amount*0.95, txn.Amount*1.05, dateFrom, dateTo)
		fmt.Printf("    │ Result: %s\n", truncate(emailResult, 200))
		fmt.Printf("    └──────────────────────────────────────────────────\n")
	}

	return preFetch, toolCalls
}

// --- Prompt builders ---

func buildPrimaryPrompt(txn models.BankTransaction, preFetch *PreFetchData) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("MATCH THIS PAYMENT:\n\n"+
		"  Transaction ID: %s\n"+
		"  Date: %s\n"+
		"  Amount: $%.2f\n"+
		"  Direction: %s\n"+
		"  Counterparty text: \"%s\"\n",
		txn.TxnID, txn.Date, txn.Amount, txn.Direction, txn.CounterpartyName))

	if txn.CounterpartyAccount != "" {
		sb.WriteString(fmt.Sprintf("  Counterparty account: \"%s\"\n", txn.CounterpartyAccount))
	}
	if txn.BankReference != "" {
		sb.WriteString(fmt.Sprintf("  Bank reference: \"%s\"\n", txn.BankReference))
	}
	if txn.RemittanceInfo != "" {
		sb.WriteString(fmt.Sprintf("  Remittance info: \"%s\"\n", txn.RemittanceInfo))
	}

	sb.WriteString("\n═══════════════════════════════════════\n")
	sb.WriteString("PRE-FETCHED DATA (already gathered for you — review this FIRST):\n")
	sb.WriteString("═══════════════════════════════════════\n")

	sb.WriteString("\n── AMOUNT MATCHES (customers with invoice combos matching $")
	sb.WriteString(fmt.Sprintf("%.2f ±3%%) ──\n", txn.Amount))
	sb.WriteString(preFetch.AmountMatches)

	sb.WriteString("\n\n── REMITTANCE EMAILS (matching amount ±5%, date ±7 days) ──\n")
	sb.WriteString(preFetch.EmailMatches)

	sb.WriteString("\n\n═══════════════════════════════════════\n")
	sb.WriteString("Review the pre-fetched data above. If sufficient, call submit_match_result immediately. Only use tools if you need more information.\n")

	return sb.String()
}

func buildEscalationPrompt(txn models.BankTransaction, preFetch *PreFetchData, primarySummary string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("MATCH THIS PAYMENT (escalated from primary agent):\n\n"+
		"  Transaction ID: %s\n"+
		"  Date: %s\n"+
		"  Amount: $%.2f\n"+
		"  Direction: %s\n"+
		"  Counterparty text: \"%s\"\n",
		txn.TxnID, txn.Date, txn.Amount, txn.Direction, txn.CounterpartyName))

	if txn.CounterpartyAccount != "" {
		sb.WriteString(fmt.Sprintf("  Counterparty account: \"%s\"\n", txn.CounterpartyAccount))
	}
	if txn.BankReference != "" {
		sb.WriteString(fmt.Sprintf("  Bank reference: \"%s\"\n", txn.BankReference))
	}
	if txn.RemittanceInfo != "" {
		sb.WriteString(fmt.Sprintf("  Remittance info: \"%s\"\n", txn.RemittanceInfo))
	}

	sb.WriteString("\n═══════════════════════════════════════\n")
	sb.WriteString("ORIGINAL PRE-FETCHED DATA:\n")
	sb.WriteString("═══════════════════════════════════════\n")

	sb.WriteString("\n── AMOUNT MATCHES ──\n")
	sb.WriteString(preFetch.AmountMatches)

	sb.WriteString("\n\n── REMITTANCE EMAILS ──\n")
	sb.WriteString(preFetch.EmailMatches)

	sb.WriteString("\n\n═══════════════════════════════════════\n")
	sb.WriteString("PRIMARY AGENT'S WORK (what was already tried):\n")
	sb.WriteString("═══════════════════════════════════════\n\n")

	if primarySummary != "" {
		sb.WriteString(primarySummary)
	} else {
		sb.WriteString("(Primary agent produced no text output)")
	}

	sb.WriteString("\n\n═══════════════════════════════════════\n")
	sb.WriteString("Pick up where the primary agent left off. You have wider tool access (including search_customers_by_amount with different tolerance and search_remittance_emails with sender keywords). Focus on what was missing and submit your final decision.\n")

	return sb.String()
}

// CalculateReactiveCost reuses the same Haiku pricing as other pipelines.
func CalculateReactiveCost(u TokenUsage) float64 {
	return agent.CalculateCost(u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens)
}
