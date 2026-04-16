package prefilter

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"cashapp-poc/internal/dataloader"
	"cashapp-poc/internal/models"
)

const (
	nameThreshold    = 0.25
	amountTolerance  = 0.02
	maxCandidates    = 8
	maxCombinationLen = 6
)

func FindCandidates(txn models.BankTransaction, data *dataloader.Data) []models.Candidate {
	scores := make(map[string]*models.Candidate)

	// Filter 1: Fuzzy name match
	fuzzyNameMatch(txn.CounterpartyName, data, scores)

	// Filter 2: Amount combination search
	amountMatch(txn.Amount, data, scores)

	// Filter 3: Alias / reference lookup
	aliasMatch(txn.CounterpartyName, data, scores)

	var candidates []models.Candidate
	for _, c := range scores {
		candidates = append(candidates, *c)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	fmt.Printf("  [PRE-FILTER] Found %d candidates\n", len(candidates))
	for i, c := range candidates {
		fmt.Printf("    %d. %s (%s) — score: %.2f — %s\n",
			i+1, c.Customer.Name, c.Customer.CustomerID, c.Score, c.Reason)
	}

	return candidates
}

func fuzzyNameMatch(bankText string, data *dataloader.Data, scores map[string]*models.Candidate) {
	normalized := normalize(bankText)
	for _, cust := range data.Customers {
		sim := trigramSimilarity(normalized, normalize(cust.Name))
		if sim >= nameThreshold {
			addScore(scores, cust, sim*0.5, fmt.Sprintf("name_similarity=%.2f", sim))
		}
	}
}

func amountMatch(targetAmount float64, data *dataloader.Data, scores map[string]*models.Candidate) {
	tolerance := targetAmount * amountTolerance

	customerInvoices := make(map[string][]models.Invoice)
	for _, inv := range data.Invoices {
		if inv.Status == "open" {
			customerInvoices[inv.CustomerID] = append(customerInvoices[inv.CustomerID], inv)
		}
	}

	for custID, invoices := range customerInvoices {
		if findCombination(invoices, targetAmount, tolerance) {
			cust := findCustomer(custID, data)
			if cust != nil {
				addScore(scores, *cust, 0.4, "amount_combination_match")
			}
		}
	}
}

func aliasMatch(bankText string, data *dataloader.Data, scores map[string]*models.Candidate) {
	upper := strings.ToUpper(bankText)
	for _, alias := range data.Aliases {
		for _, a := range alias.Aliases {
			if strings.Contains(upper, strings.ToUpper(a)) || strings.Contains(strings.ToUpper(a), upper) {
				cust := findCustomer(alias.CustomerID, data)
				if cust != nil {
					addScore(scores, *cust, 0.3, fmt.Sprintf("alias_match=%s", a))
				}
			}
		}
	}
}

func addScore(scores map[string]*models.Candidate, cust models.Customer, score float64, reason string) {
	if existing, ok := scores[cust.CustomerID]; ok {
		existing.Score += score
		existing.Reason += "; " + reason
	} else {
		scores[cust.CustomerID] = &models.Candidate{
			Customer: cust,
			Score:    score,
			Reason:   reason,
		}
	}
}

func findCustomer(id string, data *dataloader.Data) *models.Customer {
	return data.GetCustomerByID(id)
}

func findCombination(invoices []models.Invoice, target, tolerance float64) bool {
	if len(invoices) > maxCombinationLen*2 {
		invoices = invoices[:maxCombinationLen*2]
	}
	return subsetSum(invoices, 0, 0, target, tolerance, 0)
}

func subsetSum(invoices []models.Invoice, idx int, currentSum, target, tolerance float64, depth int) bool {
	if depth > maxCombinationLen {
		return false
	}
	diff := math.Abs(target - currentSum)
	if diff <= tolerance && currentSum > 0 {
		return true
	}
	if currentSum > target+tolerance {
		return false
	}
	if idx >= len(invoices) {
		return false
	}
	for i := idx; i < len(invoices); i++ {
		if subsetSum(invoices, i+1, currentSum+invoices[i].Amount, target, tolerance, depth+1) {
			return true
		}
	}
	return false
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
