package guardrails

import (
	"fmt"
	"math"

	"cashapp-agent-poc/internal/dataloader"
	"cashapp-agent-poc/internal/models"
)

func Validate(result *models.AgentMatchResult, txn models.BankTransaction, data *dataloader.Data) *models.GuardrailCheck {
	check := &models.GuardrailCheck{
		AmountsBalance:           true,
		IDsExist:                 true,
		InvoicesBelongToCustomer: true,
		DeductionsWithinPolicy:   true,
	}

	if result.Status != "matched" || len(result.Invoices) == 0 {
		check.Passed = true
		return check
	}

	computedTotal := 0.0
	for _, inv := range result.Invoices {
		computedTotal += inv.Applied
	}
	if math.Abs(computedTotal-txn.Amount) > 1.0 && math.Abs(result.TotalApplied-txn.Amount) > 1.0 {
		check.AmountsBalance = false
		check.FailReasons = append(check.FailReasons,
			fmt.Sprintf("applied total $%.2f does not match payment $%.2f", computedTotal, txn.Amount))
	}

	for _, inv := range result.Invoices {
		dbInv := data.GetInvoiceByID(inv.ID)
		if dbInv == nil {
			check.IDsExist = false
			check.FailReasons = append(check.FailReasons,
				fmt.Sprintf("invoice %s does not exist", inv.ID))
			continue
		}
		if dbInv.Status != "open" {
			check.IDsExist = false
			check.FailReasons = append(check.FailReasons,
				fmt.Sprintf("invoice %s is not open (status: %s)", inv.ID, dbInv.Status))
		}
		if dbInv.CustomerID != result.CustomerID {
			check.InvoicesBelongToCustomer = false
			check.FailReasons = append(check.FailReasons,
				fmt.Sprintf("invoice %s belongs to %s, not %s", inv.ID, dbInv.CustomerID, result.CustomerID))
		}
		if inv.Deduction > 0 {
			pct := (inv.Deduction / inv.OriginalAmount) * 100
			if pct > 5.0 {
				check.DeductionsWithinPolicy = false
				check.FailReasons = append(check.FailReasons,
					fmt.Sprintf("deduction on %s is %.1f%% of original — exceeds 5%% policy limit", inv.ID, pct))
			}
		}
	}

	check.Passed = check.AmountsBalance && check.IDsExist &&
		check.InvoicesBelongToCustomer && check.DeductionsWithinPolicy

	return check
}

func Route(result *models.AgentMatchResult, guardrails *models.GuardrailCheck) string {
	if result.Status != "matched" {
		return "manual_investigation"
	}

	if guardrails != nil && !guardrails.Passed {
		return "human_review"
	}

	if result.Confidence >= 0.95 && result.UnmatchedAmount == 0 {
		return "fast_track_review"
	}
	if result.Confidence >= 0.70 {
		return "human_review"
	}
	return "manual_investigation"
}
