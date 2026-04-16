package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cashapp-poc/internal/aiclient"
	"cashapp-poc/internal/customerid"
	"cashapp-poc/internal/dataloader"
	"cashapp-poc/internal/guardrails"
	"cashapp-poc/internal/invoicematch"
	"cashapp-poc/internal/models"
	"cashapp-poc/internal/prefilter"
	"cashapp-poc/internal/router"
	"cashapp-poc/internal/shortcircuit"
)

func main() {
	txnFlag := flag.String("txn", "", "Process a single transaction by ID (e.g. TXN-001)")
	dryRun := flag.Bool("dry-run", false, "Run without making AI API calls")
	dataDir := flag.String("data", "data", "Path to data directory")
	flag.Parse()

	loadEnvFile(".env")

	fmt.Println("=== Cash Application AI Pipeline — POC ===")
	fmt.Println()

	data, err := dataloader.Load(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load data: %v\n", err)
		os.Exit(1)
	}
	fmt.Println()

	var client *aiclient.Client
	if *dryRun {
		fmt.Println("Mode: DRY-RUN (no AI API calls)")
		client = aiclient.NewDryRun()
	} else {
		client, err = aiclient.New()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create AI client: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Mode: LIVE (calling Claude API)")
	}
	fmt.Println()

	transactions := data.BankTransactions
	if *txnFlag != "" {
		transactions = filterTransaction(data.BankTransactions, *txnFlag)
		if len(transactions) == 0 {
			fmt.Fprintf(os.Stderr, "Transaction %s not found\n", *txnFlag)
			os.Exit(1)
		}
	}

	guardrails.ResetState()

	var results []models.PipelineResult
	for _, txn := range transactions {
		result := processSingleTransaction(txn, data, client)
		results = append(results, result)
	}

	saveResults(results)
	printSummary(results)
}

func processSingleTransaction(txn models.BankTransaction, data *dataloader.Data, client *aiclient.Client) models.PipelineResult {
	fmt.Printf("━━━ Processing %s: $%.2f from %q ━━━\n", txn.TxnID, txn.Amount, txn.CounterpartyName)
	fmt.Printf("  Scenario: %s\n", txn.Scenario)

	result := models.PipelineResult{TxnID: txn.TxnID}

	// Step 1: Short-circuit check
	sc := shortcircuit.Check(txn, data)
	if sc.Matched && !sc.SkipCall1 {
		result.PipelinePath = "short_circuit"
		result.ShortCircuit = &models.ShortCircuitResult{
			Type:       sc.Type,
			CustomerID: sc.CustomerID,
			InvoiceID:  sc.InvoiceID,
		}
		result.RoutingDecision = router.AutoPost
		result.MatchesExpected = checkExpected(txn, sc.CustomerID, []string{sc.InvoiceID})
		fmt.Printf("  [ROUTER] → AUTO-POST (short-circuit: %s)\n", sc.Type)
		fmt.Println()
		return result
	}

	// Step 2: Pre-filter candidates
	var candidates []models.Candidate
	var matchedCustomerID string
	var call1Result *models.Call1Result

	if sc.Matched && sc.SkipCall1 {
		// Single exact match — skip Call 1, customer is known
		result.PipelinePath = "skip_call1"
		matchedCustomerID = sc.CustomerID
		result.ShortCircuit = &models.ShortCircuitResult{
			Type:       sc.Type,
			CustomerID: sc.CustomerID,
			InvoiceID:  sc.InvoiceID,
		}
		fmt.Printf("  Skipping Call 1 — customer known: %s\n", sc.CustomerID)
	} else {
		candidates = prefilter.FindCandidates(txn, data)
		if len(candidates) == 0 {
			result.PipelinePath = "no_candidates"
			result.RoutingDecision = router.ManualInvestigation
			result.MatchesExpected = txn.ExpectedCustomer == ""
			fmt.Println("  [ROUTER] → MANUAL INVESTIGATION (no candidates found)")
			fmt.Println()
			return result
		}

		result.PreFilter = &models.PreFilterResult{
			CandidatesFound: len(candidates),
			TopCandidate:    candidates[0].Customer.CustomerID,
			TopScore:        candidates[0].Score,
		}

		// Step 3: AI Call 1 — Customer Identification
		var err error
		call1Result, err = customerid.Call(client, txn, candidates)
		if err != nil {
			fmt.Printf("  [ERROR] %v\n", err)
			result.PipelinePath = "error"
			result.RoutingDecision = router.ManualInvestigation
			fmt.Println()
			return result
		}
		if call1Result == nil {
			// dry-run
			result.PipelinePath = "dry_run"
			result.RoutingDecision = "dry_run"
			fmt.Println()
			return result
		}

		result.Call1 = call1Result
		result.TotalCostUSD += call1Result.CostUSD

		// Confidence gate
		if call1Result.Confidence < 0.70 {
			result.PipelinePath = "call1_low_confidence"
			result.RoutingDecision = router.Route(call1Result.Confidence, nil, 0)
			result.MatchesExpected = txn.ExpectedCustomer == ""
			fmt.Println()
			return result
		}

		matchedCustomerID = call1Result.CustomerID
		result.PipelinePath = "full_chain"
	}

	// Step 4: Fetch customer data for Call 2
	cust := data.GetCustomerByID(matchedCustomerID)
	if cust == nil {
		fmt.Printf("  [ERROR] Customer %s not found in data\n", matchedCustomerID)
		result.PipelinePath = "error"
		result.RoutingDecision = router.ManualInvestigation
		fmt.Println()
		return result
	}

	invoices := data.GetInvoicesForCustomer(matchedCustomerID)
	email := data.FindRemittanceEmail(matchedCustomerID, txn.Amount)
	if email != nil {
		fmt.Printf("  [DATA] Found remittance email: %s from %s\n", email.EmailID, email.SenderName)
	} else {
		fmt.Println("  [DATA] No remittance email found")
	}

	// Step 5: AI Call 2 — Invoice Matching
	call2Result, err := invoicematch.Call(client, txn, *cust, invoices, email)
	if err != nil {
		fmt.Printf("  [ERROR] %v\n", err)
		result.RoutingDecision = router.ManualInvestigation
		fmt.Println()
		return result
	}
	if call2Result == nil {
		result.RoutingDecision = "dry_run"
		fmt.Println()
		return result
	}

	result.Call2 = call2Result
	result.TotalCostUSD += call2Result.CostUSD

	// Step 6: Guardrails
	guardrailCheck := guardrails.Validate(txn, &call2Result.InvoiceMatchResult, data)
	result.Guardrails = &guardrailCheck

	// Step 7: Route
	result.RoutingDecision = router.Route(call2Result.Confidence, &guardrailCheck, call2Result.UnmatchedAmount)

	// Check against expected
	matchedInvIDs := make([]string, len(call2Result.Invoices))
	for i, inv := range call2Result.Invoices {
		matchedInvIDs[i] = inv.ID
	}
	result.MatchesExpected = checkExpected(txn, call2Result.CustomerID, matchedInvIDs)

	fmt.Println()
	return result
}

func checkExpected(txn models.BankTransaction, customerID string, invoiceIDs []string) bool {
	if txn.ExpectedCustomer == "" && customerID == "" {
		return true
	}
	if txn.ExpectedCustomer != customerID {
		return false
	}
	if len(txn.ExpectedInvoices) == 0 {
		return true
	}
	expected := make(map[string]bool)
	for _, id := range txn.ExpectedInvoices {
		expected[id] = true
	}
	got := make(map[string]bool)
	for _, id := range invoiceIDs {
		got[id] = true
	}
	if len(expected) != len(got) {
		return false
	}
	for id := range expected {
		if !got[id] {
			return false
		}
	}
	return true
}

func filterTransaction(txns []models.BankTransaction, id string) []models.BankTransaction {
	upper := strings.ToUpper(id)
	for _, txn := range txns {
		if strings.ToUpper(txn.TxnID) == upper {
			return []models.BankTransaction{txn}
		}
	}
	return nil
}

func saveResults(results []models.PipelineResult) {
	os.MkdirAll("results", 0755)
	path := filepath.Join("results", "pipeline_results.json")
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal results: %v\n", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write results: %v\n", err)
		return
	}
	fmt.Printf("Results saved to %s\n", path)
}

func printSummary(results []models.PipelineResult) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║       Pipeline Results Summary           ║")
	fmt.Println("╚══════════════════════════════════════════╝")
	fmt.Println()

	total := len(results)
	shortCircuited := 0
	call1Only := 0
	fullChain := 0
	skipCall1 := 0
	noCandidates := 0
	dryRuns := 0
	errors := 0

	autoPost := 0
	humanReview := 0
	manualInv := 0

	correctCustomer := 0
	totalAICost := 0.0
	guardrailFails := 0

	for _, r := range results {
		switch r.PipelinePath {
		case "short_circuit":
			shortCircuited++
		case "call1_low_confidence":
			call1Only++
		case "full_chain":
			fullChain++
		case "skip_call1":
			skipCall1++
		case "no_candidates":
			noCandidates++
		case "dry_run":
			dryRuns++
		case "error":
			errors++
		}

		switch r.RoutingDecision {
		case router.AutoPost:
			autoPost++
		case router.HumanReview:
			humanReview++
		case router.ManualInvestigation:
			manualInv++
		}

		if r.MatchesExpected {
			correctCustomer++
		}
		totalAICost += r.TotalCostUSD

		if r.Guardrails != nil && !r.Guardrails.Passed {
			guardrailFails++
		}
	}

	fmt.Printf("Transactions processed:     %d\n", total)
	fmt.Printf("Short-circuited (no AI):    %d  (%.0f%%)\n", shortCircuited, pct(shortCircuited, total))
	fmt.Printf("Skip Call 1 (known cust):   %d  (%.0f%%)\n", skipCall1, pct(skipCall1, total))
	fmt.Printf("Full chain (Call 1 + 2):    %d  (%.0f%%)\n", fullChain, pct(fullChain, total))
	fmt.Printf("Call 1 only (low conf):     %d  (%.0f%%)\n", call1Only, pct(call1Only, total))
	fmt.Printf("No candidates found:        %d  (%.0f%%)\n", noCandidates, pct(noCandidates, total))
	if dryRuns > 0 {
		fmt.Printf("Dry-run (no API call):      %d\n", dryRuns)
	}
	if errors > 0 {
		fmt.Printf("Errors:                     %d\n", errors)
	}
	fmt.Println()

	fmt.Println("Routing decisions:")
	fmt.Printf("  Auto-post:                %d  (%.0f%%)\n", autoPost, pct(autoPost, total))
	fmt.Printf("  Human review:             %d  (%.0f%%)\n", humanReview, pct(humanReview, total))
	fmt.Printf("  Manual investigation:     %d  (%.0f%%)\n", manualInv, pct(manualInv, total))
	fmt.Println()

	fmt.Printf("Accuracy (vs expected):     %d/%d  (%.0f%%)\n", correctCustomer, total, pct(correctCustomer, total))
	fmt.Printf("Guardrail failures:         %d\n", guardrailFails)
	fmt.Println()

	fmt.Printf("Total AI cost:              $%.4f\n", totalAICost)
	if total > 0 {
		fmt.Printf("Avg cost per transaction:   $%.4f\n", totalAICost/float64(total))
	}
	fmt.Println()

	fmt.Println("Per-transaction breakdown:")
	fmt.Println("  TXN ID     | Path              | Routing             | Cost     | Correct")
	fmt.Println("  -----------|-------------------|---------------------|----------|--------")
	for _, r := range results {
		correct := "✓"
		if !r.MatchesExpected {
			correct = "✗"
		}
		fmt.Printf("  %-10s | %-17s | %-19s | $%.4f | %s\n",
			r.TxnID, r.PipelinePath, r.RoutingDecision, r.TotalCostUSD, correct)
	}
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
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
