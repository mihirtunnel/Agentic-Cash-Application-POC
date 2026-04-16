package funnel

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cashapp-agent-poc/internal/agent"
	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/tools"
)

// FunnelResult holds the full output of the funnel pipeline.
type FunnelResult struct {
	Match      *models.AgentMatchResult
	TotalUsage TokenUsage
	Phase      string // which phase produced the final result: "triage" or "resolver"
	PreFetch   *PreFetchData
	ToolCalls  int // total tool calls requested by the triage agent and executed locally
}

// PreFetchData captures the deterministic pre-fetch results from Phase 0.
type PreFetchData struct {
	AmountMatches string
	EmailMatches  string
}

// TriageDecision is what the triage agent returns as JSON.
type TriageDecision struct {
	Decision    string                   `json:"decision"` // "match" or "need_more"
	Result      *models.AgentMatchResult `json:"result,omitempty"`
	NeededTools []NeededTool             `json:"needed_tools,omitempty"`
	Reasoning   string                   `json:"reasoning"`
}

// NeededTool is a tool the triage agent wants to run for follow-up.
type NeededTool struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	Label string          `json:"label"`
}

// --- System prompts ---

var triagePrompt = `You are a cash application specialist for Tauber Oil Company processing incoming payments. You receive an incoming bank payment along with PRE-FETCHED DATA (amount matches and remittance email matches). Your job is to first check if this is an INTERNAL TRANSFER, then determine whether you can confidently match customer payments to Tauber Oil customers using ONLY the pre-fetched data.

CRITICAL: INTERNAL TRANSFER CHECK FIRST
Analyze the transaction and pre-fetched data for internal transfer indicators:

INTERNAL TRANSFER SIGNS:
- Remittance info shows SENDER=TAUBER and BENE=TAUBER (both Tauber entities)
- Bank memo contains "TRANSFER FROM CHECKING ACCT" or similar ZBA/inter-company routing pattern
- No external customer in remittance data or amount matches
- Scenario explicitly says "INTERNAL"
- Zero Balance Account movements between Tauber subsidiaries

If this is INTERNAL TRANSFER:
- Respond with decision "match" and status "internal_transfer"
- Set confidence to 1.0
- Do NOT request additional tools or data
- Response example:
{
  "decision": "match",
  "result": {
    "status": "internal_transfer",
    "confidence": 1.0,
    "reasoning": "Internal transfer between Tauber entities",
    "investigation_summary": "Transaction identified as internal Tauber movement"
  },
  "reasoning": "Internal transfer confirmed — no customer matching needed"
}

CUSTOMER PAYMENT MATCHING (only if NOT internal transfer):
ANALYZE THE PRE-FETCHED DATA:
1. AMOUNT MATCHES: Shows Tauber Oil customers whose open invoices (individually or combined) match the payment amount within ±3%. This is often the strongest signal.
2. EMAIL MATCHES: Shows remittance advice emails received around the payment date with similar amounts. These often contain exact invoice references.

YOUR DECISION:

If you can confidently identify the customer and allocate invoices from the pre-fetched data alone, respond with decision "match" and a complete result.

If the pre-fetched data is insufficient (e.g., multiple ambiguous candidates, no amount match, need name verification), respond with decision "need_more" and list the specific tools you want run.

RESPONSE FORMAT — respond with ONLY a JSON object, nothing else:

Option A — Confident customer match:
{
  "decision": "match",
  "result": {
    "status": "matched",
    "customer_id": "CUST-XXXX",
    "customer_name": "...",
    "confidence": 0.0-1.0,
    "invoices": [
      {"id": "INV-XXX", "original_amount": 0, "applied": 0, "deduction": 0, "reason": ""}
    ],
    "total_applied": 0,
    "unmatched_amount": 0,
    "reasoning": "...",
    "investigation_summary": "...",
    "suggested_alias": ""
  },
  "reasoning": "why this match is confident"
}

Option B — Need more data:
{
  "decision": "need_more",
  "needed_tools": [
    {"tool": "search_customers_by_name", "input": {"query": "ACME CORP"}, "label": "name_search"},
    {"tool": "get_customer_invoices", "input": {"customer_id": "CUST-0091"}, "label": "invoices_0091"},
    ...
  ],
  "reasoning": "what is ambiguous and what additional data will resolve it"
}

Option C — No customer match possible:
{
  "decision": "match",
  "result": {
    "status": "unresolved",
    "confidence": 0.0,
    "reasoning": "...",
    "investigation_summary": "..."
  },
  "reasoning": "why no match is possible"
}

AVAILABLE TOOLS YOU CAN REQUEST (for decision "need_more"):
- search_customers_by_name: {"query": "name text", "limit": 5}
- lookup_reference: {"reference": "code"}
- get_customer_details: {"customer_id": "CUST-XXXX"}
- get_customer_invoices: {"customer_id": "CUST-XXXX"}
- get_customer_payment_history: {"customer_id": "CUST-XXXX"}
- search_customers_by_amount: {"amount": N, "tolerance_percent": N} (with different params than pre-fetched)
- search_remittance_emails: {"sender_keyword": "...", ...} (with keyword filter this time)

MATCHING GUIDELINES:
- Account for early-pay discounts (typically 1-2% of invoice amount)
- If the amount matches exactly, confidence is high
- If the amount is 1-3% below invoice total, check if the customer has a discount policy
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If the bank counterparty text doesn't resemble the matched customer name, include suggested_alias
- ONLY use customer IDs and invoice IDs that appear in the pre-fetched data. NEVER invent or guess an ID.
- If you can't match confidently, request more data rather than guessing

CONFIDENCE GUIDELINES:
- 0.95+: Exact amount match + remittance email confirms invoices
- 0.85-0.94: Strong amount match with only one candidate
- 0.75-0.84: Amount match but needs name/reference verification
- Below 0.70: Insufficient evidence — request more data or mark unresolved

Respond with ONLY the JSON object, nothing else.`

var resolverPrompt = `You are a cash application specialist for Tauber Oil Company making a FINAL DECISION on a payment match. A triage agent already reviewed pre-fetched data (amount matches, remittance emails) and requested additional data. You now have ALL the data — original pre-fetched data plus the follow-up results.

Your job: analyze everything and produce the final match result for this Tauber Oil payment.

CRITICAL: CHECK FOR INTERNAL TRANSFER FIRST
Before attempting to match to a customer, analyze all data for internal transfer indicators:

INTERNAL TRANSFER SIGNS:
- Triage reasoning or pre-fetch data shows SENDER=TAUBER and BENE=TAUBER
- Bank memo contains "TRANSFER FROM CHECKING ACCT" or ZBA routing language
- No external customer found despite comprehensive search attempts
- All follow-up tool results returned no matches (all no_match or empty)
- Scenario or transaction description explicitly says "INTERNAL"

If this is an INTERNAL TRANSFER:
- Set status to "internal_transfer"
- Set confidence to 1.0
- Return immediately — do NOT try to force a customer match
- Response:
{
  "status": "internal_transfer",
  "confidence": 1.0,
  "reasoning": "Internal transfer between Tauber entities confirmed",
  "investigation_summary": "All search attempts found no external customer; transaction is internal Tauber movement"
}

CUSTOMER MATCH (only if NOT internal transfer):

RESPONSE FORMAT — respond with ONLY a JSON object matching this schema:
{
  "status": "matched" or "unresolved",
  "customer_id": "CUST-XXXX",
  "customer_name": "...",
  "confidence": 0.0-1.0,
  "invoices": [
    {"id": "INV-XXX", "original_amount": 0, "applied": 0, "deduction": 0, "reason": ""}
  ],
  "total_applied": 0,
  "unmatched_amount": 0,
  "reasoning": "...",
  "investigation_summary": "...",
  "suggested_alias": ""
}

MATCHING GUIDELINES:
- Account for early-pay discounts (typically 1-2% of invoice amount)
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If the bank counterparty text doesn't resemble the matched customer name, include suggested_alias
- ONLY use customer IDs and invoice IDs that appear in the provided data. NEVER invent or guess an ID.
- If evidence is insufficient, set status to "unresolved"
- Cross-reference all signals: name match, amount match, email match, reference lookup

CONFIDENCE GUIDELINES:
- 0.95+: Multiple signals agree (name + amount + email)
- 0.85-0.94: Two signals agree OR one very strong signal with confirmation
- 0.75-0.84: Single strong signal
- Below 0.70: Insufficient — set status to "unresolved"

Respond with ONLY the JSON object, nothing else.`

// --- Tool helpers ---

var allowedTriageTools = map[string]bool{
	"search_customers_by_name":     true,
	"search_customers_by_amount":   true,
	"search_remittance_emails":     true,
	"lookup_reference":             true,
	"get_customer_details":         true,
	"get_customer_invoices":        true,
	"get_customer_payment_history": true,
}

// --- Entry point ---

// RunFunnel orchestrates the Amount-Anchored Funnel pipeline:
// Phase 0: Deterministic pre-fetch — amount matches + remittance emails (0 LLM calls)
// Phase 1: Triage agent reviews pre-fetched data, decides match or requests more (1 LLM call, no tools)
// Phase 2a: If triage said "need_more", execute requested tools locally (0 LLM calls)
// Phase 2b: Resolver agent gets all data and makes final decision (1 LLM call, no tools)
func RunFunnel(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*FunnelResult, error) {
	result := &FunnelResult{}

	// ---- Phase 0: Deterministic pre-fetch (0 LLM calls) ----
	fmt.Println("  Phase 0: Deterministic pre-fetch (amount matches + remittance emails)...")

	preFetch, preFetchToolCalls := runPreFetch(txn, toolHandler, verbose)
	result.PreFetch = preFetch
	result.ToolCalls = preFetchToolCalls

	// ---- Phase 1: Triage agent (1 LLM call, no tools) ----
	fmt.Println("  Phase 1: Triage agent analyzing pre-fetched data...")

	triageInput := buildTriagePrompt(txn, preFetch)
	triageText, triageUsage, triageErr := callClaudeSimple("FunnelTriage", triagePrompt, triageInput, 1500, verbose)
	result.TotalUsage.Add(triageUsage)

	if triageErr != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Triage phase error: %v", triageErr),
			InvestigationSummary: "Funnel pipeline failed during triage phase.",
		}
		return result, triageErr
	}

	fmt.Printf("    Triage responded (%d in / %d out)\n", triageUsage.InputTokens, triageUsage.OutputTokens)

	decision, parseErr := parseTriageDecision(triageText)
	if parseErr != nil {
		fmt.Printf("    WARNING: Failed to parse triage response, escalating to resolver: %v\n", parseErr)
		decision = &TriageDecision{
			Decision:  "need_more",
			Reasoning: fmt.Sprintf("Triage response could not be parsed: %s", truncate(triageText, 200)),
		}
	}

	fmt.Printf("    Decision: %s — %s\n", decision.Decision, truncate(decision.Reasoning, 150))

	// ---- If triage matched, we're done ----
	if decision.Decision == "match" && decision.Result != nil {
		fmt.Println("    → Triage resolved the match directly (1 LLM call total)")
		result.Match = decision.Result
		result.Phase = "triage"
		return result, nil
	}

	// ---- Phase 2a: Execute requested tools locally (0 LLM calls) ----
	requestedTools := decision.NeededTools
	if len(requestedTools) == 0 {
		fmt.Println("    WARNING: Triage said 'need_more' but requested no tools — running default name search")
		requestedTools = buildDefaultFollowUp(txn)
	}

	fmt.Printf("  Phase 2a: Executing %d requested tool(s) locally...\n", len(requestedTools))

	followUpData := executeNeededTools(requestedTools, toolHandler, verbose)
	result.ToolCalls += len(requestedTools)

	for label, output := range followUpData {
		fmt.Printf("    [%s]: %s\n", label, truncate(output, 120))
	}

	// ---- Phase 2b: Resolver agent (1 LLM call, no tools) ----
	fmt.Println("  Phase 2b: Resolver agent making final decision with all data...")

	resolverInput := buildResolverPrompt(txn, preFetch, decision.Reasoning, followUpData)
	resolverText, resolverUsage, resolverErr := callClaudeSimple("FunnelResolver", resolverPrompt, resolverInput, 1500, verbose)
	result.TotalUsage.Add(resolverUsage)

	if resolverErr != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Resolver phase error: %v", resolverErr),
			InvestigationSummary: "Funnel pipeline failed during resolver phase.",
		}
		return result, resolverErr
	}

	fmt.Printf("    Resolver responded (%d in / %d out)\n", resolverUsage.InputTokens, resolverUsage.OutputTokens)

	match, matchErr := parseMatchResult(resolverText)
	if matchErr != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Failed to parse resolver response: %v", matchErr),
			InvestigationSummary: fmt.Sprintf("Funnel resolver returned unparseable output: %s", truncate(resolverText, 300)),
		}
		result.Phase = "resolver"
		return result, nil
	}

	result.Match = match
	result.Phase = "resolver"
	fmt.Println("    → Resolver produced final decision (2 LLM calls total)")

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

// --- Phase 2a: Execute requested tools ---

func executeNeededTools(needed []NeededTool, toolHandler *tools.ToolHandler, verbose bool) map[string]string {
	results := make(map[string]string, len(needed))

	for _, nt := range needed {
		if !allowedTriageTools[nt.Tool] {
			results[nt.Label] = fmt.Sprintf("Error: tool %q is not allowed in this pipeline", nt.Tool)
			if verbose {
				fmt.Printf("    ⚠ Blocked tool request: %s (not in allowed list)\n", nt.Tool)
			}
			continue
		}

		inputBytes, err := json.Marshal(nt.Input)
		if err != nil {
			results[nt.Label] = fmt.Sprintf("Error marshaling input: %v", err)
			continue
		}

		output, execErr := toolHandler.Execute(nt.Tool, inputBytes)
		if execErr != nil {
			results[nt.Label] = fmt.Sprintf("Error: %s", execErr.Error())
		} else {
			results[nt.Label] = output
		}

		if verbose {
			fmt.Printf("    ┌─ Follow-up tool: %s [%s]\n", nt.Tool, nt.Label)
			fmt.Printf("    │ Input:  %s\n", truncate(string(inputBytes), 150))
			fmt.Printf("    │ Result: %s\n", truncate(output, 200))
			fmt.Printf("    └──────────────────────────────────────────────────\n")
		}
	}

	return results
}

// --- Prompt builders ---

func buildTriagePrompt(txn models.BankTransaction, preFetch *PreFetchData) string {
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
	sb.WriteString("PRE-FETCHED DATA:\n")
	sb.WriteString("═══════════════════════════════════════\n")

	sb.WriteString("\n── AMOUNT MATCHES (customers with invoice combos matching $")
	sb.WriteString(fmt.Sprintf("%.2f ±3%%) ──\n", txn.Amount))
	sb.WriteString(preFetch.AmountMatches)

	sb.WriteString("\n\n── REMITTANCE EMAILS (matching amount ±5%, date ±7 days) ──\n")
	sb.WriteString(preFetch.EmailMatches)

	sb.WriteString("\n\n═══════════════════════════════════════\n")
	sb.WriteString("Based on the above, decide: can you match confidently, or do you need more data?\n")

	return sb.String()
}

func buildResolverPrompt(txn models.BankTransaction, preFetch *PreFetchData, triageReasoning string, followUpData map[string]string) string {
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
	sb.WriteString("ORIGINAL PRE-FETCHED DATA:\n")
	sb.WriteString("═══════════════════════════════════════\n")

	sb.WriteString("\n── AMOUNT MATCHES ──\n")
	sb.WriteString(preFetch.AmountMatches)

	sb.WriteString("\n\n── REMITTANCE EMAILS ──\n")
	sb.WriteString(preFetch.EmailMatches)

	sb.WriteString("\n\n═══════════════════════════════════════\n")
	sb.WriteString("TRIAGE AGENT'S ASSESSMENT:\n")
	sb.WriteString("═══════════════════════════════════════\n\n")
	sb.WriteString(triageReasoning)

	sb.WriteString("\n\n═══════════════════════════════════════\n")
	sb.WriteString("FOLLOW-UP DATA (requested by triage agent):\n")
	sb.WriteString("═══════════════════════════════════════\n")

	for label, output := range followUpData {
		sb.WriteString(fmt.Sprintf("\n── [%s] ──\n%s\n", label, output))
	}

	sb.WriteString("\n═══════════════════════════════════════\n\n")
	sb.WriteString("Using ALL the data above, make your final match decision.\n")

	return sb.String()
}

func buildDefaultFollowUp(txn models.BankTransaction) []NeededTool {
	var defaults []NeededTool

	nameInput, _ := json.Marshal(map[string]interface{}{
		"query": txn.CounterpartyName,
		"limit": 5,
	})
	defaults = append(defaults, NeededTool{
		Tool:  "search_customers_by_name",
		Input: nameInput,
		Label: "name_search",
	})

	if txn.BankReference != "" {
		refInput, _ := json.Marshal(map[string]interface{}{
			"reference": txn.BankReference,
		})
		defaults = append(defaults, NeededTool{
			Tool:  "lookup_reference",
			Input: refInput,
			Label: "ref_lookup",
		})
	}

	if txn.RemittanceInfo != "" {
		remInput, _ := json.Marshal(map[string]interface{}{
			"reference": txn.RemittanceInfo,
		})
		defaults = append(defaults, NeededTool{
			Tool:  "lookup_reference",
			Input: remInput,
			Label: "remittance_lookup",
		})
	}

	if txn.CounterpartyAccount != "" {
		accInput, _ := json.Marshal(map[string]interface{}{
			"reference": txn.CounterpartyAccount,
		})
		defaults = append(defaults, NeededTool{
			Tool:  "lookup_reference",
			Input: accInput,
			Label: "account_lookup",
		})
	}

	return defaults
}

// --- Parsers ---

func parseTriageDecision(text string) (*TriageDecision, error) {
	cleaned := strings.TrimSpace(text)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	var decision TriageDecision
	if err := json.Unmarshal([]byte(cleaned), &decision); err != nil {
		return nil, fmt.Errorf("parsing triage JSON: %w (raw: %s)", err, truncate(cleaned, 300))
	}

	if decision.Decision != "match" && decision.Decision != "need_more" {
		return nil, fmt.Errorf("unknown triage decision: %q", decision.Decision)
	}

	return &decision, nil
}

func parseMatchResult(text string) (*models.AgentMatchResult, error) {
	cleaned := strings.TrimSpace(text)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	var match models.AgentMatchResult
	if err := json.Unmarshal([]byte(cleaned), &match); err != nil {
		return nil, fmt.Errorf("parsing resolver JSON: %w (raw: %s)", err, truncate(cleaned, 300))
	}

	return &match, nil
}

// CalculateFunnelCost reuses the same Haiku pricing as other pipelines.
func CalculateFunnelCost(u TokenUsage) float64 {
	return agent.CalculateCost(u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens)
}
