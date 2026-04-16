package customerid

import (
	"fmt"
	"strings"

	"cashapp-poc/internal/aiclient"
	"cashapp-poc/internal/models"
)

const systemPrompt = `You are a treasury cash application specialist at an oil & gas company.
Your task is to identify which customer sent a bank payment.

You will receive:
- A bank transaction with amount, date, and counterparty description
- A shortlist of 3-8 candidate customers pre-filtered by name similarity and invoice amounts

Rules:
- Return exactly one customer match with a confidence score between 0.0 and 1.0
- Confidence 0.9+ means you are very sure this is the right customer
- Confidence 0.7-0.89 means probable but not certain
- Confidence below 0.7 means you cannot reliably determine the customer
- Base your confidence on: name similarity, payment amount alignment, payment method consistency, and any reference numbers
- Do not guess. If the evidence is ambiguous, return low confidence.`

func BuildTool() map[string]interface{} {
	return map[string]interface{}{
		"name":        "identify_customer",
		"description": "Identify which customer sent this bank payment",
		"input_schema": map[string]interface{}{
			"type":     "object",
			"required": []string{"customer_id", "customer_name", "confidence", "reasoning"},
			"properties": map[string]interface{}{
				"customer_id": map[string]interface{}{
					"type":        "string",
					"description": "The customer ID from the candidate list",
				},
				"customer_name": map[string]interface{}{
					"type":        "string",
					"description": "The customer name",
				},
				"confidence": map[string]interface{}{
					"type":        "number",
					"description": "Confidence score from 0.0 to 1.0",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "Brief explanation of why this customer was selected",
				},
			},
		},
	}
}

func BuildPrompt(txn models.BankTransaction, candidates []models.Candidate) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Bank transaction:\n")
	fmt.Fprintf(&b, "  Amount: $%.2f\n", txn.Amount)
	fmt.Fprintf(&b, "  Date: %s\n", txn.Date)
	fmt.Fprintf(&b, "  Counterparty: %q\n", txn.CounterpartyName)
	if txn.BankReference != "" {
		fmt.Fprintf(&b, "  Reference: %q\n", txn.BankReference)
	}
	if txn.RemittanceInfo != "" {
		fmt.Fprintf(&b, "  Remittance info: %q\n", txn.RemittanceInfo)
	}

	fmt.Fprintf(&b, "\nCandidate customers (pre-filtered by amount and name similarity):\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. %s (%s) — typical %s payer, %s, discount policy: %s\n",
			i+1, c.Customer.Name, c.Customer.CustomerID,
			c.Customer.PaymentMethod, c.Customer.TypicalPattern,
			c.Customer.DiscountPolicy)
	}

	fmt.Fprintf(&b, "\nWhich customer most likely sent this payment?")
	return b.String()
}

func Call(client *aiclient.Client, txn models.BankTransaction, candidates []models.Candidate) (*models.Call1Result, error) {
	prompt := BuildPrompt(txn, candidates)
	tool := BuildTool()

	fmt.Println("  [CALL 1] Calling Claude for customer identification...")
	result, tokens, err := client.CallCustomerID(systemPrompt, prompt, tool)
	if err != nil {
		return nil, fmt.Errorf("Call 1 failed: %w", err)
	}
	if result == nil {
		return nil, nil // dry-run
	}

	cost := aiclient.CalculateCost(tokens)
	fmt.Printf("  [CALL 1] Result: %s (%s) — confidence: %.2f\n",
		result.CustomerName, result.CustomerID, result.Confidence)
	fmt.Printf("  [CALL 1] Tokens: %d in / %d out — cost: $%.4f\n",
		tokens.InputTokens, tokens.OutputTokens, cost)
	fmt.Printf("  [CALL 1] Reasoning: %s\n", result.Reasoning)

	return &models.Call1Result{
		CustomerIDResult: *result,
		Tokens:           tokens,
		CostUSD:          cost,
	}, nil
}

func GetSystemPrompt() string {
	return systemPrompt
}
