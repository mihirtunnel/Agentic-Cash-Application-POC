package invoicematch

import (
	"fmt"
	"strings"

	"cashapp-poc/internal/aiclient"
	"cashapp-poc/internal/models"
)

const systemPrompt = `You are a treasury cash application specialist at an oil & gas company.
Your task is to match a bank payment to specific open invoices for a confirmed customer.

You will receive:
- A confirmed customer and payment details
- That customer's open invoices
- Remittance advice (if found) from email

Rules:
- Match the payment to specific invoices. The applied amounts must sum to the payment amount.
- Account for early-pay discounts (typically 1-2% of invoice amount)
- Account for short-payments — note the deduction amount and categorize the reason
- Valid deduction reasons: early_pay_discount, damaged_goods, short_shipment, pricing_dispute, credit_memo, unknown
- If remittance advice lists specific invoices, prioritize that information
- If you cannot match the full amount, report the unmatched_amount
- Confidence 0.95+ means amounts match perfectly with clear evidence
- Confidence 0.70-0.94 means reasonable match but some ambiguity
- Below 0.70 means you cannot reliably match
- If the payment exceeds the total of all matched invoices, report the excess as unmatched_amount`

func BuildTool() map[string]interface{} {
	return map[string]interface{}{
		"name":        "submit_invoice_match",
		"description": "Submit the invoice matching result for this payment",
		"input_schema": map[string]interface{}{
			"type":     "object",
			"required": []string{"customer_id", "confidence", "invoices", "total_applied", "unmatched_amount", "reasoning"},
			"properties": map[string]interface{}{
				"customer_id": map[string]interface{}{"type": "string"},
				"confidence":  map[string]interface{}{"type": "number"},
				"invoices": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type":     "object",
						"required": []string{"id", "original_amount", "applied"},
						"properties": map[string]interface{}{
							"id":              map[string]interface{}{"type": "string"},
							"original_amount": map[string]interface{}{"type": "number"},
							"applied":         map[string]interface{}{"type": "number"},
							"deduction":       map[string]interface{}{"type": "number"},
							"reason":          map[string]interface{}{"type": "string"},
						},
					},
				},
				"total_applied":    map[string]interface{}{"type": "number"},
				"unmatched_amount": map[string]interface{}{"type": "number"},
				"reasoning":        map[string]interface{}{"type": "string"},
			},
		},
	}
}

func BuildPrompt(txn models.BankTransaction, customer models.Customer, invoices []models.Invoice, email *models.RemittanceEmail) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Payment: $%.2f from %s (%s) on %s\n",
		txn.Amount, customer.Name, customer.CustomerID, txn.Date)
	fmt.Fprintf(&b, "Customer discount policy: %s\n", customer.DiscountPolicy)

	fmt.Fprintf(&b, "\nOpen invoices for %s:\n", customer.Name)
	for _, inv := range invoices {
		fmt.Fprintf(&b, "  %s — $%.2f — due %s — %s\n",
			inv.InvoiceID, inv.Amount, inv.DueDate, inv.Description)
	}

	if email != nil {
		fmt.Fprintf(&b, "\nRemittance advice (from email received %s):\n", email.ReceivedDate)
		fmt.Fprintf(&b, "  From: %s <%s>\n", email.SenderName, email.SenderEmail)
		fmt.Fprintf(&b, "  Subject: %q\n", email.Subject)
		fmt.Fprintf(&b, "  Body: %s\n", email.BodyText)
		if len(email.ParsedInvoices) > 0 {
			fmt.Fprintf(&b, "  Parsed line items:\n")
			for _, pi := range email.ParsedInvoices {
				fmt.Fprintf(&b, "    %s: $%.2f", pi.InvoiceRef, pi.Amount)
				if pi.Note != "" {
					fmt.Fprintf(&b, " (%s)", pi.Note)
				}
				fmt.Fprintf(&b, "\n")
			}
		}
	} else {
		fmt.Fprintf(&b, "\nNo remittance advice found for this payment.\n")
	}

	if txn.RemittanceInfo != "" {
		fmt.Fprintf(&b, "\nBank remittance info: %q\n", txn.RemittanceInfo)
	}

	fmt.Fprintf(&b, "\nMatch this payment to invoices. Account for any discounts or deductions.")
	return b.String()
}

func Call(client *aiclient.Client, txn models.BankTransaction, customer models.Customer, invoices []models.Invoice, email *models.RemittanceEmail) (*models.Call2Result, error) {
	prompt := BuildPrompt(txn, customer, invoices, email)
	tool := BuildTool()

	fmt.Println("  [CALL 2] Calling Claude for invoice matching...")
	result, tokens, err := client.CallInvoiceMatch(systemPrompt, prompt, tool)
	if err != nil {
		return nil, fmt.Errorf("Call 2 failed: %w", err)
	}
	if result == nil {
		return nil, nil // dry-run
	}

	cost := aiclient.CalculateCost(tokens)
	fmt.Printf("  [CALL 2] Result: confidence %.2f — %d invoices matched — unmatched: $%.2f\n",
		result.Confidence, len(result.Invoices), result.UnmatchedAmount)
	fmt.Printf("  [CALL 2] Tokens: %d in / %d out — cost: $%.4f\n",
		tokens.InputTokens, tokens.OutputTokens, cost)
	for _, inv := range result.Invoices {
		ded := ""
		if inv.Deduction > 0 {
			ded = fmt.Sprintf(" (deduction: $%.2f — %s)", inv.Deduction, inv.Reason)
		}
		fmt.Printf("    %s: $%.2f applied%s\n", inv.ID, inv.Applied, ded)
	}
	fmt.Printf("  [CALL 2] Reasoning: %s\n", result.Reasoning)

	return &models.Call2Result{
		InvoiceMatchResult: *result,
		Tokens:             tokens,
		CostUSD:            cost,
	}, nil
}
