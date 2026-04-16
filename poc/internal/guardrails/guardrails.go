package guardrails

import (
	"fmt"
	"math"

	"cashapp-poc/internal/dataloader"
	"cashapp-poc/internal/models"
)

const maxDeductionPercent = 5.0

var matchedInvoices = make(map[string]string) // invoice_id → txn_id

func Validate(txn models.BankTransaction, matchResult *models.InvoiceMatchResult, data *dataloader.Data) models.GuardrailCheck {
	check := models.GuardrailCheck{
		Passed:                 true,
		AmountsBalance:         true,
		IDsExist:               true,
		InvoicesBelongToCust:   true,
		DeductionsWithinPolicy: true,
		NoDoubleMatch:          true,
	}

	// Check 1: Applied amounts sum to payment amount
	totalApplied := 0.0
	for _, inv := range matchResult.Invoices {
		totalApplied += inv.Applied
	}
	totalApplied += matchResult.UnmatchedAmount
	if math.Abs(totalApplied-txn.Amount) > 0.01 {
		check.AmountsBalance = false
		check.Passed = false
		check.FailReasons = append(check.FailReasons,
			fmt.Sprintf("applied ($%.2f) + unmatched ($%.2f) = $%.2f ≠ payment $%.2f",
				totalApplied-matchResult.UnmatchedAmount, matchResult.UnmatchedAmount, totalApplied, txn.Amount))
	}

	// Check 2: All invoice IDs exist
	for _, inv := range matchResult.Invoices {
		dbInv := data.GetInvoiceByID(inv.ID)
		if dbInv == nil {
			check.IDsExist = false
			check.Passed = false
			check.FailReasons = append(check.FailReasons,
				fmt.Sprintf("invoice %s not found in database", inv.ID))
		}
	}

	// Check 3: All invoices belong to the matched customer
	for _, inv := range matchResult.Invoices {
		dbInv := data.GetInvoiceByID(inv.ID)
		if dbInv != nil && dbInv.CustomerID != matchResult.CustomerID {
			check.InvoicesBelongToCust = false
			check.Passed = false
			check.FailReasons = append(check.FailReasons,
				fmt.Sprintf("invoice %s belongs to %s, not %s",
					inv.ID, dbInv.CustomerID, matchResult.CustomerID))
		}
	}

	// Check 4: Deductions within policy
	for _, inv := range matchResult.Invoices {
		if inv.Deduction > 0 && inv.OriginalAmount > 0 {
			pct := (inv.Deduction / inv.OriginalAmount) * 100
			if pct > maxDeductionPercent {
				check.DeductionsWithinPolicy = false
				check.Passed = false
				check.FailReasons = append(check.FailReasons,
					fmt.Sprintf("invoice %s deduction %.1f%% exceeds max %.1f%%",
						inv.ID, pct, maxDeductionPercent))
			}
		}
	}

	// Check 5: No double-matching
	for _, inv := range matchResult.Invoices {
		if prevTxn, exists := matchedInvoices[inv.ID]; exists {
			check.NoDoubleMatch = false
			check.Passed = false
			check.FailReasons = append(check.FailReasons,
				fmt.Sprintf("invoice %s already matched to %s", inv.ID, prevTxn))
		}
	}

	// If passed, record these invoices as matched
	if check.Passed {
		for _, inv := range matchResult.Invoices {
			matchedInvoices[inv.ID] = txn.TxnID
		}
	}

	status := "PASSED"
	if !check.Passed {
		status = "FAILED"
	}
	fmt.Printf("  [GUARDRAILS] %s", status)
	if len(check.FailReasons) > 0 {
		fmt.Printf(" — %v", check.FailReasons)
	}
	fmt.Println()

	return check
}

func ResetState() {
	matchedInvoices = make(map[string]string)
}
