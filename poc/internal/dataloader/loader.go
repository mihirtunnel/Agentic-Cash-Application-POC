package dataloader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"cashapp-poc/internal/models"
)

type Data struct {
	Customers        []models.Customer
	Invoices         []models.Invoice
	Aliases          []models.CustomerAlias
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
	if err := loadJSON(filepath.Join(dataDir, "customer_aliases.json"), &d.Aliases); err != nil {
		return nil, fmt.Errorf("loading aliases: %w", err)
	}
	if err := loadJSON(filepath.Join(dataDir, "remittance_emails.json"), &d.RemittanceEmails); err != nil {
		return nil, fmt.Errorf("loading remittance emails: %w", err)
	}
	if err := loadJSON(filepath.Join(dataDir, "bank_transactions.json"), &d.BankTransactions); err != nil {
		return nil, fmt.Errorf("loading bank transactions: %w", err)
	}

	fmt.Printf("Loaded: %d customers, %d invoices, %d aliases, %d emails, %d transactions\n",
		len(d.Customers), len(d.Invoices), len(d.Aliases),
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

func (d *Data) FindRemittanceEmail(customerID string, amount float64) *models.RemittanceEmail {
	tolerance := amount * 0.03
	for i := range d.RemittanceEmails {
		e := &d.RemittanceEmails[i]
		if e.CustomerID == customerID {
			diff := e.TotalAmount - amount
			if diff < 0 {
				diff = -diff
			}
			if diff <= tolerance {
				return e
			}
		}
	}
	return nil
}

func (d *Data) GetInvoiceByID(id string) *models.Invoice {
	for i := range d.Invoices {
		if d.Invoices[i].InvoiceID == id {
			return &d.Invoices[i]
		}
	}
	return nil
}

func loadJSON(path string, v interface{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}
