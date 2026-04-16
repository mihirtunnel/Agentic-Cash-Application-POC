package models

import "strings"

type Customer struct {
	CustomerID     string `json:"customer_id"`
	Name           string `json:"name"`
	PaymentMethod  string `json:"payment_method"`
	TypicalPattern string `json:"typical_pattern"`
	DiscountPolicy string `json:"discount_policy"`
}

type Invoice struct {
	InvoiceID   string  `json:"invoice_id"`
	CustomerID  string  `json:"customer_id"`
	Amount      float64 `json:"amount"`
	DueDate     string  `json:"due_date"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
}

type ParsedInvoice struct {
	InvoiceRef string  `json:"invoice_ref"`
	Amount     float64 `json:"amount"`
	Note       string  `json:"note,omitempty"`
}

type RemittanceEmail struct {
	EmailID        string          `json:"email_id"`
	CustomerID     string          `json:"customer_id"`
	SenderName     string          `json:"sender_name"`
	SenderEmail    string          `json:"sender_email"`
	ReceivedDate   string          `json:"received_date"`
	Subject        string          `json:"subject"`
	BodyText       string          `json:"body_text"`
	ParsedInvoices []ParsedInvoice `json:"parsed_invoices"`
	TotalAmount    float64         `json:"total_amount"`
}

type BankTransaction struct {
	TxnID               string   `json:"txn_id"`
	Date                string   `json:"date"`
	Amount              float64  `json:"amount"`
	Direction           string   `json:"direction"`
	CounterpartyName    string   `json:"counterparty_name"`
	CounterpartyAccount string   `json:"counterparty_account"`
	BankReference       string   `json:"bank_reference"`
	RemittanceInfo      string   `json:"remittance_info"`
	ExpectedCustomer    string   `json:"expected_customer"`
	ExpectedInvoices    []string `json:"expected_invoices"`
	Scenario            string   `json:"scenario"`
}

type MatchedInvoice struct {
	ID             string  `json:"id"`
	OriginalAmount float64 `json:"original_amount"`
	Applied        float64 `json:"applied"`
	Deduction      float64 `json:"deduction"`
	Reason         string  `json:"reason,omitempty"`
}

type AgentMatchResult struct {
	Status               string           `json:"status"` // "matched", "unresolved", or "internal_transfer"
	CustomerID           string           `json:"customer_id,omitempty"`
	CustomerName         string           `json:"customer_name,omitempty"`
	Confidence           float64          `json:"confidence"`
	Invoices             []MatchedInvoice `json:"invoices,omitempty"`
	TotalApplied         float64          `json:"total_applied,omitempty"`
	UnmatchedAmount      float64          `json:"unmatched_amount,omitempty"`
	Reasoning            string           `json:"reasoning"`
	InvestigationSummary string           `json:"investigation_summary"`
	SuggestedAlias       string           `json:"suggested_alias,omitempty"`
}

type GuardrailCheck struct {
	Passed                   bool     `json:"passed"`
	AmountsBalance           bool     `json:"amounts_balance"`
	IDsExist                 bool     `json:"ids_exist"`
	InvoicesBelongToCustomer bool     `json:"invoices_belong_to_customer"`
	DeductionsWithinPolicy   bool     `json:"deductions_within_policy"`
	FailReasons              []string `json:"fail_reasons,omitempty"`
}

type PipelineResult struct {
	TxnID             string            `json:"txn_id"`
	Amount            float64           `json:"amount"`
	CounterpartyName  string            `json:"counterparty_name"`
	Scenario          string            `json:"scenario"`
	AgentResult       *AgentMatchResult `json:"agent_result"`
	Guardrails        *GuardrailCheck   `json:"guardrails,omitempty"`
	RoutingDecision   string            `json:"routing_decision"`
	ToolCallCount     int               `json:"tool_call_count"`
	TotalInputTokens  int               `json:"total_input_tokens"`
	TotalOutputTokens int               `json:"total_output_tokens"`
	TotalCostUSD      float64           `json:"total_cost_usd"`
	MatchesExpected   bool              `json:"matches_expected"`
}

// IsInternalTransfer checks if a bank transaction is an internal Tauber Oil transfer or ZBA.
// Returns true if the transaction is between Tauber Oil entities and should be excluded from AR matching.
func (txn *BankTransaction) IsInternalTransfer() bool {
	// Check for internal indicator in scenario field
	if strings.Contains(strings.ToUpper(txn.Scenario), "INTERNAL") {
		return true
	}

	// Check remittance info for Tauber Oil internal transfers
	remittanceUpper := strings.ToUpper(txn.RemittanceInfo)

	// Pattern: SENDER is a Tauber entity AND BENE (beneficiary) is also a Tauber entity
	tauberIndicators := []string{"TAUBER OIL", "TAUBER PETROCHEMICAL", "TAUBER"}
	zbaIndicators := []string{"ZBA", "ZERO BALANCE"}

	isSenderTauber := false
	isBeneTauber := false
	isZBA := false

	for _, indicator := range tauberIndicators {
		if strings.Contains(remittanceUpper, "SENDER="+indicator) ||
			strings.Contains(remittanceUpper, "SENDER = "+indicator) {
			isSenderTauber = true
		}
		if strings.Contains(remittanceUpper, "BENE="+indicator) ||
			strings.Contains(remittanceUpper, "BENE "+indicator) {
			isBeneTauber = true
		}
	}

	for _, indicator := range zbaIndicators {
		if strings.Contains(remittanceUpper, indicator) {
			isZBA = true
		}
	}

	// It's internal if both sender and beneficiary are Tauber entities, or if it's a ZBA transfer
	return (isSenderTauber && isBeneTauber) || isZBA
}
