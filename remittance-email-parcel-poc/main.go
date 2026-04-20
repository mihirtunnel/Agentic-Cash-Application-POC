package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"remittance-poc/internal/aiclient"
	"remittance-poc/internal/emailparser"
	"remittance-poc/internal/models"
	"remittance-poc/internal/pdfextractor"
	"remittance-poc/internal/textbuilder"
)

func main() {
	emailFlag := flag.String("email", "data/sample.eml", "Path to the .eml file to process")
	dryRun := flag.Bool("dry-run", false, "Run without making AI API calls")
	provider := flag.String("provider", "claude", "AI provider: 'claude' or 'openai'")
	flag.Parse()

	loadEnvFile(".env")

	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║    Remittance Email AI Extraction — POC      ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	// ── Parse email ──────────────────────────────────────────────────────────
	fmt.Printf("Reading email:    %s\n", *emailFlag)
	raw, err := os.ReadFile(*emailFlag)
	if err != nil {
		fatalf("Cannot read email file: %v", err)
	}

	parsed, err := emailparser.ParseEML(raw)
	if err != nil {
		fatalf("Email parsing failed: %v", err)
	}

	fmt.Printf("From:             %s\n", parsed.Metadata.From)
	fmt.Printf("Subject:          %s\n", parsed.Metadata.Subject)
	fmt.Printf("PDF attachments:  %d\n", len(parsed.Attachments))
	fmt.Println()

	// ── Extract text from each PDF attachment ─────────────────────────────
	pdfTexts := make(map[string]string)
	for _, att := range parsed.Attachments {
		fmt.Printf("Extracting text from PDF: %s (%d bytes)\n", att.Filename, len(att.Data))
		text, err := pdfextractor.ExtractText(att.Data)
		if err != nil {
			fmt.Printf("  [WARN] PDF extraction error: %v\n", err)
			text = ""
		}
		if text == "" {
			fmt.Println("  [WARN] No extractable text found — skipping (possibly a scanned PDF)")
		} else {
			fmt.Printf("  Extracted %d characters\n", len(text))
		}
		pdfTexts[att.Filename] = text
	}
	fmt.Println()

	// ── Build context string ──────────────────────────────────────────────
	context := textbuilder.Build(parsed, pdfTexts)
	fmt.Printf("Context assembled: %d characters (cap: %d)\n", len(context), textbuilder.MaxContextChars)
	fmt.Println()

	// ── Initialise AI client ──────────────────────────────────────────────
	prov := aiclient.Provider(*provider)
	var client *aiclient.Client
	if *dryRun {
		fmt.Printf("Mode:   DRY-RUN (no %s API calls)\n\n", prov)
		client = aiclient.NewDryRun(prov)
	} else {
		client, err = aiclient.New(prov)
		if err != nil {
			fatalf("AI client setup failed: %v", err)
		}
		fmt.Printf("Mode:   LIVE  provider=%s  model=%s\n\n", prov, client.ModelName())
	}

	// ── Call AI ───────────────────────────────────────────────────────────
	fmt.Println("Calling AI for structured extraction…")
	start := time.Now()
	data, usage, err := client.Extract(context)
	elapsed := time.Since(start)
	if err != nil {
		fatalf("AI extraction failed: %v", err)
	}

	if data == nil {
		fmt.Println("No data returned (dry-run).")
		return
	}

	// ── Print results ─────────────────────────────────────────────────────
	cost := client.CalculateCost(usage)
	fmt.Printf("Done in %s  |  tokens: %d in / %d out  |  cost: $%.5f\n\n",
		elapsed.Round(time.Millisecond), usage.InputTokens, usage.OutputTokens, cost)

	printResult(data)

	// ── Save results ──────────────────────────────────────────────────────
	result := models.ExtractionResult{
		Data:        data,
		TokenUsage:  usage,
		CostUSD:     cost,
		Provider:    string(prov),
		ModelUsed:   client.ModelName(),
		EmailFile:   *emailFlag,
		ProcessedAt: time.Now().UTC(),
	}
	saveResult(result, *emailFlag)
}

func printResult(data *models.RemittanceData) {
	fmt.Println("┌─────────────────────────────────────────────┐")
	fmt.Println("│              Extraction Result               │")
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Printf("Payer:              %s\n", data.Payer)
	fmt.Printf("Payment Date:       %s\n", data.PaymentDate)
	fmt.Printf("Currency:           %s\n", data.Currency)
	fmt.Printf("Total Amount:       %.2f\n", data.TotalAmount)
	fmt.Printf("Bank Reference:     %s\n", data.BankReferenceNumber)
	fmt.Printf("Invoice Count:      %d\n", data.InvoiceCount)
	fmt.Println()

	if len(data.Invoices) > 0 {
		fmt.Println("Invoices:")
		fmt.Println("  ┌─────────────────┬──────────────┬───────────────────────────────┐")
		fmt.Println("  │ Invoice #        │ Amount       │ Description                   │")
		fmt.Println("  ├─────────────────┼──────────────┼───────────────────────────────┤")
		for _, inv := range data.Invoices {
			desc := inv.Description
			if len(desc) > 29 {
				desc = desc[:26] + "..."
			}
			fmt.Printf("  │ %-15s │ %12.2f │ %-29s │\n", inv.InvoiceNumber, inv.Amount, desc)
		}
		fmt.Println("  └─────────────────┴──────────────┴───────────────────────────────┘")
	}

	if data.Notes != "" {
		fmt.Printf("\nNotes: %s\n", data.Notes)
	}
	fmt.Println()
}

func saveResult(result models.ExtractionResult, emailPath string) {
	os.MkdirAll("results", 0755)
	base := strings.TrimSuffix(filepath.Base(emailPath), filepath.Ext(emailPath))
	filename := fmt.Sprintf("%s_%s.json", base, result.ProcessedAt.Format("20060102_150405"))
	path := filepath.Join("results", filename)

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] Failed to marshal result: %v\n", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] Failed to write result: %v\n", err)
		return
	}
	fmt.Printf("Result saved to: %s\n", path)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
}
