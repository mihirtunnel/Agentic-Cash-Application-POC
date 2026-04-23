// gmail_sync fetches remittance emails directly from Gmail and runs them
// through the AI extraction pipeline.
//
// Usage:
//
//	go run ./cmd/gmail_sync
//	go run ./cmd/gmail_sync -credentials ./gmial-client_secret_...json
//	go run ./cmd/gmail_sync -query "subject:remittance" -max 20
//	go run ./cmd/gmail_sync -provider openai -dry-run
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
	"remittance-poc/internal/gmailclient"
	"remittance-poc/internal/models"
	"remittance-poc/internal/pdfextractor"
	"remittance-poc/internal/textbuilder"
)

func main() {
	credFile := flag.String("credentials",
		envOrDefault("GMAIL_CREDENTIALS_FILE", "gmail_credentials.json"),
		"Path to Gmail OAuth2 credentials JSON (downloaded from Google Cloud Console)")
	tokenFile := flag.String("token",
		envOrDefault("GMAIL_TOKEN_FILE", "gmail_token.json"),
		"Path to store/load the OAuth2 token (created automatically on first run)")
	query := flag.String("query",
		"has:attachment filename:pdf",
		"Gmail search query — same syntax as the Gmail search box")
	maxResults := flag.Int64("max", 10,
		"Maximum number of emails to fetch and process")
	dryRun := flag.Bool("dry-run", false,
		"Parse emails and build AI context but skip the actual API call")
	provider := flag.String("provider", "claude",
		"AI provider: 'claude' or 'openai'")
	flag.Parse()

	loadEnvFile(".env")

	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║  Remittance Gmail Sync — AI Extraction POC  ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	// ── Gmail authentication ──────────────────────────────────────────────────
	fmt.Printf("Credentials file: %s\n", *credFile)
	fmt.Printf("Token file:       %s\n\n", *tokenFile)

	svc, err := gmailclient.GetService(*credFile, *tokenFile)
	if err != nil {
		fatalf("Gmail auth failed: %v", err)
	}
	fmt.Println("Gmail authenticated successfully.")
	fmt.Println()

	// ── Fetch emails ──────────────────────────────────────────────────────────
	fmt.Printf("Query:  %s\n", *query)
	fmt.Printf("Limit:  %d email(s)\n\n", *maxResults)

	messages, err := gmailclient.FetchRawMessages(svc, *query, *maxResults)
	if err != nil {
		fatalf("Fetching Gmail messages: %v", err)
	}
	if len(messages) == 0 {
		fmt.Println("No emails matched the query.")
		return
	}
	fmt.Printf("Found %d email(s) to process.\n\n", len(messages))

	// ── AI client ─────────────────────────────────────────────────────────────
	prov := aiclient.Provider(*provider)
	var client *aiclient.Client
	if *dryRun {
		fmt.Printf("Mode: DRY-RUN (no %s API calls)\n\n", prov)
		client = aiclient.NewDryRun(prov)
	} else {
		client, err = aiclient.New(prov)
		if err != nil {
			fatalf("AI client setup: %v", err)
		}
		fmt.Printf("Mode: LIVE  provider=%s  model=%s\n\n", prov, client.ModelName())
	}

	// ── Process ───────────────────────────────────────────────────────────────
	os.MkdirAll("results", 0755)

	var totalCost float64
	processed := 0

	for i, msg := range messages {
		fmt.Printf("━━━ [%d/%d] %s ━━━\n", i+1, len(messages), msg.ID)
		if msg.Subject != "" {
			fmt.Printf("Subject: %s\n", msg.Subject)
		}

		result := processMessage(msg, client, prov)
		if result != nil {
			totalCost += result.CostUSD
			saveResult(*result)
			processed++
		}
		fmt.Println()
	}

	// ── Summary ───────────────────────────────────────────────────────────────
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║                   Summary                    ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Printf("Emails fetched:    %d\n", len(messages))
	fmt.Printf("Successfully proc: %d\n", processed)
	fmt.Printf("Total AI cost:     $%.5f\n", totalCost)
}

func processMessage(msg gmailclient.RawMessage, client *aiclient.Client, prov aiclient.Provider) *models.ExtractionResult {
	// Parse raw RFC 2822 email.
	parsed, err := emailparser.ParseEML(msg.Raw)
	if err != nil {
		fmt.Printf("  [ERROR] Email parsing: %v\n", err)
		return nil
	}
	fmt.Printf("From:            %s\n", parsed.Metadata.From)
	fmt.Printf("Date:            %s\n", parsed.Metadata.Date.Format("2006-01-02 15:04 MST"))
	fmt.Printf("PDF attachments: %d\n", len(parsed.Attachments))

	// Extract text from each PDF attachment.
	pdfTexts := make(map[string]string)
	for _, att := range parsed.Attachments {
		fmt.Printf("  Extracting: %s (%d bytes)\n", att.Filename, len(att.Data))
		text, err := pdfextractor.ExtractText(att.Data)
		if err != nil {
			fmt.Printf("    [WARN] PDF extraction error: %v\n", err)
		}
		if text == "" {
			fmt.Println("    [WARN] No extractable text — possibly a scanned/image-only PDF")
		} else {
			fmt.Printf("    Extracted %d characters\n", len(text))
		}
		pdfTexts[att.Filename] = text
	}

	// Assemble context string.
	context := textbuilder.Build(parsed, pdfTexts)
	fmt.Printf("Context:         %d characters\n", len(context))

	// Call AI.
	fmt.Println("Calling AI…")
	start := time.Now()
	data, usage, err := client.Extract(context)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Printf("  [ERROR] AI extraction: %v\n", err)
		return nil
	}
	if data == nil {
		fmt.Println("  [DRY-RUN] No data returned.")
		return nil
	}

	cost := client.CalculateCost(usage)
	fmt.Printf("Done in %s  |  %d in / %d out tokens  |  $%.5f\n",
		elapsed.Round(time.Millisecond), usage.InputTokens, usage.OutputTokens, cost)

	// Print key fields.
	fmt.Printf("  Payer:      %s\n", data.Payer)
	fmt.Printf("  Date:       %s\n", data.PaymentDate)
	fmt.Printf("  Total:      %.2f %s\n", data.TotalAmount, data.Currency)
	fmt.Printf("  Bank Ref:   %s\n", data.BankReferenceNumber)
	fmt.Printf("  Invoices:   %d\n", data.InvoiceCount)

	return &models.ExtractionResult{
		Data:        data,
		TokenUsage:  usage,
		CostUSD:     cost,
		Provider:    string(prov),
		ModelUsed:   client.ModelName(),
		EmailFile:   fmt.Sprintf("gmail:%s", msg.ID),
		ProcessedAt: time.Now().UTC(),
	}
}

func saveResult(result models.ExtractionResult) {
	id := strings.TrimPrefix(result.EmailFile, "gmail:")
	filename := fmt.Sprintf("gmail_%s_%s.json", id, result.ProcessedAt.Format("20060102_150405"))
	path := filepath.Join("results", filename)

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [WARN] marshal result: %v\n", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "  [WARN] save result: %v\n", err)
		return
	}
	fmt.Printf("  Result saved: %s\n", path)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
