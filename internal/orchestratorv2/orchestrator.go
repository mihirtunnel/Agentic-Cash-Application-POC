package orchestratorv2

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"cashapp-agent-poc/internal/agent"
	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/tools"
)

// OrchestratorV2Result holds the full output of the orchestrator-v2 mode.
type OrchestratorV2Result struct {
	Match        *models.AgentMatchResult
	Plan         *InvestigationPlan
	TotalUsage   TokenUsage
	SubagentLogs []SubagentLog
}

type SubagentLog struct {
	Name  string
	Usage TokenUsage
	Error string
}

// InvestigationPlan is what the orchestrator returns: which tools to call,
// which sub-agents to dispatch, and what data each sub-agent should receive.
type InvestigationPlan struct {
	ToolCalls []PlannedToolCall `json:"tool_calls"`
	SubAgents []PlannedSubagent `json:"sub_agents"`
	Reasoning string            `json:"reasoning"`
}

type PlannedToolCall struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	Label string          `json:"label"`
}

type PlannedSubagent struct {
	Name     string   `json:"name"`
	FeedData []string `json:"feed_data"`
	Guidance string   `json:"guidance"`
}

// AnalystOpinion is the structured JSON each analyst sub-agent returns.
type AnalystOpinion struct {
	CustomerID   string   `json:"customer_id"`
	CustomerName string   `json:"customer_name"`
	Confidence   float64  `json:"confidence"`
	Evidence     string   `json:"evidence"`
	InvoiceIDs   []string `json:"invoice_ids,omitempty"`
	NoMatch      bool     `json:"no_match"`
}

// --- Tool: orchestrator submits its plan ---

var submitPlanTool = map[string]interface{}{
	"name":        "submit_plan",
	"description": "Submit your investigation plan. Specify which tools to call to gather data, and which analyst sub-agents should receive the results.",
	"input_schema": map[string]interface{}{
		"type":     "object",
		"required": []string{"tool_calls", "sub_agents", "reasoning"},
		"properties": map[string]interface{}{
			"tool_calls": map[string]interface{}{
				"type":        "array",
				"description": "Tools to execute locally to gather data. Each gets a label so you can reference it in sub-agent feed_data.",
				"items": map[string]interface{}{
					"type":     "object",
					"required": []string{"tool", "input", "label"},
					"properties": map[string]interface{}{
						"tool":  map[string]interface{}{"type": "string", "enum": []string{"search_customers_by_name", "search_customers_by_amount", "search_remittance_emails", "lookup_reference", "get_customer_invoices", "get_customer_details"}},
						"input": map[string]interface{}{"type": "object", "description": "The input parameters for the tool, matching the tool's schema."},
						"label": map[string]interface{}{"type": "string", "description": "A short label for this data (e.g., 'name_search', 'amount_search'). Used in feed_data."},
					},
				},
			},
			"sub_agents": map[string]interface{}{
				"type":        "array",
				"description": "Analyst sub-agents to dispatch. Each receives specific pre-fetched data and gives an opinion.",
				"items": map[string]interface{}{
					"type":     "object",
					"required": []string{"name", "feed_data", "guidance"},
					"properties": map[string]interface{}{
						"name":      map[string]interface{}{"type": "string", "enum": []string{"NameAnalyst", "AmountAnalyst", "EmailAnalyst"}},
						"feed_data": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Labels of tool_calls whose results this sub-agent should receive."},
						"guidance":  map[string]interface{}{"type": "string", "description": "What this analyst should focus on when interpreting the data."},
					},
				},
			},
			"reasoning": map[string]interface{}{
				"type":        "string",
				"description": "Explain your investigation strategy.",
			},
		},
	},
}

// --- System prompts ---

var plannerPrompt = `You are the LEAD INVESTIGATOR for Tauber Oil Company's cash application system. Your job is to analyze an incoming bank payment and create a data-gathering plan.

You have access to these data tools (you don't call them directly — you tell the system which to run):
- search_customers_by_name: Search customers by name. Input: {"query": "name", "limit": 5}
- search_customers_by_amount: Find customers whose invoices match an amount. Input: {"amount": 45000, "tolerance_percent": 3}
- search_remittance_emails: Search remittance emails. Input: {"amount_min": N, "amount_max": N, "date_from": "YYYY-MM-DD", "date_to": "YYYY-MM-DD", "sender_keyword": "optional"}
- lookup_reference: Look up a reference code. Input: {"reference": "code"}
- get_customer_invoices: Get invoices for a customer. Input: {"customer_id": "CUST-XXXX"}
- get_customer_details: Get customer profile. Input: {"customer_id": "CUST-XXXX"}

You also have 3 analyst sub-agents (they have NO tools — they only analyze data you feed them):
- NameAnalyst: Expert at interpreting name/reference search results
- AmountAnalyst: Expert at interpreting amount matches and invoice combinations
- EmailAnalyst: Expert at interpreting remittance email matches

YOUR TASK:
1. Analyze the transaction (counterparty text, amount, references, date)
2. Decide which tools to call and with what parameters — give each a short label
3. Decide which analysts to use and which tool results (by label) to feed each one
4. Provide guidance to each analyst about what to look for

GUIDELINES:
- If counterparty text is gibberish/wire code, skip name search and NameAnalyst
- If there's a bank reference or remittance info with a code, use lookup_reference
- Always use search_customers_by_amount (amounts are usually the strongest signal)
- For emails: set amount_min to payment * 0.95, amount_max to payment * 1.05, date range ±7 days
- You can chain tools: e.g., search by amount first, then get_customer_invoices for each result. Use labels like "amount_search" and "invoices_cust1" etc.
- Be specific with guidance — tell each analyst exactly what to look for
- CROSS-FEED DATA: Give each analyst ALL relevant data, not just their specialty. For example, feed name_search results to the EmailAnalyst too so it can map email senders to known customer IDs. This prevents analysts from guessing customer IDs they haven't seen.
- Analysts have NO tools and CANNOT look up data themselves. If they need a customer ID, it must be in the data you feed them.

You MUST call submit_plan. Do not call any other tools.`

var nameAnalystPrompt = `You are a NAME MATCHING analyst for Tauber Oil Company treasury. You receive pre-fetched search results and must give your opinion on which customer (if any) matches the payment.

You have NO tools. Just analyze the data provided and respond with a JSON object:
{
  "customer_id": "CUST-XXXX or empty",
  "customer_name": "name or empty",
  "confidence": 0.0 to 1.0,
  "evidence": "explain your reasoning",
  "invoice_ids": ["INV-XXX", ...] (if known),
  "no_match": true/false
}

CRITICAL RULES:
- ONLY use customer IDs (CUST-XXXX) and invoice IDs (INV-XXX) that appear in the pre-fetched data. NEVER invent or guess an ID.
- If the data does not contain an explicit customer ID, leave customer_id as an empty string.

CONFIDENCE GUIDELINES:
- 0.90+: Strong name match (exact or very close fuzzy match)
- 0.70-0.89: Partial name match (abbreviation, alias, or partial overlap)
- Below 0.70: Weak match — set no_match to true

Respond with ONLY the JSON object, nothing else.`

var amountAnalystPrompt = `You are an AMOUNT MATCHING analyst for Tauber Oil Company treasury. You receive pre-fetched amount search results and invoice details, and must give your opinion on which customer (if any) matches the payment.

You have NO tools. Just analyze the data provided and respond with a JSON object:
{
  "customer_id": "CUST-XXXX or empty",
  "customer_name": "name or empty",
  "confidence": 0.0 to 1.0,
  "evidence": "explain your reasoning",
  "invoice_ids": ["INV-XXX", ...],
  "no_match": true/false
}

CRITICAL RULES:
- ONLY use customer IDs (CUST-XXXX) and invoice IDs (INV-XXX) that appear in the pre-fetched data. NEVER invent or guess an ID.
- If the data does not contain an explicit customer ID, leave customer_id as an empty string.

CONFIDENCE GUIDELINES:
- 0.90+: Exact or near-exact amount match (within 0.5%), with invoice IDs confirmed
- 0.80-0.89: Match within early-pay discount range (1-2%)
- 0.70-0.79: Match within tolerance but could be coincidence
- Below 0.70: Weak match — set no_match to true

Account for early-pay discounts (typically 1-2% of invoice amount).
Respond with ONLY the JSON object, nothing else.`

var emailAnalystPrompt = `You are a REMITTANCE EMAIL analyst for Tauber Oil Company treasury. You receive pre-fetched email search results and must give your opinion on which customer (if any) sent this payment.

You have NO tools. Just analyze the data provided and respond with a JSON object:
{
  "customer_id": "CUST-XXXX or empty",
  "customer_name": "name or empty",
  "confidence": 0.0 to 1.0,
  "evidence": "explain your reasoning",
  "invoice_ids": ["INV-XXX", ...],
  "no_match": true/false
}

CRITICAL RULES:
- ONLY use customer IDs (CUST-XXXX) and invoice IDs (INV-XXX) that appear in the pre-fetched data. NEVER invent or guess an ID.
- If the email data does not contain an explicit customer ID (like CUST-XXXX), leave customer_id as an empty string. You may still provide the customer_name from the email sender.

CONFIDENCE GUIDELINES:
- 0.95+: Email explicitly references this exact payment amount and lists invoices
- 0.80-0.94: Email matches amount but date is slightly off
- 0.70-0.79: Partial match
- Below 0.70: Weak match — set no_match to true

Respond with ONLY the JSON object, nothing else.`

var synthesizerPrompt = `You are the LEAD INVESTIGATOR making your FINAL DECISION for Tauber Oil Company's cash application system. You planned the investigation, gathered data, and received analyst opinions. Now make your decision.

Your job is to:
1. Review analyst opinions and identify which customer has the strongest evidence
2. If multiple analysts point to the SAME customer, confidence increases
3. ONLY verify a customer if the analyst-provided customer_id looks valid (format CUST-XXXX and present in the analyst data). If an analyst opinion has customer_id marked as "[UNVERIFIED]" or empty, do NOT call get_customer_details for it — instead rely on other analysts or look up the customer by name/amount.
4. Aim to verify only ONE candidate (the strongest) to minimize tool calls
5. Submit your final decision using submit_match_result

CONFIDENCE GUIDELINES:
- 0.95+: Two or more analysts agree AND invoice amounts match
- 0.85-0.94: Two analysts agree OR one has very strong evidence
- 0.70-0.84: Only one analyst found a match
- Below 0.70: Insufficient evidence — submit as unresolved

RULES:
- Account for early-pay discounts (typically 1-2%)
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If the bank text doesn't resemble the customer name, suggest an alias in suggested_alias
- Do NOT guess. If evidence is insufficient, submit status "unresolved".
- MINIMIZE TOOL CALLS: Only call verification tools for the single most likely candidate. Do not verify every analyst opinion separately.
- You MUST call submit_match_result to end your investigation.`

// --- Synthesizer tool set (only verification + submit) ---

func synthesizerTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	return filterTools(all,
		"get_customer_details",
		"get_customer_invoices",
		"get_customer_payment_history",
		"submit_match_result",
	)
}

// --- Entry point ---

// RunOrchestratorV2 orchestrates the improved pipeline:
// Phase 1: Orchestrator plans tool calls + sub-agent assignments (1 LLM call)
// Phase 1b: Execute planned tools locally (0 LLM calls)
// Phase 2: Analyst sub-agents receive pre-fetched data, give opinions (1 LLM call each, no tools)
// Phase 3: Orchestrator synthesizes opinions + verifies (1-2 LLM calls with verification tools)
func RunOrchestratorV2(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*OrchestratorV2Result, error) {
	result := &OrchestratorV2Result{}

	// ---- Phase 1: Orchestrator plans the investigation ----
	fmt.Println("  Phase 1: Orchestrator analyzing transaction and planning data gathering...")

	plan, planUsage, err := runPlanningPhase(txn, verbose)
	result.TotalUsage.Add(planUsage)

	if err != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Planning phase error: %v", err),
			InvestigationSummary: "OrchestratorV2 failed during planning phase.",
		}
		return result, err
	}
	result.Plan = plan

	fmt.Printf("    Plan: %d tool call(s), %d analyst(s) — %s\n",
		len(plan.ToolCalls), len(plan.SubAgents), analystNames(plan.SubAgents))
	fmt.Printf("    Reasoning: %s\n", truncate(plan.Reasoning, 150))

	// ---- Phase 1b: Execute planned tools locally (no LLM calls) ----
	fmt.Printf("  Phase 1b: Executing %d tool call(s) locally...\n", len(plan.ToolCalls))

	toolResults := executeToolCalls(plan.ToolCalls, toolHandler, verbose)

	for label, output := range toolResults {
		fmt.Printf("    [%s]: %s\n", label, truncate(output, 120))
	}

	// ---- Phase 2: Run analyst sub-agents in parallel (1 call each, no tools) ----
	fmt.Printf("  Phase 2: Running %d analyst(s) in parallel (no tools, data only)...\n", len(plan.SubAgents))

	opinions, subagentLogs, subagentUsage := runAnalystPhase(plan, txn, toolResults, verbose)
	result.SubagentLogs = subagentLogs
	result.TotalUsage.Add(subagentUsage)

	for _, log := range subagentLogs {
		if log.Error != "" {
			fmt.Printf("    %s ERROR: %s\n", log.Name, log.Error)
		} else {
			fmt.Printf("    %s responded (%d in / %d out)\n", log.Name, log.Usage.InputTokens, log.Usage.OutputTokens)
		}
	}

	// ---- Phase 2b: Validate analyst opinions against actual data ----
	validateAnalystOpinions(opinions, toolResults, verbose)

	// ---- Phase 3: Orchestrator synthesizes opinions ----
	fmt.Println("  Phase 3: Orchestrator synthesizing analyst opinions and making decision...")

	synthInput := buildSynthesisPrompt(txn, plan, opinions)

	submitInput, _, synthUsage, synthErr := runToolLoop(
		"V2Synthesizer",
		synthesizerPrompt,
		synthesizerTools(),
		synthInput,
		4,
		toolHandler.Execute,
		"submit_match_result",
		verbose,
	)
	result.TotalUsage.Add(synthUsage)

	if synthErr != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Synthesis phase error: %v", synthErr),
			InvestigationSummary: "OrchestratorV2 failed during synthesis phase.",
		}
		return result, synthErr
	}

	if submitInput != nil {
		var match models.AgentMatchResult
		if parseErr := json.Unmarshal(submitInput, &match); parseErr != nil {
			return result, fmt.Errorf("parsing synthesis submit_match_result: %w", parseErr)
		}
		result.Match = &match
	} else {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            "Orchestrator did not submit a result after synthesis.",
			InvestigationSummary: "OrchestratorV2: synthesizer did not produce output.",
		}
	}

	return result, nil
}

// --- Phase 1: Planning ---

func runPlanningPhase(txn models.BankTransaction, verbose bool) (*InvestigationPlan, TokenUsage, error) {
	prompt := buildTxnPrompt(txn)
	planTools := []map[string]interface{}{submitPlanTool}

	submitInput, _, usage, err := runToolLoop(
		"V2Planner",
		plannerPrompt,
		planTools,
		prompt,
		2,
		func(name string, input json.RawMessage) (string, error) {
			return "", fmt.Errorf("planner should only call submit_plan, got: %s", name)
		},
		"submit_plan",
		verbose,
	)
	if err != nil {
		return nil, usage, err
	}

	if submitInput == nil {
		return nil, usage, fmt.Errorf("planner did not call submit_plan")
	}

	var plan InvestigationPlan
	if err := json.Unmarshal(submitInput, &plan); err != nil {
		return nil, usage, fmt.Errorf("parsing investigation plan: %w", err)
	}

	if len(plan.SubAgents) == 0 {
		return nil, usage, fmt.Errorf("planner returned empty sub-agent list")
	}

	return &plan, usage, nil
}

// --- Phase 1b: Local tool execution ---

func executeToolCalls(calls []PlannedToolCall, toolHandler *tools.ToolHandler, verbose bool) map[string]string {
	results := make(map[string]string, len(calls))

	for _, call := range calls {
		inputBytes, err := json.Marshal(call.Input)
		if err != nil {
			results[call.Label] = fmt.Sprintf("Error marshaling input: %v", err)
			continue
		}

		output, execErr := toolHandler.Execute(call.Tool, inputBytes)
		if execErr != nil {
			results[call.Label] = fmt.Sprintf("Error: %s", execErr.Error())
		} else {
			results[call.Label] = output
		}

		if verbose {
			fmt.Printf("    ┌─ Local tool: %s [%s]\n", call.Tool, call.Label)
			fmt.Printf("    │ Input:  %s\n", truncate(string(inputBytes), 150))
			fmt.Printf("    │ Result: %s\n", truncate(output, 200))
			fmt.Printf("    └──────────────────────────────────────────────────\n")
		}
	}

	return results
}

// --- Phase 2: Analyst sub-agents (no tools, single LLM call each) ---

func runAnalystPhase(plan *InvestigationPlan, txn models.BankTransaction, toolResults map[string]string, verbose bool) (map[string]*AnalystOpinion, []SubagentLog, TokenUsage) {
	promptMap := map[string]string{
		"NameAnalyst":   nameAnalystPrompt,
		"AmountAnalyst": amountAnalystPrompt,
		"EmailAnalyst":  emailAnalystPrompt,
	}

	type analystJob struct {
		name     string
		prompt   string
		guidance string
		data     string
	}

	var jobs []analystJob
	for _, sa := range plan.SubAgents {
		sysPrompt, ok := promptMap[sa.Name]
		if !ok {
			fmt.Printf("    WARNING: Unknown analyst '%s', skipping.\n", sa.Name)
			continue
		}

		var dataBuilder strings.Builder
		dataBuilder.WriteString(buildTxnPrompt(txn))
		dataBuilder.WriteString("\nGUIDANCE FROM LEAD INVESTIGATOR:\n")
		dataBuilder.WriteString(sa.Guidance)
		dataBuilder.WriteString("\n\nPRE-FETCHED DATA:\n")
		dataBuilder.WriteString("═══════════════════════════════════════\n")

		for _, label := range sa.FeedData {
			if result, exists := toolResults[label]; exists {
				dataBuilder.WriteString(fmt.Sprintf("\n── [%s] ──\n%s\n", label, result))
			} else {
				dataBuilder.WriteString(fmt.Sprintf("\n── [%s] ──\n(no data — tool call may have been skipped)\n", label))
			}
		}

		jobs = append(jobs, analystJob{
			name:   sa.Name,
			prompt: sysPrompt,
			data:   dataBuilder.String(),
		})
	}

	var wg sync.WaitGroup
	opinions := make([]*AnalystOpinion, len(jobs))
	texts := make([]string, len(jobs))
	usages := make([]TokenUsage, len(jobs))
	errs := make([]error, len(jobs))

	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, j analystJob) {
			defer wg.Done()
			if verbose {
				fmt.Printf("    Starting %s (no tools, data-only)...\n", j.name)
			}
			text, usage, err := callClaudeSimple(j.name, j.prompt, j.data, 1024, verbose)
			texts[idx] = text
			usages[idx] = usage
			errs[idx] = err

			if err == nil {
				var opinion AnalystOpinion
				cleaned := strings.TrimSpace(text)
				cleaned = strings.TrimPrefix(cleaned, "```json")
				cleaned = strings.TrimPrefix(cleaned, "```")
				cleaned = strings.TrimSuffix(cleaned, "```")
				cleaned = strings.TrimSpace(cleaned)
				if parseErr := json.Unmarshal([]byte(cleaned), &opinion); parseErr == nil {
					opinions[idx] = &opinion
				} else {
					opinions[idx] = &AnalystOpinion{
						NoMatch:  true,
						Evidence: fmt.Sprintf("Failed to parse analyst response: %s", truncate(text, 200)),
					}
				}
			}
		}(i, job)
	}
	wg.Wait()

	opinionMap := make(map[string]*AnalystOpinion)
	var logs []SubagentLog
	var totalUsage TokenUsage

	for i, job := range jobs {
		log := SubagentLog{Name: job.name, Usage: usages[i]}
		totalUsage.Add(usages[i])

		if errs[i] != nil {
			log.Error = errs[i].Error()
		} else if opinions[i] != nil && verbose {
			if opinions[i].NoMatch {
				fmt.Printf("      → %s: no match\n", job.name)
			} else {
				fmt.Printf("      → %s: %s (%s) confidence=%.2f\n",
					job.name, opinions[i].CustomerName, opinions[i].CustomerID, opinions[i].Confidence)
			}
		}

		opinionMap[job.name] = opinions[i]
		logs = append(logs, log)
	}

	return opinionMap, logs, totalUsage
}

// --- Prompt builders ---

func buildTxnPrompt(txn models.BankTransaction) string {
	prompt := fmt.Sprintf("INVESTIGATE THIS PAYMENT:\n\n"+
		"  Transaction ID: %s\n"+
		"  Date: %s\n"+
		"  Amount: $%.2f\n"+
		"  Direction: %s\n"+
		"  Counterparty text: \"%s\"\n",
		txn.TxnID, txn.Date, txn.Amount, txn.Direction, txn.CounterpartyName)

	if txn.CounterpartyAccount != "" {
		prompt += fmt.Sprintf("  Counterparty account: \"%s\"\n", txn.CounterpartyAccount)
	}
	if txn.BankReference != "" {
		prompt += fmt.Sprintf("  Bank reference: \"%s\"\n", txn.BankReference)
	}
	if txn.RemittanceInfo != "" {
		prompt += fmt.Sprintf("  Remittance info: \"%s\"\n", txn.RemittanceInfo)
	}
	return prompt
}

func buildSynthesisPrompt(txn models.BankTransaction, plan *InvestigationPlan, opinions map[string]*AnalystOpinion) string {
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
	sb.WriteString("YOUR INVESTIGATION PLAN:\n")
	sb.WriteString("═══════════════════════════════════════\n\n")
	sb.WriteString(fmt.Sprintf("Strategy: %s\n", plan.Reasoning))

	sb.WriteString("\n═══════════════════════════════════════\n")
	sb.WriteString("ANALYST OPINIONS (summarized):\n")
	sb.WriteString("═══════════════════════════════════════\n\n")

	for _, sa := range plan.SubAgents {
		sb.WriteString(fmt.Sprintf("── %s ──\n", sa.Name))
		opinion, exists := opinions[sa.Name]
		if !exists || opinion == nil {
			sb.WriteString("  (no response)\n\n")
			continue
		}
		if opinion.NoMatch {
			sb.WriteString("  No match found.\n")
			sb.WriteString(fmt.Sprintf("  Notes: %s\n\n", opinion.Evidence))
			continue
		}
		sb.WriteString(fmt.Sprintf("  Customer: %s (%s)\n", opinion.CustomerName, opinion.CustomerID))
		sb.WriteString(fmt.Sprintf("  Confidence: %.2f\n", opinion.Confidence))
		sb.WriteString(fmt.Sprintf("  Evidence: %s\n", opinion.Evidence))
		if len(opinion.InvoiceIDs) > 0 {
			sb.WriteString(fmt.Sprintf("  Invoice IDs: %s\n", strings.Join(opinion.InvoiceIDs, ", ")))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("═══════════════════════════════════════\n\n")
	sb.WriteString("Based on analyst opinions, verify the strongest candidate and submit your final match result.\n")
	sb.WriteString("Use get_customer_details, get_customer_invoices, and get_customer_payment_history to confirm before submitting.")

	return sb.String()
}

// --- Phase 2b: Validate analyst opinions ---

var custIDPattern = regexp.MustCompile(`CUST-[A-Z0-9]+`)

// validateAnalystOpinions checks each analyst's customer_id against IDs
// that actually appeared in tool results. If an analyst returned a customer_id
// that doesn't exist in the fetched data, it's likely hallucinated — we clear
// it and mark the evidence so the synthesizer doesn't waste a tool call on it.
func validateAnalystOpinions(opinions map[string]*AnalystOpinion, toolResults map[string]string, verbose bool) {
	knownIDs := make(map[string]bool)
	for _, output := range toolResults {
		for _, id := range custIDPattern.FindAllString(output, -1) {
			knownIDs[id] = true
		}
	}

	for name, opinion := range opinions {
		if opinion == nil || opinion.NoMatch || opinion.CustomerID == "" {
			continue
		}
		if !knownIDs[opinion.CustomerID] {
			if verbose {
				fmt.Printf("    ⚠ %s provided customer_id=%q which was NOT found in tool results — marking as [UNVERIFIED]\n",
					name, opinion.CustomerID)
			}
			opinion.Evidence = fmt.Sprintf("[UNVERIFIED customer_id %s — not found in fetched data] %s", opinion.CustomerID, opinion.Evidence)
			opinion.CustomerID = ""
		}
	}
}

// --- Helpers ---

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

func analystNames(agents []PlannedSubagent) string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

// CalculateV2Cost reuses the same Haiku pricing as the single agent.
func CalculateV2Cost(u TokenUsage) float64 {
	return agent.CalculateCost(u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens)
}
