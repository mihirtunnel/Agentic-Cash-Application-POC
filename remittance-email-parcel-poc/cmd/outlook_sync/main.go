// outlook_sync fetches remittance emails directly from Outlook via the
// Microsoft Graph API and runs them through the AI extraction pipeline.
//
// Two authentication modes are available:
//
// 1. interactive (default for personal @outlook.com accounts):
//    Opens a browser for OAuth2 consent on first run; saves a refresh token
//    so all subsequent runs are fully automatic (no browser).
//
//    go run ./cmd/outlook_sync -auth interactive -client-id <id>
//
// 2. app (for Microsoft 365 work/school accounts):
//    Non-interactive client credentials flow — no browser ever. Requires
//    OUTLOOK_TENANT_ID, OUTLOOK_CLIENT_SECRET, and OUTLOOK_MAILBOX.
//
//    go run ./cmd/outlook_sync -auth app
//
// Other usage examples:
//
//	go run ./cmd/outlook_sync -query "hasAttachments eq true" -max 20
//	go run ./cmd/outlook_sync -provider openai -dry-run
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"remittance-poc/internal/aiclient"
	"remittance-poc/internal/emailparser"
	"remittance-poc/internal/models"
	"remittance-poc/internal/outlookclient"
	"remittance-poc/internal/pdfextractor"
	"remittance-poc/internal/textbuilder"
)

func main() {
	clientID := flag.String("client-id",
		envOrDefault("OUTLOOK_CLIENT_ID", ""),
		"Azure App Registration client ID (or set OUTLOOK_CLIENT_ID in .env)")
	tokenFile := flag.String("token",
		envOrDefault("OUTLOOK_TOKEN_FILE", "outlook_token.json"),
		"Path to store/load the OAuth2 token (created automatically on first run, interactive mode only)")
	tenantID := flag.String("tenant-id",
		envOrDefault("OUTLOOK_TENANT_ID", ""),
		"Azure AD directory/tenant ID — enables app-only auth (or set OUTLOOK_TENANT_ID in .env)")
	clientSecret := flag.String("client-secret",
		envOrDefault("OUTLOOK_CLIENT_SECRET", ""),
		"Azure App client secret value — required for app-only auth (or set OUTLOOK_CLIENT_SECRET in .env)")
	mailbox := flag.String("mailbox",
		envOrDefault("OUTLOOK_MAILBOX", ""),
		"Mailbox email address to read (required for app-only auth, or set OUTLOOK_MAILBOX in .env)")
	query := flag.String("query",
		"hasAttachments eq true",
		"OData $filter expression for the Graph API messages endpoint")
	maxResults := flag.Int("max", 10,
		"Maximum number of emails to fetch and process")
	authMode := flag.String("auth", "",
		"Auth mode: 'interactive' (browser login, personal accounts) or 'app' (client credentials, M365 work accounts). "+
			"Auto-detected from env vars if not set.")
	dryRun := flag.Bool("dry-run", false,
		"Parse emails and build AI context but skip the actual API call")
	provider := flag.String("provider", "claude",
		"AI provider: 'claude' or 'openai'")
	flag.Parse()

	loadEnvFile(".env")

	// Re-check after .env is loaded in case flag defaults were empty.
	if *clientID == "" {
		*clientID = os.Getenv("OUTLOOK_CLIENT_ID")
	}
	if *tenantID == "" {
		*tenantID = os.Getenv("OUTLOOK_TENANT_ID")
	}
	if *clientSecret == "" {
		*clientSecret = os.Getenv("OUTLOOK_CLIENT_SECRET")
	}
	if *mailbox == "" {
		*mailbox = os.Getenv("OUTLOOK_MAILBOX")
	}

	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║ Remittance Outlook Sync — AI Extraction POC ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	if *clientID == "" {
		fatalf("No Outlook client ID provided.\n" +
			"  Pass -client-id <id>  or  set OUTLOOK_CLIENT_ID in your .env file.\n" +
			"  See plan.md Part 1 for how to get your client ID from Azure Portal.")
	}

	// ── Outlook authentication ────────────────────────────────────────────────
	// -auth flag takes precedence; falls back to auto-detect from env vars.
	var appOnly bool
	switch *authMode {
	case "app":
		appOnly = true
	case "interactive":
		appOnly = false
	default:
		appOnly = *tenantID != "" && *clientSecret != ""
	}

	var (
		err        error
		httpClient *http.Client
	)
	if appOnly {
		// Non-interactive: client credentials grant (requires Microsoft 365 work/school account)
		if *mailbox == "" {
			fatalf("App-only auth requires -mailbox <email>  or  OUTLOOK_MAILBOX in .env")
		}
		fmt.Printf("Auth mode:  app-only (client credentials)\n")
		fmt.Printf("Client ID:  %s\n", *clientID)
		fmt.Printf("Tenant ID:  %s\n", *tenantID)
		fmt.Printf("Mailbox:    %s\n\n", *mailbox)
		httpClient, err = outlookclient.GetAppClient(*clientID, *clientSecret, *tenantID)
		if err != nil {
			fatalf("Outlook app-only auth failed: %v", err)
		}
	} else {
		// Interactive: delegated OAuth2 (personal or work accounts)
		fmt.Printf("Auth mode:  interactive (delegated OAuth2)\n")
		fmt.Printf("Client ID:  %s\n", *clientID)
		fmt.Printf("Token file: %s\n\n", *tokenFile)
		httpClient, err = outlookclient.GetClient(*clientID, *tokenFile)
		if err != nil {
			fatalf("Outlook auth failed: %v", err)
		}
	}
	fmt.Println("Outlook authenticated successfully.")
	fmt.Println()

	// ── Fetch emails ──────────────────────────────────────────────────────────
	fmt.Printf("Filter: %s\n", *query)
	fmt.Printf("Limit:  %d email(s)\n\n", *maxResults)

	var messages []outlookclient.RawMessage
	if appOnly {
		messages, err = outlookclient.FetchRawMessagesForMailbox(httpClient, *mailbox, *query, *maxResults)
	} else {
		messages, err = outlookclient.FetchRawMessages(httpClient, *query, *maxResults)
	}
	if err != nil {
		fatalf("Fetching Outlook messages: %v", err)
	}
	if len(messages) == 0 {
		fmt.Println("No emails matched the filter.")
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

func processMessage(msg outlookclient.RawMessage, client *aiclient.Client, prov aiclient.Provider) *models.ExtractionResult {
	parsed, err := emailparser.ParseEML(msg.Raw)
	if err != nil {
		fmt.Printf("  [ERROR] Email parsing: %v\n", err)
		return nil
	}
	fmt.Printf("From:            %s\n", parsed.Metadata.From)
	fmt.Printf("Date:            %s\n", parsed.Metadata.Date.Format("2006-01-02 15:04 MST"))
	fmt.Printf("PDF attachments: %d\n", len(parsed.Attachments))

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

	context := textbuilder.Build(parsed, pdfTexts)
	fmt.Printf("Context:         %d characters\n", len(context))

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
		EmailFile:   fmt.Sprintf("outlook:%s", msg.ID),
		ProcessedAt: time.Now().UTC(),
	}
}

func saveResult(result models.ExtractionResult) {
	id := strings.TrimPrefix(result.EmailFile, "outlook:")
	if len(id) > 20 {
		id = id[:20]
	}
	filename := fmt.Sprintf("outlook_%s_%s.json", id, result.ProcessedAt.Format("20060102_150405"))
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
