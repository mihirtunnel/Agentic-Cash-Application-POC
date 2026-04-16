package shortcircuit

import (
	"fmt"
	"math"
	"strings"

	"cashapp-poc/internal/dataloader"
	"cashapp-poc/internal/models"
)

type Result struct {
	Matched    bool
	Type       string
	CustomerID string
	InvoiceID  string
	SkipCall1  bool // true when customer is known but still need Call 2 for invoice match
}

func Check(txn models.BankTransaction, data *dataloader.Data) Result {
	// Check 1: Exact account number match + exact amount
	if txn.CounterpartyAccount != "" {
		if r := checkAccountMatch(txn, data); r.Matched {
			fmt.Printf("  [SHORT-CIRCUIT] Account match: %s → %s\n", txn.CounterpartyAccount, r.CustomerID)
			return r
		}
	}

	// Check 2: Invoice reference in bank reference or remittance info
	if r := checkInvoiceReference(txn, data); r.Matched {
		fmt.Printf("  [SHORT-CIRCUIT] Invoice ref match: %s\n", r.InvoiceID)
		return r
	}

	// Check 3: Single customer with single invoice exact amount match
	if r := checkSingleExactMatch(txn, data); r.Matched {
		fmt.Printf("  [SHORT-CIRCUIT] Single exact match: %s → %s (skip Call 1)\n", r.CustomerID, r.InvoiceID)
		return r
	}

	return Result{Matched: false}
}

func checkAccountMatch(txn models.BankTransaction, data *dataloader.Data) Result {
	for _, alias := range data.Aliases {
		for _, acct := range alias.BankAccounts {
			if strings.EqualFold(acct, txn.CounterpartyAccount) {
				invoices := data.GetInvoicesForCustomer(alias.CustomerID)
				for _, inv := range invoices {
					if math.Abs(inv.Amount-txn.Amount) < 0.01 {
						return Result{
							Matched:    true,
							Type:       "account_and_amount",
							CustomerID: alias.CustomerID,
							InvoiceID:  inv.InvoiceID,
						}
					}
				}
			}
		}
	}
	return Result{}
}

func checkInvoiceReference(txn models.BankTransaction, data *dataloader.Data) Result {
	searchTexts := []string{txn.BankReference, txn.RemittanceInfo}
	for _, text := range searchTexts {
		if text == "" {
			continue
		}
		upper := strings.ToUpper(text)
		for _, inv := range data.Invoices {
			if inv.Status != "open" {
				continue
			}
			if strings.Contains(upper, strings.ToUpper(inv.InvoiceID)) {
				if math.Abs(inv.Amount-txn.Amount) < 0.01 {
					return Result{
						Matched:    true,
						Type:       "invoice_reference",
						CustomerID: inv.CustomerID,
						InvoiceID:  inv.InvoiceID,
					}
				}
			}
		}
	}
	return Result{}
}

func checkSingleExactMatch(txn models.BankTransaction, data *dataloader.Data) Result {
	var matches []models.Invoice
	for _, inv := range data.Invoices {
		if inv.Status == "open" && math.Abs(inv.Amount-txn.Amount) < 0.01 {
			matches = append(matches, inv)
		}
	}
	if len(matches) == 1 {
		return Result{
			Matched:    true,
			Type:       "single_exact_match",
			CustomerID: matches[0].CustomerID,
			InvoiceID:  matches[0].InvoiceID,
			SkipCall1:  true,
		}
	}
	return Result{}
}
