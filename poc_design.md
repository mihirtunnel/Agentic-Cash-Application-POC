# Cash Application AI Pipeline — POC Design

**Date:** April 2026
**Language:** Go (same language as production — no throwaway prototype code)
**AI Provider:** Claude API (Anthropic)
**Data:** Static dummy data in JSON files (no database, no ERP, no email server)

---

## 1. What This POC Proves

This POC validates the core matching logic end-to-end:

1. Can the pre-filtering (fuzzy match + amount search) narrow 20+ customers to 3-8 candidates?
2. Can Claude correctly identify the customer from candidates? (Call 1)
3. Can Claude correctly match invoices with deductions? (Call 2)
4. Do the short-circuit rules work and skip AI when appropriate?
5. Do the programmatic guardrails catch bad matches?
6. What does the confidence distribution look like across different transaction types?

**What this POC does NOT do:** Connect to a real bank, ERP, or email server. Those are Phase 2.

---

## 2. Project Structure

```
poc/
├── data/                          # All dummy data lives here (edit these files to test scenarios)
│   ├── customers.json             # Customer master list (20 customers)
│   ├── invoices.json              # Open invoices per customer
│   ├── remittance_emails.json     # Parsed remittance email data
│   ├── bank_transactions.json     # Bank statement entries to process
│   └── customer_aliases.json      # Known aliases and account numbers
│
├── internal/                      # Core pipeline packages
│   ├── models/
│   │   └── models.go              # Structs for all entities
│   ├── dataloader/
│   │   └── loader.go              # Loads dummy data from JSON files
│   ├── shortcircuit/
│   │   └── shortcircuit.go        # Step 1: exact match rules (no AI)
│   ├── prefilter/
│   │   └── prefilter.go           # Step 2: fuzzy match + amount search (no AI)
│   ├── aiclient/
│   │   └── client.go              # Claude API client with structured outputs
│   ├── customerid/
│   │   └── customerid.go          # Step 3: AI Call 1 — customer identification
│   ├── invoicematch/
│   │   └── invoicematch.go        # Step 4: AI Call 2 — invoice matching
│   ├── guardrails/
│   │   └── guardrails.go          # Step 5: programmatic validation
│   └── router/
│       └── router.go              # Step 6: confidence-based routing
│
├── cmd/
│   └── main.go                    # Entry point — processes all transactions
│
├── go.mod                         # Go module definition
├── go.sum                         # Dependency checksums
├── .env.example                   # Template for API key
└── results/                       # Output directory (created at runtime)
    └── (match results saved here as JSON)
```

---

## 3. Dummy Data Design

All data is in `data/` as JSON files. To test new scenarios, edit or add entries to these files. No code changes needed.

### 3.1 customers.json — 20 Customers

Contains customer profiles that cover different matching scenarios:

```json
[
  {
    "customer_id": "CUST-0091",
    "name": "Acme Corp Ltd",
    "payment_method": "wire",
    "typical_pattern": "pays 2-3 invoices together",
    "discount_policy": "2% if paid within 10 days",
    "account_number": "ACC-8821"
  },
  ...
]
```

The 20 customers are designed to test:
- **Similar names:** Acme Corp Ltd, Acme Corporation, Acme Corp International (tests fuzzy matching)
- **Unique names:** Baker Industries, Delta Fuels Inc (tests easy matches)
- **Multiple payment methods:** Wire, ACH, check
- **Varying activity levels:** Active (many invoices), dormant (no recent payments)

### 3.2 invoices.json — Open Invoices

Each customer has 1-8 open invoices:

```json
[
  {
    "invoice_id": "INV-312",
    "customer_id": "CUST-0091",
    "amount": 25000.00,
    "due_date": "2024-04-15",
    "description": "Fuel delivery March 28",
    "status": "open"
  },
  ...
]
```

Designed to test:
- **Exact single match:** One invoice exactly matches payment amount
- **Multi-invoice match:** 2-3 invoices sum to payment amount
- **Discount scenarios:** Invoice totals are 1-2% higher than payment (early-pay discount)
- **No match:** Payment amount doesn't match any combination
- **Ambiguous match:** Multiple possible combinations for the same amount

### 3.3 bank_transactions.json — 10 Test Transactions

Each transaction tests a different scenario:

```json
[
  {
    "txn_id": "TXN-001",
    "date": "2024-04-10",
    "amount": 47230.00,
    "direction": "credit",
    "counterparty_name": "ACME CORP INT WIRE",
    "counterparty_account": "",
    "bank_reference": "PAYMENT REF 88421",
    "remittance_info": "",
    "expected_result": {
      "customer_id": "CUST-0091",
      "invoices": ["INV-312", "INV-287"],
      "scenario": "multi-invoice with discount"
    }
  },
  ...
]
```

The 10 transactions cover:

| TXN | Scenario | Expected Path | Tests |
|-----|----------|---------------|-------|
| TXN-001 | Multi-invoice with early-pay discount | Full chain (Call 1 + Call 2) | Discount handling |
| TXN-002 | Exact account number + exact amount | Short-circuit → auto-post | No AI needed |
| TXN-003 | Invoice reference in bank memo | Short-circuit → auto-post | No AI needed |
| TXN-004 | Single customer, single invoice match | Skip Call 1, direct to Call 2 | Partial short-circuit |
| TXN-005 | Similar customer names, ambiguous | Full chain, lower confidence | Fuzzy matching stress test |
| TXN-006 | Payment with no matching customer | Call 1 returns low confidence | Correct rejection |
| TXN-007 | Payment covers all open invoices | Full chain, high confidence | Full balance payoff |
| TXN-008 | Short-payment (disputed deduction) | Full chain, deduction flagged | Deduction detection |
| TXN-009 | Remittance email available | Full chain with email context | Email context improves match |
| TXN-010 | Overpayment (more than invoice total) | Full chain, unmatched amount | Edge case handling |

### 3.4 remittance_emails.json — Parsed Email Data

Pre-parsed (no actual email parsing in POC):

```json
[
  {
    "email_id": "EMAIL-001",
    "customer_id": "CUST-0091",
    "sender_name": "AP Department - Acme Corp",
    "sender_email": "ap@acmecorp.com",
    "received_date": "2024-04-09",
    "subject": "Payment notification - April batch",
    "body_text": "Please find attached remittance for wire transfer April 10.",
    "parsed_invoices": [
      {"invoice_ref": "INV-312", "amount": 25000.00},
      {"invoice_ref": "INV-287", "amount": 22230.00, "note": "2% early pay discount"}
    ],
    "total_amount": 47230.00
  },
  ...
]
```

### 3.5 customer_aliases.json — Account Numbers & Known Names

Maps bank counterparty names and account numbers to customers:

```json
[
  {
    "customer_id": "CUST-0091",
    "aliases": ["Acme Corp", "ACME CORP LTD", "Acme Corp Ltd"],
    "bank_accounts": ["ACC-8821", "ACC-8822"]
  },
  ...
]
```

---

## 4. Pipeline Steps in Code

### Step 1: Short-Circuit Check (`short_circuit.py`)

```
Input:  bank transaction + customer_aliases + invoices
Output: match result (if short-circuit hits) OR None (proceed to pre-filter)

Logic:
  1. Check if counterparty_account matches any known bank account
     → If yes AND invoice amounts match → return auto-post result
  2. Check if bank_reference matches any invoice ID
     → If yes AND amount matches → return auto-post result  
  3. Check if exactly one customer has exactly one invoice matching the amount
     → If yes → return that customer (skip Call 1, go to Call 2)
```

### Step 2: Pre-Filter (`pre_filter.py`)

```
Input:  bank transaction + all customers + all invoices
Output: list of 3-8 candidate customers with scores

Logic:
  1. Fuzzy name match: trigram similarity of counterparty vs all customer names
  2. Amount combination: find customers whose invoices sum to payment amount (±2%)
  3. Reference lookup: check aliases for counterparty name fragments
  4. Combine scores, sort, return top candidates
```

### Step 3: AI Call 1 — Customer ID (`customer_id.py`)

```
Input:  bank transaction + candidate customers (from pre-filter)
Output: customer_id + confidence + reasoning

Makes one Claude API call with:
  - System prompt (treasury specialist role + rules)
  - Structured output schema (forced JSON via tool_use)
  - Transaction data + candidates
```

### Step 4: AI Call 2 — Invoice Match (`invoice_match.py`)

```
Input:  confirmed customer + their invoices + remittance email (if found)
Output: invoice match proposal with applied amounts, deductions, confidence

Makes one Claude API call with:
  - System prompt (same role, invoice matching rules)
  - Structured output schema (invoice-level match JSON)
  - Customer invoices + remittance data
```

### Step 5: Guardrails (`guardrails.py`)

```
Input:  AI match result + original transaction + invoice data
Output: validated result (pass/fail + reasons)

Checks:
  1. Applied amounts sum to payment amount
  2. All invoice IDs exist in our data
  3. All invoices belong to the matched customer
  4. Deductions are within policy limits
  5. No invoice is matched to more than one payment
```

### Step 6: Router (`router.py`)

```
Input:  validated match result with confidence
Output: routing decision (auto-post / human-review / manual-investigation)

Rules:
  confidence ≥ 0.95 AND guardrails pass → auto-post
  confidence 0.70-0.94                  → human review (suggestion pre-filled)
  confidence < 0.70                     → manual investigation
```

---

## 5. Output Format

For each transaction, the pipeline produces a result JSON:

```json
{
  "txn_id": "TXN-001",
  "pipeline_path": "full_chain",
  "short_circuit": null,
  "pre_filter": {
    "candidates_found": 3,
    "top_candidate": "CUST-0091",
    "top_score": 0.78
  },
  "call_1": {
    "customer_id": "CUST-0091",
    "confidence": 0.93,
    "reasoning": "...",
    "tokens_used": {"input": 1300, "output": 80},
    "cost_usd": 0.005
  },
  "call_2": {
    "customer_id": "CUST-0091",
    "confidence": 0.97,
    "invoices": [...],
    "reasoning": "...",
    "tokens_used": {"input": 1600, "output": 200},
    "cost_usd": 0.007
  },
  "guardrails": {
    "passed": true,
    "checks": {
      "amounts_balance": true,
      "ids_exist": true,
      "deductions_within_policy": true,
      "no_double_match": true
    }
  },
  "routing_decision": "auto_post",
  "total_cost_usd": 0.012,
  "matches_expected": true
}
```

---

## 6. How to Run

```bash
# 1. Navigate to poc directory
cd poc

# 2. Set your Claude API key
cp .env.example .env
# Edit .env and add your ANTHROPIC_API_KEY

# 3. Run the pipeline on all 10 test transactions
go run ./cmd/main.go

# 4. Run on a single transaction
go run ./cmd/main.go -txn TXN-001

# 5. Run without AI calls (test short-circuit and pre-filter only)
go run ./cmd/main.go -dry-run
```

---

## 7. How to Add/Modify Test Data

To test a new scenario:

1. Add a customer to `data/customers.json`
2. Add their invoices to `data/invoices.json`
3. Add a bank transaction to `data/bank_transactions.json`
4. Optionally add a remittance email to `data/remittance_emails.json`
5. Optionally add aliases to `data/customer_aliases.json`
6. Run `go run ./cmd/main.go`

No code changes needed. The pipeline reads all data from these files at startup.

---

## 8. What We Measure

After running all 10 transactions, the pipeline prints a summary:

```
=== Pipeline Results Summary ===

Transactions processed:    10
Short-circuited (no AI):   3  (30%)
Call 1 only (low conf):    1  (10%)
Full chain (Call 1 + 2):   6  (60%)

Routing decisions:
  Auto-post:               4  (40%)
  Human review:            4  (40%)
  Manual investigation:    2  (20%)

Accuracy (vs expected):
  Correct customer ID:     9/10  (90%)
  Correct invoice match:   8/10  (80%)

AI costs:
  Total Claude API cost:   $0.052
  Avg cost per transaction: $0.0052

Guardrail catches:
  Amounts didn't balance:  1
  Deduction out of policy: 0
  Double-match prevented:  0
```

This tells us if the pipeline works before we connect it to real systems.
