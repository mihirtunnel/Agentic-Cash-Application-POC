package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"cashapp-agent-poc/internal/agent"
	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/tools"
)

// OrchestratorResult holds the full output of the orchestrator mode.
type OrchestratorResult struct {
	Match        *models.AgentMatchResult
	Plan         *InvestigationPlan
	Findings     []*SubagentFinding
	TotalUsage   TokenUsage
	ToolCalls    int
	SubagentLogs []SubagentLog
}

type SubagentLog struct {
	Name       string
	Candidates int
	Usage      TokenUsage
	Error      string
}

// InvestigationPlan is the structured output from the main agent's planning call.
type InvestigationPlan struct {
	SubAgents []PlannedSubagent `json:"sub_agents"`
	Reasoning string            `json:"reasoning"`
}

type PlannedSubagent struct {
	Name     string `json:"name"`
	Guidance string `json:"guidance"`
}

// SubagentFinding is the structured output each subagent produces.
type SubagentFinding struct {
	AgentName  string      `json:"agent_name"`
	Candidates []Candidate `json:"candidates"`
	RawText    string      `json:"raw_text"`
	Usage      TokenUsage  `json:"-"`
}

type Candidate struct {
	CustomerID   string   `json:"customer_id"`
	CustomerName string   `json:"customer_name"`
	Confidence   float64  `json:"confidence"`
	Evidence     string   `json:"evidence"`
	InvoiceIDs   []string `json:"invoice_ids,omitempty"`
}

// --- Tool used by the main agent to submit its investigation plan ---

var submitPlanTool = map[string]interface{}{
	"name":        "submit_plan",
	"description": "Submit your investigation plan. Specify which sub-agents to dispatch and what guidance each should follow.",
	"input_schema": map[string]interface{}{
		"type":     "object",
		"required": []string{"sub_agents", "reasoning"},
		"properties": map[string]interface{}{
			"sub_agents": map[string]interface{}{
				"type":        "array",
				"description": "List of sub-agents to dispatch. Each must have a name (NameAgent, AmountAgent, or EmailAgent) and guidance text.",
				"items": map[string]interface{}{
					"type":     "object",
					"required": []string{"name", "guidance"},
					"properties": map[string]interface{}{
						"name":     map[string]interface{}{"type": "string", "enum": []string{"NameAgent", "AmountAgent", "EmailAgent"}},
						"guidance": map[string]interface{}{"type": "string", "description": "Specific instructions for this sub-agent based on your analysis of the transaction."},
					},
				},
			},
			"reasoning": map[string]interface{}{
				"type":        "string",
				"description": "Explain why you chose these sub-agents and what you expect them to find.",
			},
		},
	},
}

// --- Tool used by each sub-agent to return findings ---

var submitFindingTool = map[string]interface{}{
	"name":        "submit_finding",
	"description": "Submit your investigation findings. List each candidate customer you found with evidence and confidence.",
	"input_schema": map[string]interface{}{
		"type":     "object",
		"required": []string{"candidates"},
		"properties": map[string]interface{}{
			"candidates": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type":     "object",
					"required": []string{"customer_id", "customer_name", "confidence", "evidence"},
					"properties": map[string]interface{}{
						"customer_id":   map[string]interface{}{"type": "string"},
						"customer_name": map[string]interface{}{"type": "string"},
						"confidence":    map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1},
						"evidence":      map[string]interface{}{"type": "string"},
						"invoice_ids":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
			},
		},
	},
}

// --- System prompts ---

var plannerPrompt = `You are the LEAD INVESTIGATOR for Tauber Oil Company's cash application system. Your job is to first check if a bank payment is an INTERNAL TRANSFER, then decide which specialist sub-agents to dispatch for customer matching.

CRITICAL: INTERNAL TRANSFER CHECK FIRST
Analyze the remittance info and counterparty data immediately:

INTERNAL TRANSFER INDICATORS:
- Remittance contains SENDER=TAUBER and BENE=TAUBER (or similar Tauber entities)
- Example: "SENDER=TAUBER OIL / BENE=TAUBER PETROCHEMICAL"
- Bank memo contains "TRANSFER FROM CHECKING ACCT" or similar ZBA routing language
- ZBA (Zero Balance Account) or inter-company account movement
- Scenario explicitly says "INTERNAL"
- No recognizable external company name

If this appears to be an INTERNAL TRANSFER:
- Dispatch agents to CONFIRM it's internal (they should all find no external customer match)
- Provide guidance: "This may be an internal Tauber transfer. If no external customer matches, recommend internal_transfer status."

Available sub-agents (for CUSTOMER MATCHING once internal transfer confirmed as false):
1. NameAgent — Searches by counterparty name and reference codes. Best when the bank text contains a recognizable company name, abbreviation, or reference code.
2. AmountAgent — Searches by payment amount and checks invoice combinations. Best when the amount is distinctive or could match specific invoice totals.
3. EmailAgent — Searches for remittance advice emails around the payment date. Best for larger payments where customers typically send remittance advice.

ANALYSIS & DISPATCH GUIDELINES:
- For possible internal transfers: Dispatch all 3 agents to confirm (they should find no external customer)
- For clear external payments: Use targeted sub-agent selection
- If the counterparty text is just a wire code or gibberish (no recognizable name), skip NameAgent.
- If the amount is a round number that could match many invoices, AmountAgent is still useful but lower confidence.
- If there's a bank reference or remittance info that looks like a code, include NameAgent to look it up.
- If the amount is very distinctive (odd number, large), AmountAgent is high priority.
- Always consider EmailAgent for payments above a few thousand dollars.
- For each sub-agent you dispatch, provide SPECIFIC guidance — don't just say "search", tell it WHAT to search for.

You MUST call submit_plan to send your investigation plan. Do not call any other tools.`

var nameAgentPrompt = `You are a NAME MATCHING specialist for Tauber Oil Company treasury. Given a bank payment and specific guidance from the lead investigator, find candidate customers by analyzing the counterparty name and any reference codes.

Use your tools to:
1. If the bank reference or counterparty text contains a code, look it up with lookup_reference.
2. Search for customers by name using search_customers_by_name.

For each candidate you find, assess how well the name matches and assign a confidence:
- 0.90+: Strong name match (exact or very close fuzzy match)
- 0.70-0.89: Partial name match (abbreviation, alias, or partial overlap)
- Below 0.70: Weak match

You MUST call submit_finding to return your results. Even if you find nothing, submit an empty candidates list.`

var amountAgentPrompt = `You are an AMOUNT MATCHING specialist for Tauber Oil Company treasury. Given a bank payment and specific guidance from the lead investigator, find candidate customers whose open invoices match the payment amount.

Use your tools to:
1. Search by amount using search_customers_by_amount with a 3% tolerance (to account for early-pay discounts).
2. For each candidate, get their invoices using get_customer_invoices to verify the match.

For each candidate you find, assess the amount match quality:
- 0.90+: Exact or near-exact match (within 0.5%)
- 0.80-0.89: Match within discount range (1-2%)
- 0.70-0.79: Match within tolerance but could be coincidence
- Below 0.70: Weak match

Include the specific invoice IDs that match in your findings.
You MUST call submit_finding to return your results.`

var emailAgentPrompt = `You are a REMITTANCE EMAIL specialist for Tauber Oil Company treasury. Given a bank payment and specific guidance from the lead investigator, search for remittance advice emails that match this payment.

Use your tools to:
1. Search remittance emails by amount range (payment amount ±5%) and date range (±7 days from payment date).
2. If the counterparty name is available, also search by sender keyword.

For each matching email, extract the customer it came from and the invoices referenced.
Assign confidence:
- 0.95+: Email explicitly references this exact payment amount and lists invoices
- 0.80-0.94: Email matches amount but date is a few days off
- 0.70-0.79: Partial match (amount close but not exact)

You MUST call submit_finding to return your results.`

var synthesizerPrompt = `You are the LEAD INVESTIGATOR making your FINAL DECISION for Tauber Oil Company's cash application system. Earlier, you analyzed a bank payment and dispatched specialist sub-agents. Now you have their findings.

CRITICAL: CHECK FOR INTERNAL TRANSFER FIRST
Before matching to a customer, review the agent findings for internal transfer patterns:

INTERNAL TRANSFER SIGNS FROM AGENT FINDINGS:
- NO agents found any valid external customer (all returned empty or low confidence < 0.70)
- Remittance text shows only Tauber entities (SENDER and BENE are both Tauber subsidiaries)
- All agents noted this looks like inter-company movement
- Name agent returned no external customer matches despite searching

If findings indicate this is an INTERNAL TRANSFER:
- Set status to "internal_transfer"
- Set confidence to 1.0
- DO NOT perform customer verification
- Submit with reasoning "No external customer found — internal Tauber entity transfer"

CUSTOMER MATCH DECISION (only if NOT internal transfer):
Your job is to:
1. Review all findings and identify which customer (if any) has the strongest combined evidence.
2. If multiple agents point to the SAME customer, that strongly increases confidence.
3. Use get_customer_details and get_customer_invoices to verify the best candidate.
4. Use get_customer_payment_history to confirm payment patterns.
5. Submit your final decision using submit_match_result.

CONFIDENCE GUIDELINES FOR CUSTOMER MATCHES:
- 0.95+: Two or more agents agree AND invoice amounts match
- 0.85-0.94: Two agents agree OR one agent has very strong evidence confirmed by verification
- 0.70-0.84: Only one agent found a match, partially verified
- Below 0.70: Insufficient evidence — submit as unresolved

RULES:
- Account for early-pay discounts (typically 1-2%)
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If the bank text doesn't resemble the customer name, suggest an alias in suggested_alias
- Do NOT guess. If evidence is insufficient, submit status "unresolved".
- You MUST call submit_match_result to end your investigation.`

// --- Sub-agent tool sets ---

func nameAgentTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	selected := filterTools(all, "search_customers_by_name", "lookup_reference")
	return append(selected, submitFindingTool)
}

func amountAgentTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	selected := filterTools(all, "search_customers_by_amount", "get_customer_invoices")
	return append(selected, submitFindingTool)
}

func emailAgentTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	selected := filterTools(all, "search_remittance_emails")
	return append(selected, submitFindingTool)
}

func synthesizerTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	return filterTools(all,
		"get_customer_details",
		"get_customer_invoices",
		"get_customer_payment_history",
		"submit_match_result",
	)
}

// --- Orchestrator entry point ---

// RunOrchestrator orchestrates the full multi-agent orchestrator flow for a single transaction:
// Phase 1: Main agent analyzes txn and decides which sub-agents to use
// Phase 2: Selected sub-agents run in parallel with guidance from Phase 1
// Phase 3: Main agent synthesizes findings and makes final decision
func RunOrchestrator(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*OrchestratorResult, error) {
	result := &OrchestratorResult{}

	// ---- Phase 1: Main agent plans the investigation ----
	fmt.Println("  Phase 1: Main agent analyzing transaction and planning investigation...")

	plan, planUsage, err := runPlanningPhase(txn, verbose)
	result.TotalUsage.Add(planUsage)

	if err != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Planning phase error: %v", err),
			InvestigationSummary: "Orchestrator failed during planning phase.",
		}
		return result, err
	}
	result.Plan = plan

	fmt.Printf("    Plan: dispatch %d sub-agent(s) — %s\n", len(plan.SubAgents), agentNames(plan.SubAgents))
	fmt.Printf("    Reasoning: %s\n", truncate(plan.Reasoning, 150))

	// ---- Phase 2: Run selected sub-agents in parallel ----
	fmt.Printf("  Phase 2: Running %d sub-agent(s) in parallel...\n", len(plan.SubAgents))

	findings, subagentLogs, subagentUsage := runSubagentPhase(plan, txn, toolHandler, verbose)
	result.Findings = findings
	result.SubagentLogs = subagentLogs
	result.TotalUsage.Add(subagentUsage)

	for _, log := range subagentLogs {
		if log.Error != "" {
			fmt.Printf("    %s ERROR: %s\n", log.Name, log.Error)
		} else {
			fmt.Printf("    %s found %d candidate(s)\n", log.Name, log.Candidates)
		}
	}

	// ---- Phase 3: Main agent synthesizes findings ----
	fmt.Println("  Phase 3: Main agent synthesizing findings and making decision...")

	synthInput := buildSynthesisPrompt(txn, plan, findings)

	submitInput, _, synthUsage, synthErr := runToolLoop(
		"OrchestratorMain",
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
			InvestigationSummary: "Orchestrator failed during synthesis phase.",
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
			Reasoning:            "Main agent did not submit a result after synthesis.",
			InvestigationSummary: "Orchestrator: main agent did not produce output.",
		}
	}

	return result, nil
}

// --- Phase 1: Planning ---

func runPlanningPhase(txn models.BankTransaction, verbose bool) (*InvestigationPlan, TokenUsage, error) {
	prompt := buildTxnPrompt(txn)
	planTools := []map[string]interface{}{submitPlanTool}

	submitInput, _, usage, err := runToolLoop(
		"OrchestratorPlanner",
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

// --- Phase 2: Sub-agents ---

func runSubagentPhase(plan *InvestigationPlan, txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) ([]*SubagentFinding, []SubagentLog, TokenUsage) {
	type subagentConfig struct {
		name     string
		prompt   string
		tools    []map[string]interface{}
		guidance string
	}

	configMap := map[string]subagentConfig{
		"NameAgent": {
			name:   "NameAgent",
			prompt: nameAgentPrompt,
			tools:  nameAgentTools(),
		},
		"AmountAgent": {
			name:   "AmountAgent",
			prompt: amountAgentPrompt,
			tools:  amountAgentTools(),
		},
		"EmailAgent": {
			name:   "EmailAgent",
			prompt: emailAgentPrompt,
			tools:  emailAgentTools(),
		},
	}

	var configs []subagentConfig
	for _, planned := range plan.SubAgents {
		if cfg, ok := configMap[planned.Name]; ok {
			cfg.guidance = planned.Guidance
			configs = append(configs, cfg)
		} else {
			fmt.Printf("    WARNING: Unknown sub-agent '%s' in plan, skipping.\n", planned.Name)
		}
	}

	var wg sync.WaitGroup
	findings := make([]*SubagentFinding, len(configs))
	errs := make([]error, len(configs))

	for i, cfg := range configs {
		wg.Add(1)
		go func(idx int, c subagentConfig) {
			defer wg.Done()
			if verbose {
				fmt.Printf("    Starting %s...\n", c.name)
			}
			f, err := runSubagent(c.name, c.prompt, c.tools, c.guidance, txn, toolHandler, verbose)
			findings[idx] = f
			errs[idx] = err
		}(i, cfg)
	}
	wg.Wait()

	var allFindings []*SubagentFinding
	var logs []SubagentLog
	var totalUsage TokenUsage

	for i, cfg := range configs {
		log := SubagentLog{Name: cfg.name}
		if findings[i] != nil {
			log.Usage = findings[i].Usage
			log.Candidates = len(findings[i].Candidates)
			allFindings = append(allFindings, findings[i])
			totalUsage.Add(findings[i].Usage)
		}
		if errs[i] != nil {
			log.Error = errs[i].Error()
		} else if verbose && findings[i] != nil {
			for _, c := range findings[i].Candidates {
				fmt.Printf("      → %s (%s) confidence=%.2f  evidence: %s\n",
					c.CustomerName, c.CustomerID, c.Confidence, truncate(c.Evidence, 100))
			}
		}
		logs = append(logs, log)
	}

	return allFindings, logs, totalUsage
}

func runSubagent(name string, systemPrompt string, agentTools []map[string]interface{}, guidance string, txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*SubagentFinding, error) {
	prompt := buildTxnPrompt(txn)
	if guidance != "" {
		prompt += fmt.Sprintf("\n\nGUIDANCE FROM LEAD INVESTIGATOR:\n%s", guidance)
	}

	submitInput, agentText, usage, err := runToolLoop(
		name,
		systemPrompt,
		agentTools,
		prompt,
		3,
		toolHandler.Execute,
		"submit_finding",
		verbose,
	)

	finding := &SubagentFinding{
		AgentName: name,
		RawText:   agentText,
		Usage:     usage,
	}

	if err != nil {
		return finding, err
	}

	if submitInput != nil {
		var result struct {
			Candidates []Candidate `json:"candidates"`
		}
		if parseErr := json.Unmarshal(submitInput, &result); parseErr != nil {
			return finding, fmt.Errorf("parsing %s submit_finding: %w", name, parseErr)
		}
		finding.Candidates = result.Candidates
	}

	return finding, nil
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

func buildSynthesisPrompt(txn models.BankTransaction, plan *InvestigationPlan, findings []*SubagentFinding) string {
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
	sb.WriteString("YOUR EARLIER INVESTIGATION PLAN:\n")
	sb.WriteString("═══════════════════════════════════════\n\n")
	sb.WriteString(fmt.Sprintf("Reasoning: %s\n", plan.Reasoning))
	for _, sa := range plan.SubAgents {
		sb.WriteString(fmt.Sprintf("  → %s: %s\n", sa.Name, sa.Guidance))
	}

	sb.WriteString("\n═══════════════════════════════════════\n")
	sb.WriteString("SUB-AGENT FINDINGS:\n")
	sb.WriteString("═══════════════════════════════════════\n\n")

	for _, f := range findings {
		if f == nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("── %s ──\n", f.AgentName))
		if len(f.Candidates) == 0 {
			sb.WriteString("  No candidates found.\n\n")
			continue
		}
		for i, c := range f.Candidates {
			sb.WriteString(fmt.Sprintf("  Candidate %d: %s (%s)\n", i+1, c.CustomerName, c.CustomerID))
			sb.WriteString(fmt.Sprintf("    Confidence: %.2f\n", c.Confidence))
			sb.WriteString(fmt.Sprintf("    Evidence: %s\n", c.Evidence))
			if len(c.InvoiceIDs) > 0 {
				sb.WriteString(fmt.Sprintf("    Invoice IDs: %s\n", strings.Join(c.InvoiceIDs, ", ")))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("═══════════════════════════════════════\n\n")
	sb.WriteString("Based on these findings, verify the strongest candidate and submit your final match result.\n")
	sb.WriteString("Use get_customer_details, get_customer_invoices, and get_customer_payment_history to confirm before submitting.")

	return sb.String()
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

func agentNames(agents []PlannedSubagent) string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

// CalculateOrchestratorCost reuses the same Haiku pricing as the single agent.
func CalculateOrchestratorCost(u TokenUsage) float64 {
	return agent.CalculateCost(u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens)
}
