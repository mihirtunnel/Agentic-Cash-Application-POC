package models

import "time"

// InvoiceLine represents a single invoice found in the remittance.
type InvoiceLine struct {
	InvoiceNumber string  `json:"invoice_number"`
	Amount        float64 `json:"amount"`
	Description   string  `json:"description,omitempty"`
}

// RemittanceData is the structured output produced by the AI extraction.
type RemittanceData struct {
	Payer               string        `json:"payer"`
	PaymentDate         string        `json:"payment_date"`
	TotalAmount         float64       `json:"total_amount"`
	Currency            string        `json:"currency"`
	BankReferenceNumber string        `json:"bank_reference_number"`
	Invoices            []InvoiceLine `json:"invoices"`
	InvoiceCount        int           `json:"invoice_count"`
	Notes               string        `json:"notes,omitempty"`
}

// EmailMetadata holds the header fields parsed from the .eml file.
type EmailMetadata struct {
	From    string
	To      string
	Subject string
	Date    time.Time
}

// PDFAttachment holds raw bytes and the filename of an attached PDF.
type PDFAttachment struct {
	Filename string
	Data     []byte
}

// ParsedEmail is the full result of email parsing before AI processing.
type ParsedEmail struct {
	Metadata    EmailMetadata
	BodyText    string
	Attachments []PDFAttachment
}

// TokenUsage tracks API token consumption.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
}

// ExtractionResult wraps the AI output together with cost metadata.
type ExtractionResult struct {
	Data        *RemittanceData
	TokenUsage  TokenUsage
	CostUSD     float64
	Provider    string
	ModelUsed   string
	EmailFile   string
	ProcessedAt time.Time
}
