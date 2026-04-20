// gmail_sync fetches emails from the last N hours via the Gmail API,
// classifies each one as remittance or not, and runs the AI extraction
// pipeline on any remittance email — saving results to results/.
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
	"remittance-poc/internal/gmailclient"
	"remittance-poc/internal/models"
	"remittance-poc/internal/pdfextractor"
	"remittance-poc/internal/textbuilder"
)

// EmailSummary captures the outcome for a single fetched email.
type EmailSummary struct {
	MessageID    string    `json:"message_id"`
	From         string    `json:"from"`
	Subject      string    `json:"subject"`
	Date         time.Time `json:"date"`
	IsRemittance bool      `json:"is_remittance"`
	ProcessedAt  time.Time `json:"processed_at"`
	ResultFile   string    `json:"result_file,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// SyncSummary is written at the end of each run.
type SyncSummary struct {
	FetchedAt       time.Time      `json:"fetched_at"`
	EmailsSince     time.Time      `json:"emails_since"`
	TotalEmails     int            `json:"total_emails"`
	RemittanceCount int            `json:"remittance_count"`
	Provider        string         `json:"provider"`
	ModelUsed       string         `json:"model_used"`
	Emails          []EmailSummary `json:"emails"`
}

func main() {
	credFile := flag.String("credentials", "gmail_credentials.json",
		"Path to Google OAuth2 client credentials JSON (from Cloud Console)")
	tokenFile := flag.String("token", "gmail_token.json",
		"Path to saved OAuth2 user token (created on first run)")
	hours := flag.Int("hours", 2,
		"How many hours back to look for emails")
	provider := flag.String("provider", "claude",
		"AI provider: 'claude' or 'openai'")
	dryRun := flag.Bool("dry-run", false,
		"Classify emails but skip AI extraction calls")
	flag.Parse()

	loadEnvFile(".env")

	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║   Gmail Remittance Sync — POC                    ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	// ── Connect to Gmail ──────────────────────────────────────────────────
	fmt.Printf("Connecting to Gmail (credentials: %s) …\n", *credFile)
	gc, err := gmailclient.New(*credFile, *tokenFile)
	if err != nil {
		fatalf("Gmail setup failed: %v\n\nMake sure you have:\n"+
			"  1. Created OAuth2 credentials in Google Cloud Console\n"+
			"  2. Downloaded them as JSON to %q\n"+
			"  3. Enabled the Gmail API for your project\n", err, *credFile)
	}
	fmt.Println("Gmail connected.")
	fmt.Println()

	// ── Fetch emails ──────────────────────────────────────────────────────
	since := time.Now().UTC().Add(-time.Duration(*hours) * time.Hour)
	fmt.Printf("Fetching emails since: %s  (last %d hour(s))\n\n",
		since.Format("2006-01-02 15:04:05 UTC"), *hours)

	emails, messageIDs, err := gc.FetchEmailsSince(since)
	if err != nil {
		fatalf("Failed to fetch emails: %v", err)
	}
	fmt.Printf("Found %d email(s)\n\n", len(emails))

	if len(emails) == 0 {
		fmt.Println("No emails in the specified window. Done.")
		return
	}

	// ── Set up AI client ──────────────────────────────────────────────────
	prov := aiclient.Provider(*provider)
	var ai *aiclient.Client
	if *dryRun {
		fmt.Printf("Mode: DRY-RUN (no %s API calls)\n\n", prov)
		ai = aiclient.NewDryRun(prov)
	} else {
		ai, err = aiclient.New(prov)
		if err != nil {
			fatalf("AI client setup failed: %v", err)
		}
		fmt.Printf("Mode: LIVE  provider=%s  model=%s\n\n", prov, ai.ModelName())
	}

	// ── Process each email ────────────────────────────────────────────────
	summary := SyncSummary{
		FetchedAt:   time.Now().UTC(),
		EmailsSince: since,
		TotalEmails: len(emails),
		Provider:    string(prov),
		ModelUsed:   ai.ModelName(),
	}

	for i, parsed := range emails {
		msgID := messageIDs[i]

		fmt.Printf("─── [%d/%d] ──────────────────────────────────────────\n",
			i+1, len(emails))
		fmt.Printf("From:    %s\n", parsed.Metadata.From)
		fmt.Printf("Subject: %s\n", parsed.Metadata.Subject)
		fmt.Printf("Date:    %s\n", parsed.Metadata.Date.Format("2006-01-02 15:04:05 UTC"))
		fmt.Printf("PDFs:    %d attachment(s)\n", len(parsed.Attachments))

		es := EmailSummary{
			MessageID:   msgID,
			From:        parsed.Metadata.From,
			Subject:     parsed.Metadata.Subject,
			Date:        parsed.Metadata.Date,
			ProcessedAt: time.Now().UTC(),
		}

		// ── Classify ──────────────────────────────────────────────────────
		isRemittance := gmailclient.IsRemittanceEmail(parsed)
		es.IsRemittance = isRemittance

		if !isRemittance {
			fmt.Println("Classification: NOT a remittance email — skipped")
			fmt.Println()
			summary.Emails = append(summary.Emails, es)
			continue
		}

		fmt.Println("Classification: REMITTANCE email — extracting…")
		summary.RemittanceCount++

		// ── Extract PDF text ──────────────────────────────────────────────
		pdfTexts := make(map[string]string)
		for _, att := range parsed.Attachments {
			fmt.Printf("  Extracting PDF: %s (%d bytes)\n", att.Filename, len(att.Data))
			text, err := pdfextractor.ExtractText(att.Data)
			if err != nil {
				fmt.Printf("  [WARN] PDF extraction error: %v\n", err)
			}
			if text == "" {
				fmt.Println("  [WARN] No extractable text (possibly a scanned PDF)")
			} else {
				fmt.Printf("  Extracted %d chars from %s\n", len(text), att.Filename)
			}
			pdfTexts[att.Filename] = text
		}

		// ── Build LLM context ─────────────────────────────────────────────
		context := textbuilder.Build(parsed, pdfTexts)
		fmt.Printf("  Context: %d chars (cap: %d)\n", len(context), textbuilder.MaxContextChars)

		// ── Call AI ───────────────────────────────────────────────────────
		fmt.Println("  Calling AI for structured extraction…")
		start := time.Now()
		data, usage, err := ai.Extract(context)
		elapsed := time.Since(start)

		if err != nil {
			fmt.Printf("  [ERROR] AI extraction failed: %v\n\n", err)
			es.Error = err.Error()
			summary.Emails = append(summary.Emails, es)
			continue
		}

		if data == nil {
			fmt.Println("  No data (dry-run).")
			fmt.Println()
			summary.Emails = append(summary.Emails, es)
			continue
		}

		cost := ai.CalculateCost(usage)
		fmt.Printf("  Done in %s | tokens: %d in / %d out | cost: $%.5f\n",
			elapsed.Round(time.Millisecond), usage.InputTokens, usage.OutputTokens, cost)
		fmt.Printf("  Payer: %q  |  Amount: %.2f %s  |  Invoices: %d\n",
			data.Payer, data.TotalAmount, data.Currency, data.InvoiceCount)

		// ── Save extraction result ─────────────────────────────────────────
		result := models.ExtractionResult{
			Data:        data,
			TokenUsage:  usage,
			CostUSD:     cost,
			Provider:    string(prov),
			ModelUsed:   ai.ModelName(),
			EmailFile:   fmt.Sprintf("gmail-message-id:%s", msgID),
			ProcessedAt: time.Now().UTC(),
		}
		resultPath := saveExtractionResult(result, parsed.Metadata.Subject, msgID)
		es.ResultFile = resultPath
		fmt.Printf("  Saved: %s\n\n", resultPath)

		summary.Emails = append(summary.Emails, es)
	}

	// ── Save run summary ──────────────────────────────────────────────────
	summaryPath := saveSyncSummary(summary)

	fmt.Println("════════════════════════════════════════════════════")
	fmt.Printf("Processed %d email(s) — %d remittance(s) extracted.\n",
		summary.TotalEmails, summary.RemittanceCount)
	fmt.Printf("Summary: %s\n", summaryPath)
}

// saveExtractionResult writes a single email's extraction result to results/.
func saveExtractionResult(result models.ExtractionResult, subject, msgID string) string {
	os.MkdirAll("results", 0755) //nolint:errcheck
	safe := sanitizeFilename(subject)
	if safe == "" {
		safe = "msg_" + msgID[:8]
	}
	filename := fmt.Sprintf("gmail_%s_%s.json",
		safe, result.ProcessedAt.UTC().Format("20060102_150405"))
	path := filepath.Join("results", filename)

	out, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(path, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] Failed to save result: %v\n", err)
	}
	return path
}

// saveSyncSummary writes the overall run summary to results/.
func saveSyncSummary(s SyncSummary) string {
	os.MkdirAll("results", 0755) //nolint:errcheck
	filename := fmt.Sprintf("gmail_sync_summary_%s.json",
		s.FetchedAt.UTC().Format("20060102_150405"))
	path := filepath.Join("results", filename)

	out, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(path, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] Failed to save summary: %v\n", err)
	}
	return path
}

// sanitizeFilename converts a string into a safe filename segment (≤ 40 chars).
func sanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
		if sb.Len() >= 40 {
			break
		}
	}
	return strings.Trim(sb.String(), "_")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\nERROR: "+format+"\n", args...)
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
