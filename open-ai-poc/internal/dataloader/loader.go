package dataloader

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"cashapp-agent-poc/internal/models"
)

type Data struct {
	Customers        []models.Customer
	Invoices         []models.Invoice
	RemittanceEmails []models.RemittanceEmail
	BankTransactions []models.BankTransaction
}

func Load(dataDir string) (*Data, error) {
	d := &Data{}

	if err := loadJSON(filepath.Join(dataDir, "customers.json"), &d.Customers); err != nil {
		return nil, fmt.Errorf("loading customers: %w", err)
	}
	if err := loadJSON(filepath.Join(dataDir, "invoices.json"), &d.Invoices); err != nil {
		return nil, fmt.Errorf("loading invoices: %w", err)
	}
	if err := loadJSON(filepath.Join(dataDir, "remittance_emails.json"), &d.RemittanceEmails); err != nil {
		return nil, fmt.Errorf("loading remittance emails: %w", err)
	}
	if err := loadJSON(filepath.Join(dataDir, "bank_transactions.json"), &d.BankTransactions); err != nil {
		return nil, fmt.Errorf("loading bank transactions: %w", err)
	}

	fmt.Printf("Loaded: %d customers, %d invoices, %d emails, %d transactions\n",
		len(d.Customers), len(d.Invoices),
		len(d.RemittanceEmails), len(d.BankTransactions))

	return d, nil
}

func (d *Data) GetCustomerByID(id string) *models.Customer {
	for i := range d.Customers {
		if d.Customers[i].CustomerID == id {
			return &d.Customers[i]
		}
	}
	return nil
}

func (d *Data) GetInvoicesForCustomer(customerID string) []models.Invoice {
	var result []models.Invoice
	for _, inv := range d.Invoices {
		if inv.CustomerID == customerID && inv.Status == "open" {
			result = append(result, inv)
		}
	}
	return result
}

func (d *Data) GetInvoiceByID(id string) *models.Invoice {
	for i := range d.Invoices {
		if d.Invoices[i].InvoiceID == id {
			return &d.Invoices[i]
		}
	}
	return nil
}

func (d *Data) SearchCustomersByName(query string, limit int) []CustomerMatch {
	normalized := normalize(query)
	var results []CustomerMatch

	for _, cust := range d.Customers {
		score := trigramSimilarity(normalized, normalize(cust.Name))
		if score >= 0.15 {
			results = append(results, CustomerMatch{
				CustomerID: cust.CustomerID,
				Name:       cust.Name,
				Score:      score,
			})
		}
	}

	sortByScore(results)
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func (d *Data) SearchCustomersByAmount(amount float64, tolerancePct float64) []AmountMatch {
	tolerance := amount * (tolerancePct / 100.0)
	var results []AmountMatch

	customerInvoices := make(map[string][]models.Invoice)
	for _, inv := range d.Invoices {
		if inv.Status == "open" {
			customerInvoices[inv.CustomerID] = append(customerInvoices[inv.CustomerID], inv)
		}
	}

	for custID, invoices := range customerInvoices {
		combos := findCombinations(invoices, amount, tolerance)
		if len(combos) > 0 {
			cust := d.GetCustomerByID(custID)
			if cust != nil {
				for _, combo := range combos {
					total := 0.0
					var ids []string
					for _, inv := range combo {
						total += inv.Amount
						ids = append(ids, inv.InvoiceID)
					}
					results = append(results, AmountMatch{
						CustomerID:  cust.CustomerID,
						CustomerName: cust.Name,
						InvoiceIDs:  ids,
						InvoiceTotal: total,
						Difference:  total - amount,
						DiffPercent: ((total - amount) / amount) * 100,
					})
				}
			}
		}
	}
	return results
}

func (d *Data) SearchRemittanceEmails(amountMin, amountMax float64, dateFrom, dateTo, senderKeyword, invoiceRef string) []models.RemittanceEmail {
	var results []models.RemittanceEmail
	for _, email := range d.RemittanceEmails {
		if amountMin > 0 && email.TotalAmount < amountMin {
			continue
		}
		if amountMax > 0 && email.TotalAmount > amountMax {
			continue
		}
		if dateFrom != "" && email.ReceivedDate < dateFrom {
			continue
		}
		if dateTo != "" && email.ReceivedDate > dateTo {
			continue
		}
		if senderKeyword != "" {
			kw := strings.ToUpper(senderKeyword)
			if !strings.Contains(strings.ToUpper(email.SenderName), kw) &&
				!strings.Contains(strings.ToUpper(email.SenderEmail), kw) {
				continue
			}
		}
		if invoiceRef != "" {
			found := false
			ref := strings.ToUpper(invoiceRef)
			for _, pi := range email.ParsedInvoices {
				if strings.ToUpper(pi.InvoiceRef) == ref {
					found = true
					break
				}
			}
			if !found && !strings.Contains(strings.ToUpper(email.BodyText), ref) {
				continue
			}
		}
		results = append(results, email)
	}
	return results
}

func (d *Data) LookupReference(reference string) *ReferenceMatch {
	upper := strings.ToUpper(strings.TrimSpace(reference))

	for _, inv := range d.Invoices {
		if strings.ToUpper(inv.InvoiceID) == upper {
			cust := d.GetCustomerByID(inv.CustomerID)
			name := ""
			if cust != nil {
				name = cust.Name
			}
			return &ReferenceMatch{
				Type:         "invoice",
				CustomerID:   inv.CustomerID,
				CustomerName: name,
				InvoiceID:    inv.InvoiceID,
				Amount:       inv.Amount,
			}
		}
	}

	return nil
}

type CustomerMatch struct {
	CustomerID string  `json:"customer_id"`
	Name       string  `json:"name"`
	Score      float64 `json:"similarity_score"`
}

type AmountMatch struct {
	CustomerID   string   `json:"customer_id"`
	CustomerName string   `json:"customer_name"`
	InvoiceIDs   []string `json:"invoice_ids"`
	InvoiceTotal float64  `json:"invoice_total"`
	Difference   float64  `json:"difference"`
	DiffPercent  float64  `json:"difference_percent"`
}

type ReferenceMatch struct {
	Type         string  `json:"type"`
	CustomerID   string  `json:"customer_id"`
	CustomerName string  `json:"customer_name"`
	InvoiceID    string  `json:"invoice_id,omitempty"`
	Amount       float64 `json:"amount,omitempty"`
}

func findCombinations(invoices []models.Invoice, target, tolerance float64) [][]models.Invoice {
	var results [][]models.Invoice
	maxDepth := 6
	if len(invoices) > 12 {
		invoices = invoices[:12]
	}

	var search func(idx int, current []models.Invoice, sum float64, depth int)
	search = func(idx int, current []models.Invoice, sum float64, depth int) {
		if depth > maxDepth {
			return
		}
		if len(current) > 0 && math.Abs(target-sum) <= tolerance {
			combo := make([]models.Invoice, len(current))
			copy(combo, current)
			results = append(results, combo)
			if len(results) >= 5 {
				return
			}
		}
		if sum > target+tolerance {
			return
		}
		for i := idx; i < len(invoices) && len(results) < 5; i++ {
			search(i+1, append(current, invoices[i]), sum+invoices[i].Amount, depth+1)
		}
	}
	search(0, nil, 0, 0)
	return results
}

func normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func trigrams(s string) map[string]bool {
	result := make(map[string]bool)
	if len(s) < 3 {
		if len(s) > 0 {
			result[s] = true
		}
		return result
	}
	for i := 0; i <= len(s)-3; i++ {
		result[s[i:i+3]] = true
	}
	return result
}

func trigramSimilarity(a, b string) float64 {
	tA := trigrams(a)
	tB := trigrams(b)
	if len(tA) == 0 || len(tB) == 0 {
		return 0
	}
	intersection := 0
	for k := range tA {
		if tB[k] {
			intersection++
		}
	}
	union := len(tA) + len(tB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func sortByScore(matches []CustomerMatch) {
	for i := 0; i < len(matches); i++ {
		for j := i + 1; j < len(matches); j++ {
			if matches[j].Score > matches[i].Score {
				matches[i], matches[j] = matches[j], matches[i]
			}
		}
	}
}

func loadJSON(path string, v interface{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}
