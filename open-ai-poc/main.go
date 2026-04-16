package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cashapp-agent-poc/internal/agent"
	"cashapp-agent-poc/internal/dataloader"
	"cashapp-agent-poc/internal/funnel"
	"cashapp-agent-poc/internal/guardrails"
	"cashapp-agent-poc/internal/models"
	"cashapp-agent-poc/internal/multiagent"
	"cashapp-agent-poc/internal/orchestrator"
	"cashapp-agent-poc/internal/orchestratorv2"
	"cashapp-agent-poc/internal/reactive"
	"cashapp-agent-poc/internal/tools"
)

func main() {
	dataDir := flag.String("data", "frost-data", "Path to data directory (shared with linear POC)")
	txnFilter := flag.String("txn", "", "Process a single transaction by ID (e.g., TXN-001)")
	verbose := flag.Bool("verbose", false, "Print full API request and response for every Claude call")
	mode := flag.String("mode", "single", "Agent mode: single, multi, orchestrator, orchestrator-v2, funnel, or reactive")
	flag.Parse()

	loadEnvFile(".env")

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		fmt.Println("ERROR: ANTHROPIC_API_KEY not set.")
		fmt.Println("Copy .env.example to .env and add your key, or export ANTHROPIC_API_KEY=sk-ant-...")
		os.Exit(1)
	}

	data, err := dataloader.Load(*dataDir)
	if err != nil {
		fmt.Printf("ERROR loading data: %v\n", err)
		os.Exit(1)
	}

	toolHandler := &tools.ToolHandler{Data: data}

	transactions := data.BankTransactions
	if *txnFilter != "" {
		var filtered []models.BankTransaction
		for _, txn := range transactions {
			if strings.EqualFold(txn.TxnID, *txnFilter) {
				filtered = append(filtered, txn)
			}
		}
		if len(filtered) == 0 {
			fmt.Printf("ERROR: Transaction %s not found.\n", *txnFilter)
			os.Exit(1)
		}
		transactions = filtered
	}

	modeLabel := "Single Agent"
	switch *mode {
	case "multi":
		modeLabel = "Multi-Agent (3 parallel subagents + merger)"
	case "orchestrator":
		modeLabel = "Multi-Agent Orchestrator (main agent plans → subagents → main agent decides)"
	case "orchestrator-v2":
		modeLabel = "Orchestrator V2 (plan → fetch data locally → analysts reason → synthesize)"
	case "funnel":
		modeLabel = "Amount-Anchored Funnel (pre-fetch → triage → resolve)"
	case "reactive":
		modeLabel = "Reactive Chain (pre-fetch → agent with tools → escalate if needed)"
	}

	fmt.Println("\n========================================")
	fmt.Println(" AGENTIC CASH APPLICATION POC")
	fmt.Printf(" Mode: %s\n", modeLabel)
	fmt.Println("========================================")
	fmt.Println()

	var results []models.PipelineResult
	totalCost := 0.0
	correctCustomer := 0
	correctInvoices := 0
	correctInternalTransfer := 0
	totalProcessed := 0

	for _, txn := range transactions {
		fmt.Printf("\n─── %s: $%.2f from \"%s\" ───\n", txn.TxnID, txn.Amount, txn.CounterpartyName)
		fmt.Printf("  Scenario: %s\n", txn.Scenario)

		var matchResult *models.AgentMatchResult
		var cost float64
		var toolCallCount, totalIn, totalOut int
		var processErr error

		switch *mode {
		case "multi":
			matchResult, cost, toolCallCount, totalIn, totalOut, processErr = runMultiAgentTxn(txn, toolHandler, *verbose)
		case "orchestrator":
			matchResult, cost, toolCallCount, totalIn, totalOut, processErr = runOrchestratorTxn(txn, toolHandler, *verbose)
		case "orchestrator-v2":
			matchResult, cost, toolCallCount, totalIn, totalOut, processErr = runOrchestratorV2Txn(txn, toolHandler, *verbose)
		case "funnel":
			matchResult, cost, toolCallCount, totalIn, totalOut, processErr = runFunnelTxn(txn, toolHandler, *verbose)
		case "reactive":
			matchResult, cost, toolCallCount, totalIn, totalOut, processErr = runReactiveTxn(txn, toolHandler, *verbose)
		default:
			matchResult, cost, toolCallCount, totalIn, totalOut, processErr = runSingleAgentTxn(txn, toolHandler, *verbose)
		}

		if processErr != nil {
			fmt.Printf("  ERROR: %v\n", processErr)
			continue
		}

		totalProcessed++
		totalCost += cost

		var gc *models.GuardrailCheck
		if matchResult.Status == "matched" {
			gc = guardrails.Validate(matchResult, txn, data)
		}

		routing := guardrails.Route(matchResult, gc)

		customerMatch := matchResult.CustomerID == txn.ExpectedCustomer
		invoiceMatch := checkInvoiceMatch(matchResult, txn.ExpectedInvoices)
		internalTransferMatch := matchResult.Status == "internal_transfer" && txn.ExpectedCustomer == ""

		if customerMatch && txn.ExpectedCustomer != "" {
			correctCustomer++
		}
		if invoiceMatch && len(txn.ExpectedInvoices) > 0 {
			correctInvoices++
		}
		if internalTransferMatch {
			correctInternalTransfer++
		}

		pr := models.PipelineResult{
			TxnID:             txn.TxnID,
			Amount:            txn.Amount,
			CounterpartyName:  txn.CounterpartyName,
			Scenario:          txn.Scenario,
			AgentResult:       matchResult,
			Guardrails:        gc,
			RoutingDecision:   routing,
			ToolCallCount:     toolCallCount,
			TotalInputTokens:  totalIn,
			TotalOutputTokens: totalOut,
			TotalCostUSD:      cost,
			MatchesExpected:   customerMatch,
		}
		results = append(results, pr)

		fmt.Printf("  Result: %s", matchResult.Status)
		if matchResult.Status == "matched" {
			fmt.Printf(" → %s (%s)", matchResult.CustomerName, matchResult.CustomerID)
		}
		fmt.Println()
		fmt.Printf("  Confidence: %.2f\n", matchResult.Confidence)
		fmt.Printf("  Tool calls: %d | Tokens: %d in / %d out | Cost: $%.4f\n",
			toolCallCount, totalIn, totalOut, cost)
		fmt.Printf("  Routing: %s\n", routing)

		if gc != nil && !gc.Passed {
			fmt.Printf("  Guardrails FAILED: %s\n", strings.Join(gc.FailReasons, "; "))
		}

		expectedCustStr := txn.ExpectedCustomer
		if expectedCustStr == "" {
			expectedCustStr = "(none)"
		}
		matchSymbol := "✗"
		if customerMatch {
			matchSymbol = "✓"
		}
		if txn.ExpectedCustomer == "" && matchResult.Status == "unresolved" {
			matchSymbol = "✓"
		}
		if internalTransferMatch {
			matchSymbol = "✓"
		}
		fmt.Printf("  Expected: %s %s\n", expectedCustStr, matchSymbol)
	}

	printSummary(results, totalProcessed, correctCustomer, correctInvoices, correctInternalTransfer, totalCost)
	saveResults(results)
}

func runSingleAgentTxn(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (
	match *models.AgentMatchResult, cost float64, toolCallCount, totalIn, totalOut int, err error,
) {
	runResult, runErr := agent.RunAgent(txn, toolHandler, verbose)
	if runErr != nil {
		if runResult != nil && len(runResult.Rounds) > 0 {
			fmt.Printf("  Partial progress: %d round(s) completed before failure\n", len(runResult.Rounds))
			fmt.Printf("  Tokens used so far: %d in / %d out (cache write: %d, read: %d)\n",
				runResult.TotalInputTokens, runResult.TotalOutputTokens,
				runResult.TotalCacheCreationTokens, runResult.TotalCacheReadTokens)
		}
		err = runErr
		return
	}
	match = runResult.Match
	cost = agent.CalculateCost(runResult.TotalInputTokens, runResult.TotalOutputTokens,
		runResult.TotalCacheCreationTokens, runResult.TotalCacheReadTokens)
	toolCallCount = runResult.ToolCallCount
	totalIn = runResult.TotalInputTokens
	totalOut = runResult.TotalOutputTokens
	return
}

func runMultiAgentTxn(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (
	match *models.AgentMatchResult, cost float64, toolCallCount, totalIn, totalOut int, err error,
) {
	maResult, runErr := multiagent.RunMultiAgent(txn, toolHandler, verbose)
	if runErr != nil && maResult == nil {
		err = runErr
		return
	}
	if maResult == nil {
		err = fmt.Errorf("multi-agent returned nil result")
		return
	}
	match = maResult.Match
	cost = multiagent.CalculateMultiAgentCost(maResult.TotalUsage)
	totalIn = maResult.TotalUsage.InputTokens
	totalOut = maResult.TotalUsage.OutputTokens

	for _, sl := range maResult.SubagentLogs {
		fmt.Printf("    %s: %d candidate(s), %d in / %d out\n",
			sl.Name, sl.Candidates, sl.Usage.InputTokens, sl.Usage.OutputTokens)
	}
	return
}

func runOrchestratorTxn(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (
	match *models.AgentMatchResult, cost float64, toolCallCount, totalIn, totalOut int, err error,
) {
	orchResult, runErr := orchestrator.RunOrchestrator(txn, toolHandler, verbose)
	if runErr != nil && orchResult == nil {
		err = runErr
		return
	}
	if orchResult == nil {
		err = fmt.Errorf("orchestrator returned nil result")
		return
	}
	match = orchResult.Match
	cost = orchestrator.CalculateOrchestratorCost(orchResult.TotalUsage)
	totalIn = orchResult.TotalUsage.InputTokens
	totalOut = orchResult.TotalUsage.OutputTokens

	if orchResult.Plan != nil {
		fmt.Printf("    Plan: %s\n", orchResult.Plan.Reasoning)
	}
	for _, sl := range orchResult.SubagentLogs {
		fmt.Printf("    %s: %d candidate(s), %d in / %d out\n",
			sl.Name, sl.Candidates, sl.Usage.InputTokens, sl.Usage.OutputTokens)
	}
	return
}

func runOrchestratorV2Txn(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (
	match *models.AgentMatchResult, cost float64, toolCallCount, totalIn, totalOut int, err error,
) {
	v2Result, runErr := orchestratorv2.RunOrchestratorV2(txn, toolHandler, verbose)
	if runErr != nil && v2Result == nil {
		err = runErr
		return
	}
	if v2Result == nil {
		err = fmt.Errorf("orchestrator-v2 returned nil result")
		return
	}
	match = v2Result.Match
	cost = orchestratorv2.CalculateV2Cost(v2Result.TotalUsage)
	totalIn = v2Result.TotalUsage.InputTokens
	totalOut = v2Result.TotalUsage.OutputTokens

	if v2Result.Plan != nil {
		fmt.Printf("    Plan: %s\n", v2Result.Plan.Reasoning)
	}
	for _, sl := range v2Result.SubagentLogs {
		fmt.Printf("    %s: %d in / %d out\n",
			sl.Name, sl.Usage.InputTokens, sl.Usage.OutputTokens)
	}
	return
}

func runFunnelTxn(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (
	match *models.AgentMatchResult, cost float64, toolCallCount, totalIn, totalOut int, err error,
) {
	funnelResult, runErr := funnel.RunFunnel(txn, toolHandler, verbose)
	if runErr != nil && funnelResult == nil {
		err = runErr
		return
	}
	if funnelResult == nil {
		err = fmt.Errorf("funnel returned nil result")
		return
	}
	match = funnelResult.Match
	cost = funnel.CalculateFunnelCost(funnelResult.TotalUsage)
	toolCallCount = funnelResult.ToolCalls
	totalIn = funnelResult.TotalUsage.InputTokens
	totalOut = funnelResult.TotalUsage.OutputTokens

	fmt.Printf("    Phase: %s | Pre-fetch tools: 2 | Follow-up tools: %d\n",
		funnelResult.Phase, funnelResult.ToolCalls-2)
	return
}

func runReactiveTxn(txn models.BankTransaction, toolHandler *tools.ToolHandler, verbose bool) (
	match *models.AgentMatchResult, cost float64, toolCallCount, totalIn, totalOut int, err error,
) {
	reactiveResult, runErr := reactive.RunReactive(txn, toolHandler, verbose)
	if runErr != nil && reactiveResult == nil {
		err = runErr
		return
	}
	if reactiveResult == nil {
		err = fmt.Errorf("reactive returned nil result")
		return
	}
	match = reactiveResult.Match
	cost = reactive.CalculateReactiveCost(reactiveResult.TotalUsage)
	toolCallCount = reactiveResult.ToolCalls
	totalIn = reactiveResult.TotalUsage.InputTokens
	totalOut = reactiveResult.TotalUsage.OutputTokens

	fmt.Printf("    Phase: %s | Pre-fetch tools: 2 | Agent tool calls: %d\n",
		reactiveResult.Phase, reactiveResult.ToolCalls-2)
	return
}

func checkInvoiceMatch(result *models.AgentMatchResult, expected []string) bool {
	if len(expected) == 0 {
		return true
	}
	if len(result.Invoices) == 0 {
		return false
	}

	matched := make(map[string]bool)
	for _, inv := range result.Invoices {
		matched[inv.ID] = true
	}
	for _, exp := range expected {
		if !matched[exp] {
			return false
		}
	}
	return true
}

func printSummary(results []models.PipelineResult, processed, correctCust, correctInv, correctInternal int, totalCost float64) {
	fmt.Println("\n\n========================================")
	fmt.Println(" PIPELINE RESULTS SUMMARY")
	fmt.Println("========================================")

	matchedCount := 0
	unresolvedCount := 0
	internalTransferCount := 0
	fastTrack := 0
	humanReview := 0
	manualInvestigation := 0
	internalTransfer := 0
	totalToolCalls := 0

	for _, r := range results {
		if r.AgentResult.Status == "matched" {
			matchedCount++
		} else if r.AgentResult.Status == "internal_transfer" {
			internalTransferCount++
		} else {
			unresolvedCount++
		}
		switch r.RoutingDecision {
		case "fast_track_review":
			fastTrack++
		case "human_review":
			humanReview++
		case "manual_investigation":
			manualInvestigation++
		case "internal_transfer":
			internalTransfer++
		}
		totalToolCalls += r.ToolCallCount
	}

	fmt.Printf("\nTransactions processed:    %d\n", processed)
	fmt.Printf("  Matched by agent:       %d  (%.0f%%)\n", matchedCount, pct(matchedCount, processed))
	fmt.Printf("  Internal transfers:     %d  (%.0f%%)\n", internalTransferCount, pct(internalTransferCount, processed))
	fmt.Printf("  Unresolved:             %d  (%.0f%%)\n", unresolvedCount, pct(unresolvedCount, processed))

	fmt.Printf("\nRouting decisions:\n")
	fmt.Printf("  Internal transfer:      %d  (%.0f%%)\n", internalTransfer, pct(internalTransfer, processed))
	fmt.Printf("  Fast-track review:      %d  (%.0f%%)\n", fastTrack, pct(fastTrack, processed))
	fmt.Printf("  Human review:           %d  (%.0f%%)\n", humanReview, pct(humanReview, processed))
	fmt.Printf("  Manual investigation:   %d  (%.0f%%)\n", manualInvestigation, pct(manualInvestigation, processed))

	withExpected := 0
	withExpectedInv := 0
	for _, r := range results {
		if r.CounterpartyName != "" {
			// count transactions that have expected customer
		}
	}
	for _, r := range results {
		txn := findTxn(r.TxnID, results)
		if txn != nil {
			withExpected++
		}
	}
	_ = withExpected
	_ = withExpectedInv

	fmt.Printf("\nAccuracy (vs expected):\n")
	fmt.Printf("  Correct customer ID:      %d/%d\n", correctCust, processed)
	fmt.Printf("  Correct invoice match:    %d/%d\n", correctInv, processed)
	fmt.Printf("  Correct internal transfer: %d/%d\n", correctInternal, processed)

	fmt.Printf("\nAI costs:\n")
	fmt.Printf("  Total Claude API cost:  $%.4f\n", totalCost)
	if processed > 0 {
		fmt.Printf("  Avg cost per txn:       $%.4f\n", totalCost/float64(processed))
	}
	fmt.Printf("  Total tool calls:       %d\n", totalToolCalls)
	if processed > 0 {
		fmt.Printf("  Avg tool calls per txn: %.1f\n", float64(totalToolCalls)/float64(processed))
	}

	fmt.Printf("\n  Projected daily cost (500 txns): $%.2f\n", (totalCost/float64(max(processed, 1)))*500)
	fmt.Printf("  Projected monthly cost:          $%.2f\n", (totalCost/float64(max(processed, 1)))*500*30)
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func findTxn(id string, results []models.PipelineResult) *models.PipelineResult {
	for i := range results {
		if results[i].TxnID == id {
			return &results[i]
		}
	}
	return nil
}

func saveResults(results []models.PipelineResult) {
	os.MkdirAll("results", 0755)
	path := filepath.Join("results", "agent_results.json")
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Printf("ERROR saving results: %v\n", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Printf("ERROR writing results file: %v\n", err)
		return
	}
	fmt.Printf("\nResults saved to %s\n", path)
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
