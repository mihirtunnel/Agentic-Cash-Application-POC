# Remittance Email AI Extraction — POC

A Go POC that parses remittance emails (`.eml` files with PDF attachments), extracts plain text from both the email body and the PDFs, and sends the combined context to an LLM (Claude or OpenAI) to return structured payment data.

## What it extracts

| Field | Example |
|---|---|
| Payer | Global Supplies Inc. |
| Payment date | 2026-04-15 |
| Currency | USD |
| Total amount | 15000.00 |
| Bank reference # | WIRE-20260415-8842 |
| Invoice list | INV-2026-001 $5,250 / INV-2026-002 $3,750 / INV-2026-003 $6,000 |
| Invoice count | 3 |

## Quick start

```bash
# 1. Copy and fill in your API key(s)
cp .env.example .env
# edit .env — add ANTHROPIC_API_KEY or OPENAI_API_KEY

# 2. Generate the sample email + PDF test files
go run cmd/generate_sample/main.go

# 3. Run against the sample (uses Claude by default)
go run main.go

# 4. Use OpenAI instead
go run main.go -provider openai

# 5. Dry-run — parse and build context but skip the AI call
go run main.go -dry-run

# 6. Point to your own .eml file
go run main.go -email path/to/your/email.eml

go run main.go -email "./data/Payment Initiated.eml" -provider openai

go run main.go -email "./data/Payment Initiated 71 invoices.eml" -provider openai


```

Results are written as JSON to `results/`.

## Flags

| Flag | Default | Description |
|---|---|---|
| `-email` | `data/sample.eml` | Path to the `.eml` file to process |
| `-provider` | `claude` | `claude` or `openai` |
| `-dry-run` | false | Parse + build context but skip API call |

## How token usage is minimised

Raw PDF binary is **never sent** to the LLM. Instead:

1. **Email body** — the plain-text body is extracted from the MIME structure; HTML bodies are stripped of tags first.
2. **PDF attachments** — `ledongthuc/pdf` reads text directly from the PDF content streams (works for machine-generated PDFs; scanned/image-only PDFs produce no text and are skipped with a warning).
3. **Context cap** — the combined string is capped at `12 000` characters (`textbuilder.MaxContextChars`) and truncated with a notice if longer.

## Project layout

```
.
├── main.go                      # CLI entry point
├── cmd/generate_sample/main.go  # generates data/sample.pdf + sample.eml
├── internal/
│   ├── models/      # RemittanceData, InvoiceLine, EmailMetadata structs
│   ├── emailparser/ # .eml → metadata, plain-text body, PDF attachment bytes
│   ├── pdfextractor/# PDF bytes → plain text (ledongthuc/pdf)
│   ├── textbuilder/ # assemble compact context string for the LLM
│   └── aiclient/    # Claude + OpenAI REST calls, JSON response parsing
├── data/            # sample.eml and sample.pdf (generated)
└── results/         # JSON output files (created at runtime)
```

## Limitations (POC scope)

- **Scanned PDFs** — image-only pages produce no text. OCR is out of scope.
- **Input format** — only `.eml` files on disk (no IMAP/live inbox polling).
- **Single email per run** — batch processing is a trivial extension.
