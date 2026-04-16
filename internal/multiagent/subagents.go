package multiagent

import (
	"encoding/json"
	"fmt"

	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/tools"
)

// SubagentFinding is the structured output each subagent produces.
type SubagentFinding struct {
	AgentName   string   `json:"agent_name"`
	Candidates  []Candidate `json:"candidates"`
	RawText     string   `json:"raw_text"`
	Usage       TokenUsage `json:"-"`
}

type Candidate struct {
	CustomerID   string  `json:"customer_id"`
	CustomerName string  `json:"customer_name"`
	Confidence   float64 `json:"confidence"`
	Evidence     string  `json:"evidence"`
	InvoiceIDs   []string `json:"invoice_ids,omitempty"`
}

// submit_finding is the tool each subagent uses to return structured results.
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

// ----- Name Agent -----
// Searches by counterparty name and reference codes.

var nameAgentPrompt = `You are a NAME MATCHING specialist for Tauber Oil Company treasury. Given a bank payment, find candidate customers by analyzing the counterparty name and any reference codes.

Use your tools to:
1. If the bank reference or counterparty text may contain an invoice ID, look it up with lookup_reference (invoice IDs only).
2. Search for customers by name using search_customers_by_name.

For each candidate you find, assess how well the name matches and assign a confidence:
- 0.90+: Strong name match (exact or very close fuzzy match)
- 0.70-0.89: Partial name match (abbreviation or partial overlap)
- Below 0.70: Weak match

You MUST call submit_finding to return your results. Even if you find nothing, submit an empty candidates list.`

func nameAgentTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	selected := filterTools(all, "search_customers_by_name", "lookup_reference")
	return append(selected, submitFindingTool)
}

// ----- Amount Agent -----
// Searches by payment amount and checks invoice combinations.

var amountAgentPrompt = `You are an AMOUNT MATCHING specialist for Tauber Oil Company treasury. Given a bank payment, find candidate customers whose open invoices match the payment amount.

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

func amountAgentTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	selected := filterTools(all, "search_customers_by_amount", "get_customer_invoices")
	return append(selected, submitFindingTool)
}

// ----- Email Agent -----
// Searches remittance emails for corroborating evidence.

var emailAgentPrompt = `You are a REMITTANCE EMAIL specialist for Tauber Oil Company treasury. Given a bank payment, search for remittance advice emails that match this payment.

Use your tools to:
1. Search remittance emails by amount range (payment amount ±5%) and date range (±7 days from payment date).
2. If the counterparty name is available, also search by sender keyword.

For each matching email, extract the customer it came from and the invoices referenced.
Assign confidence:
- 0.95+: Email explicitly references this exact payment amount and lists invoices
- 0.80-0.94: Email matches amount but date is a few days off
- 0.70-0.79: Partial match (amount close but not exact)

You MUST call submit_finding to return your results.`

func emailAgentTools() []map[string]interface{} {
	all := tools.ToolDefinitions()
	selected := filterTools(all, "search_remittance_emails")
	return append(selected, submitFindingTool)
}

// ----- Runner -----

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

// RunSubagent runs a single subagent with its specialized prompt, tools, and the transaction.
func RunSubagent(name string, systemPrompt string, agentTools []map[string]interface{}, txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (*SubagentFinding, error) {
	prompt := buildTxnPrompt(txn)

	submitInput, agentText, usage, err := runToolLoop(
		name,
		systemPrompt,
		agentTools,
		prompt,
		3, // max 3 rounds per subagent
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

// filterTools returns only the tool definitions whose names appear in the keep list.
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
