package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"cashapp-agent-poc/internal/dataloader"
)

type ToolHandler struct {
	Data *dataloader.Data
}

func (h *ToolHandler) Execute(toolName string, input json.RawMessage) (string, error) {
	switch toolName {
	case "search_customers_by_name":
		return h.searchCustomersByName(input)
	case "search_customers_by_amount":
		return h.searchCustomersByAmount(input)
	case "get_customer_details":
		return h.getCustomerDetails(input)
	case "get_customer_invoices":
		return h.getCustomerInvoices(input)
	case "search_remittance_emails":
		return h.searchRemittanceEmails(input)
	case "get_customer_payment_history":
		return h.getCustomerPaymentHistory(input)
	case "lookup_reference":
		return h.lookupReference(input)
	case "check_credit_memos":
		return h.checkCreditMemos(input)
	case "submit_match_result":
		return "", fmt.Errorf("submit_match_result should be handled by the agent loop")
	default:
		return "", fmt.Errorf("unknown tool: %s", toolName)
	}
}

func (h *ToolHandler) searchCustomersByName(input json.RawMessage) (string, error) {
	var params struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}
	if params.Limit == 0 {
		params.Limit = 5
	}

	matches := h.Data.SearchCustomersByName(params.Query, params.Limit)
	if len(matches) == 0 {
		return fmt.Sprintf("No customers found matching \"%s\".", params.Query), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d matching customers:\n\n", len(matches)))
	for i, m := range matches {
		sb.WriteString(fmt.Sprintf("%d. %s (%s) — similarity: %.2f\n", i+1, m.Name, m.CustomerID, m.Score))
	}
	return sb.String(), nil
}

func (h *ToolHandler) searchCustomersByAmount(input json.RawMessage) (string, error) {
	var params struct {
		Amount           float64 `json:"amount"`
		TolerancePercent float64 `json:"tolerance_percent"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}
	if params.TolerancePercent == 0 {
		params.TolerancePercent = 3
	}

	matches := h.Data.SearchCustomersByAmount(params.Amount, params.TolerancePercent)
	if len(matches) == 0 {
		return fmt.Sprintf("No customers found with invoice combinations matching $%.2f (±%.0f%%).", params.Amount, params.TolerancePercent), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d customer(s) with matching invoice combinations:\n\n", len(matches)))
	for i, m := range matches {
		sb.WriteString(fmt.Sprintf("%d. %s (%s)\n", i+1, m.CustomerName, m.CustomerID))
		sb.WriteString(fmt.Sprintf("   Invoices: %s = $%.2f", strings.Join(m.InvoiceIDs, " + "), m.InvoiceTotal))
		if m.Difference != 0 {
			sb.WriteString(fmt.Sprintf(" (%.2f%% %s payment)", abs(m.DiffPercent), direction(m.Difference)))
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

func (h *ToolHandler) getCustomerDetails(input json.RawMessage) (string, error) {
	var params struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}

	cust := h.Data.GetCustomerByID(params.CustomerID)
	if cust == nil {
		return fmt.Sprintf("Customer %s not found.", params.CustomerID), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Customer: %s (%s)\n", cust.Name, cust.CustomerID))
	sb.WriteString(fmt.Sprintf("  Payment method: %s\n", cust.PaymentMethod))
	sb.WriteString(fmt.Sprintf("  Typical pattern: %s\n", cust.TypicalPattern))
	sb.WriteString(fmt.Sprintf("  Discount policy: %s\n", cust.DiscountPolicy))
	return sb.String(), nil
}

func (h *ToolHandler) getCustomerInvoices(input json.RawMessage) (string, error) {
	var params struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}

	invoices := h.Data.GetInvoicesForCustomer(params.CustomerID)
	if len(invoices) == 0 {
		return fmt.Sprintf("No open invoices found for customer %s.", params.CustomerID), nil
	}

	cust := h.Data.GetCustomerByID(params.CustomerID)
	name := params.CustomerID
	if cust != nil {
		name = cust.Name
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Open invoices for %s (%s):\n\n", name, params.CustomerID))
	total := 0.0
	for _, inv := range invoices {
		sb.WriteString(fmt.Sprintf("  %s — $%.2f — due %s — %s\n", inv.InvoiceID, inv.Amount, inv.DueDate, inv.Description))
		total += inv.Amount
	}
	sb.WriteString(fmt.Sprintf("\n  Total open balance: $%.2f\n", total))
	return sb.String(), nil
}

func (h *ToolHandler) searchRemittanceEmails(input json.RawMessage) (string, error) {
	var params struct {
		AmountMin     float64 `json:"amount_min"`
		AmountMax     float64 `json:"amount_max"`
		DateFrom      string  `json:"date_from"`
		DateTo        string  `json:"date_to"`
		SenderKeyword string  `json:"sender_keyword"`
		InvoiceRef    string  `json:"invoice_ref"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}

	emails := h.Data.SearchRemittanceEmails(
		params.AmountMin, params.AmountMax,
		params.DateFrom, params.DateTo,
		params.SenderKeyword, params.InvoiceRef,
	)

	if len(emails) == 0 {
		return "No matching remittance emails found.", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d matching remittance email(s):\n\n", len(emails)))
	for _, e := range emails {
		sb.WriteString(fmt.Sprintf("  Email ID: %s\n", e.EmailID))
		sb.WriteString(fmt.Sprintf("  From: %s <%s>\n", e.SenderName, e.SenderEmail))
		sb.WriteString(fmt.Sprintf("  Received: %s\n", e.ReceivedDate))
		sb.WriteString(fmt.Sprintf("  Subject: \"%s\"\n", e.Subject))
		sb.WriteString(fmt.Sprintf("  Body: \"%s\"\n", e.BodyText))
		if len(e.ParsedInvoices) > 0 {
			sb.WriteString("  Parsed invoices:\n")
			for _, pi := range e.ParsedInvoices {
				sb.WriteString(fmt.Sprintf("    - %s: $%.2f", pi.InvoiceRef, pi.Amount))
				if pi.Note != "" {
					sb.WriteString(fmt.Sprintf(" (%s)", pi.Note))
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString(fmt.Sprintf("  Total amount: $%.2f\n\n", e.TotalAmount))
	}
	return sb.String(), nil
}

func (h *ToolHandler) getCustomerPaymentHistory(input json.RawMessage) (string, error) {
	var params struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}

	cust := h.Data.GetCustomerByID(params.CustomerID)
	if cust == nil {
		return fmt.Sprintf("Customer %s not found.", params.CustomerID), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s (%s) payment history:\n", cust.Name, cust.CustomerID))
	sb.WriteString(fmt.Sprintf("  Payment method: %s\n", cust.PaymentMethod))
	sb.WriteString(fmt.Sprintf("  Typical pattern: %s\n", cust.TypicalPattern))
	sb.WriteString(fmt.Sprintf("  Discount policy: %s\n", cust.DiscountPolicy))
	sb.WriteString("  (Detailed payment history not available in POC — using customer profile as proxy)\n")
	return sb.String(), nil
}

func (h *ToolHandler) lookupReference(input json.RawMessage) (string, error) {
	var params struct {
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}

	match := h.Data.LookupReference(params.Reference)
	if match == nil {
		return fmt.Sprintf("No match found for \"%s\" in invoice IDs.", params.Reference), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Match found! Type: %s\n", match.Type))
	sb.WriteString(fmt.Sprintf("  Customer: %s (%s)\n", match.CustomerName, match.CustomerID))
	if match.InvoiceID != "" {
		sb.WriteString(fmt.Sprintf("  Invoice: %s — $%.2f\n", match.InvoiceID, match.Amount))
	}
	return sb.String(), nil
}

func (h *ToolHandler) checkCreditMemos(input json.RawMessage) (string, error) {
	var params struct {
		CustomerID string `json:"customer_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", err
	}

	return fmt.Sprintf("No outstanding credit memos found for customer %s. (Credit memo data not available in POC)", params.CustomerID), nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func direction(diff float64) string {
	if diff > 0 {
		return "over"
	}
	return "under"
}

func ToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "search_customers_by_name",
			"description": "Search for customers by name using fuzzy matching. Returns top matches with similarity scores.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"query"},
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "The name or partial name to search for"},
					"limit": map[string]interface{}{"type": "integer", "default": 5, "description": "Max results to return"},
				},
			},
		},
		{
			"name":        "search_customers_by_amount",
			"description": "Find all customers whose open invoices (individually or in combination) sum to the given amount within a tolerance percentage. Often the strongest signal for identifying the payer.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"amount"},
				"properties": map[string]interface{}{
					"amount":            map[string]interface{}{"type": "number", "description": "The payment amount to match"},
					"tolerance_percent": map[string]interface{}{"type": "number", "default": 3, "description": "Tolerance percentage (e.g., 3 means ±3%)"},
				},
			},
		},
		{
			"name":        "get_customer_details",
			"description": "Get a customer's full profile: name, payment method, typical pattern, and discount policy.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"customer_id"},
				"properties": map[string]interface{}{
					"customer_id": map[string]interface{}{"type": "string", "description": "The customer ID (e.g., CUST-0091)"},
				},
			},
		},
		{
			"name":        "get_customer_invoices",
			"description": "Get all open invoices for a specific customer. Returns invoice IDs, amounts, due dates, and descriptions.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"customer_id"},
				"properties": map[string]interface{}{
					"customer_id": map[string]interface{}{"type": "string", "description": "The customer ID"},
				},
			},
		},
		{
			"name":        "search_remittance_emails",
			"description": "Search for remittance advice emails by amount range, date range, sender keyword, or invoice reference. Customers often email remittance advice around payment time.",
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"amount_min":     map[string]interface{}{"type": "number", "description": "Minimum payment amount"},
					"amount_max":     map[string]interface{}{"type": "number", "description": "Maximum payment amount"},
					"date_from":      map[string]interface{}{"type": "string", "description": "Start date (YYYY-MM-DD)"},
					"date_to":        map[string]interface{}{"type": "string", "description": "End date (YYYY-MM-DD)"},
					"sender_keyword": map[string]interface{}{"type": "string", "description": "Keyword in sender name/email"},
					"invoice_ref":    map[string]interface{}{"type": "string", "description": "Invoice reference to search for"},
				},
			},
		},
		{
			"name":        "get_customer_payment_history",
			"description": "Get a customer's recent payment history: typical payment method, frequency, usual amounts, and patterns.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"customer_id"},
				"properties": map[string]interface{}{
					"customer_id": map[string]interface{}{"type": "string", "description": "The customer ID"},
				},
			},
		},
		{
			"name":        "lookup_reference",
			"description": "Look up a reference string against invoice IDs. Use when the bank text or reference contains a code.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"reference"},
				"properties": map[string]interface{}{
					"reference": map[string]interface{}{"type": "string", "description": "The reference code, account number, or ID to look up"},
				},
			},
		},
		{
			"name":        "check_credit_memos",
			"description": "Check if a customer has outstanding credit memos that could explain an amount mismatch.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"customer_id"},
				"properties": map[string]interface{}{
					"customer_id": map[string]interface{}{"type": "string", "description": "The customer ID"},
				},
			},
		},
		{
			"name":        "submit_match_result",
			"description": "Submit your final matching result. You MUST call this to complete the investigation. Set status to 'matched' if you identified the customer and invoices, or 'unresolved' if you could not.",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []string{"status", "confidence", "reasoning", "investigation_summary"},
				"properties": map[string]interface{}{
					"status":                map[string]interface{}{"type": "string", "enum": []string{"matched", "unresolved"}},
					"customer_id":           map[string]interface{}{"type": "string"},
					"customer_name":         map[string]interface{}{"type": "string"},
					"confidence":            map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1},
					"invoices":              map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}, "original_amount": map[string]interface{}{"type": "number"}, "applied": map[string]interface{}{"type": "number"}, "deduction": map[string]interface{}{"type": "number", "default": 0}, "reason": map[string]interface{}{"type": "string"}}}},
					"total_applied":         map[string]interface{}{"type": "number"},
					"unmatched_amount":      map[string]interface{}{"type": "number"},
					"reasoning":             map[string]interface{}{"type": "string"},
					"investigation_summary": map[string]interface{}{"type": "string"},
					"suggested_alias":       map[string]interface{}{"type": "string"},
				},
			},
		},
	}
}
