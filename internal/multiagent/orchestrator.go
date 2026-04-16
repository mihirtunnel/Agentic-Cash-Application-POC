package multiagent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"cashapp-agent-poc/internal/agent"
	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/tools"
)

// MultiAgentResult holds the full output of the multi-agent pipeline.
type MultiAgentResult struct {
	Match        *models.AgentMatchResult
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

var mergerPrompt = `You are the FINAL DECISION MAKER for Tauber Oil Company's cash application system. You receive findings from three specialist agents who investigated the same bank payment:

1. NAME AGENT — searched by counterparty name and reference codes
2. AMOUNT AGENT — searched by payment amount and invoice combinations  
3. EMAIL AGENT — searched for remittance advice emails

CRITICAL: CHECK FOR INTERNAL TRANSFER FIRST
Before making a customer match decision, analyze all agent findings for internal transfer indicators:

INTERNAL TRANSFER SIGNS (from agent findings):
- Both agents agree it's internal (e.g., "Tauber" to "Tauber Petrochemical")
- Agent remittance text shows SENDER and BENE are both Tauber entities
- Bank memo contains "TRANSFER FROM CHECKING ACCT" or similar ZBA routing with no external customer
- No agent identified a valid external customer despite searching
- References indicate ZBA or inter-company movement

If this is clearly an INTERNAL TRANSFER:
- Set status to "internal_transfer"
- Set confidence to 1.0
- Do NOT perform customer verification
- Submit immediately with reasoning "Internal transfer between Tauber entities"

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
- If the bank counterparty/wire name differs from the matched customer's registered name, optionally set suggested_alias to that exact bank text (audit only; not from a lookup table)
- Do NOT guess. If evidence is insufficient, submit status "unresolved".
- You MUST call submit_match_result to end your investigation.`

func mergerTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	return filterTools(all,
		"get_customer_details",
		"get_customer_invoices",
		"get_customer_payment_history",
		"submit_match_result",
	)
}

// RunMultiAgent orchestrates the full multi-agent pipeline for a single transaction.
func RunMultiAgent(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*MultiAgentResult, error) {
	result := &MultiAgentResult{}

	// ---- Phase 1: Run 3 subagents in parallel ----

	type subagentConfig struct {
		name   string
		prompt string
		tools  []map[string]interface{}
	}

	configs := []subagentConfig{
		{"NameAgent", nameAgentPrompt, nameAgentTools()},
		{"AmountAgent", amountAgentPrompt, amountAgentTools()},
		{"EmailAgent", emailAgentPrompt, emailAgentTools()},
	}

	fmt.Println("  Phase 1: Running 3 subagents in parallel...")

	var wg sync.WaitGroup
	findings := make([]*SubagentFinding, len(configs))
	errors := make([]error, len(configs))

	for i, cfg := range configs {
		wg.Add(1)
		go func(idx int, c subagentConfig) {
			defer wg.Done()
			if verbose {
				fmt.Printf("    Starting %s...\n", c.name)
			}
			f, err := RunSubagent(c.name, c.prompt, c.tools, txn, toolHandler, verbose)
			findings[idx] = f
			errors[idx] = err
		}(i, cfg)
	}
	wg.Wait()

	// Collect findings and print a summary.
	for i, cfg := range configs {
		log := SubagentLog{Name: cfg.name}
		if findings[i] != nil {
			log.Usage = findings[i].Usage
			log.Candidates = len(findings[i].Candidates)
			result.Findings = append(result.Findings, findings[i])
			result.TotalUsage.Add(findings[i].Usage)
		}
		if errors[i] != nil {
			log.Error = errors[i].Error()
			fmt.Printf("    %s ERROR: %v\n", cfg.name, errors[i])
		} else {
			fmt.Printf("    %s found %d candidate(s)\n", cfg.name, log.Candidates)
			if verbose && findings[i] != nil {
				for _, c := range findings[i].Candidates {
					fmt.Printf("      → %s (%s) confidence=%.2f  evidence: %s\n",
						c.CustomerName, c.CustomerID, c.Confidence, truncate(c.Evidence, 100))
				}
			}
		}
		result.SubagentLogs = append(result.SubagentLogs, log)
	}

	// ---- Phase 2: Merger agent synthesizes findings ----

	fmt.Println("  Phase 2: Merger agent synthesizing findings...")

	mergerInput := buildMergerPrompt(txn, findings)

	submitInput, _, mergerUsage, err := runToolLoop(
		"MergerAgent",
		mergerPrompt,
		mergerTools(),
		mergerInput,
		4,
		toolHandler.Execute,
		"submit_match_result",
		verbose,
	)
	result.TotalUsage.Add(mergerUsage)

	if err != nil {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            fmt.Sprintf("Merger agent error: %v", err),
			InvestigationSummary: "Multi-agent pipeline failed during merge phase.",
		}
		return result, err
	}

	if submitInput != nil {
		var match models.AgentMatchResult
		if parseErr := json.Unmarshal(submitInput, &match); parseErr != nil {
			return result, fmt.Errorf("parsing merger submit_match_result: %w", parseErr)
		}
		result.Match = &match
	} else {
		result.Match = &models.AgentMatchResult{
			Status:               "unresolved",
			Reasoning:            "Merger agent did not submit a result.",
			InvestigationSummary: "Multi-agent pipeline: merger did not produce output.",
		}
	}

	return result, nil
}

// buildMergerPrompt assembles the transaction info and all subagent findings into a single prompt
// for the merger agent to analyze.
func buildMergerPrompt(txn models.BankTransaction, findings []*SubagentFinding) string {
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
	sb.WriteString("SPECIALIST AGENT FINDINGS:\n")
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

// CalculateMultiAgentCost reuses the same pricing as the single agent.
func CalculateMultiAgentCost(u TokenUsage) float64 {
	return agent.CalculateCost(u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens)
}
