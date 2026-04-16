package models

type Customer struct {
	CustomerID     string `json:"customer_id"`
	Name           string `json:"name"`
	PaymentMethod  string `json:"payment_method"`
	TypicalPattern string `json:"typical_pattern"`
	DiscountPolicy string `json:"discount_policy"`
}

type Invoice struct {
	InvoiceID  string  `json:"invoice_id"`
	CustomerID string  `json:"customer_id"`
	Amount     float64 `json:"amount"`
	DueDate    string  `json:"due_date"`
	Description string `json:"description"`
	Status     string  `json:"status"`
}

type CustomerAlias struct {
	CustomerID   string   `json:"customer_id"`
	Aliases      []string `json:"aliases"`
	BankAccounts []string `json:"bank_accounts"`
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
	TxnID              string   `json:"txn_id"`
	Date               string   `json:"date"`
	Amount             float64  `json:"amount"`
	Direction          string   `json:"direction"`
	CounterpartyName   string   `json:"counterparty_name"`
	CounterpartyAccount string  `json:"counterparty_account"`
	BankReference      string   `json:"bank_reference"`
	RemittanceInfo     string   `json:"remittance_info"`
	ExpectedCustomer   string   `json:"expected_customer"`
	ExpectedInvoices   []string `json:"expected_invoices"`
	Scenario           string   `json:"scenario"`
}

type Candidate struct {
	Customer Customer
	Score    float64
	Reason   string
}

type MatchedInvoice struct {
	ID             string  `json:"id"`
	OriginalAmount float64 `json:"original_amount"`
	Applied        float64 `json:"applied"`
	Deduction      float64 `json:"deduction"`
	Reason         string  `json:"reason,omitempty"`
}

type CustomerIDResult struct {
	CustomerID   string  `json:"customer_id"`
	CustomerName string  `json:"customer_name"`
	Confidence   float64 `json:"confidence"`
	Reasoning    string  `json:"reasoning"`
}

type InvoiceMatchResult struct {
	CustomerID      string           `json:"customer_id"`
	Confidence      float64          `json:"confidence"`
	Invoices        []MatchedInvoice `json:"invoices"`
	TotalApplied    float64          `json:"total_applied"`
	UnmatchedAmount float64          `json:"unmatched_amount"`
	Reasoning       string           `json:"reasoning"`
}

type GuardrailCheck struct {
	Passed                bool `json:"passed"`
	AmountsBalance        bool `json:"amounts_balance"`
	IDsExist              bool `json:"ids_exist"`
	InvoicesBelongToCust  bool `json:"invoices_belong_to_customer"`
	DeductionsWithinPolicy bool `json:"deductions_within_policy"`
	NoDoubleMatch         bool `json:"no_double_match"`
	FailReasons           []string `json:"fail_reasons,omitempty"`
}

type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type PipelineResult struct {
	TxnID           string              `json:"txn_id"`
	PipelinePath    string              `json:"pipeline_path"`
	ShortCircuit    *ShortCircuitResult `json:"short_circuit,omitempty"`
	PreFilter       *PreFilterResult    `json:"pre_filter,omitempty"`
	Call1           *Call1Result        `json:"call_1,omitempty"`
	Call2           *Call2Result        `json:"call_2,omitempty"`
	Guardrails      *GuardrailCheck     `json:"guardrails,omitempty"`
	RoutingDecision string              `json:"routing_decision"`
	TotalCostUSD    float64             `json:"total_cost_usd"`
	MatchesExpected bool                `json:"matches_expected"`
}

type ShortCircuitResult struct {
	Type       string `json:"type"`
	CustomerID string `json:"customer_id"`
	InvoiceID  string `json:"invoice_id,omitempty"`
}

type PreFilterResult struct {
	CandidatesFound int       `json:"candidates_found"`
	TopCandidate    string    `json:"top_candidate"`
	TopScore        float64   `json:"top_score"`
}

type Call1Result struct {
	CustomerIDResult
	Tokens  TokenUsage `json:"tokens"`
	CostUSD float64    `json:"cost_usd"`
}

type Call2Result struct {
	InvoiceMatchResult
	Tokens  TokenUsage `json:"tokens"`
	CostUSD float64    `json:"cost_usd"`
}
