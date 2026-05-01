# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Quick start

The project is a Go POC that extracts remittance payment data from `.eml` emails with PDF attachments by parsing the content and sending it to Claude or OpenAI.

### Running the main entry point
```bash
# Set up API keys
cp .env.example .env
# Edit .env and add ANTHROPIC_API_KEY or OPENAI_API_KEY

# Generate sample email + PDF
go run cmd/generate_sample/main.go

# Run against sample (Claude by default)
go run main.go

# Use OpenAI instead
go run main.go -provider openai

# Point to a specific .eml file
go run main.go -email path/to/email.eml

# Dry-run (parse and build context, skip AI call)
go run main.go -dry-run
```

### Running the Gmail sync command
```bash
# First-time setup: downloads a credentials JSON from Google Cloud Console
go run ./cmd/gmail_sync -credentials ./gmial-client_secret_....json

# Fetch remittance emails from Gmail (uses Gmail search syntax)
go run ./cmd/gmail_sync -query "subject:remittance has:attachment" -max 20

# Dry-run
go run ./cmd/gmail_sync -dry-run

# Use OpenAI instead
go run ./cmd/gmail_sync -provider openai
```

### Running the Outlook sync command
```bash
# First-time setup: opens browser for Microsoft OAuth2 consent, saves token
go run ./cmd/outlook_sync -client-id <your-azure-app-client-id>

# Subsequent runs — token auto-reloaded from outlook_token.json, no browser
go run ./cmd/outlook_sync

# Custom OData filter
go run ./cmd/outlook_sync -query "hasAttachments eq true" -max 20

# Dry-run
go run ./cmd/outlook_sync -dry-run

# Use OpenAI instead
go run ./cmd/outlook_sync -provider openai
```

Requirements: set `OUTLOOK_CLIENT_ID` in `.env` (see `plan.md` Part 1 for Azure App Registration steps).

### Building binaries
```bash
# Main binary
go build -o remittance-poc main.go

# Gmail sync binary
go build -o gmail-sync ./cmd/gmail_sync

# Outlook sync binary
go build -o outlook-sync ./cmd/outlook_sync
```

## Architecture

### Entry points
- **main.go** — CLI tool that processes a single `.eml` file from disk. Reads flags for email path, provider, and dry-run mode.
- **cmd/gmail_sync/main.go** — CLI tool that fetches emails directly from Gmail using OAuth2, then processes them through the same pipeline.
- **cmd/outlook_sync/main.go** — CLI tool that fetches emails from a personal Outlook account via Microsoft Graph API (OAuth2), then processes them through the same pipeline.
- **cmd/generate_sample/main.go** — Generates sample `.eml` and `.pdf` test files for development.

### Processing pipeline
The main flow is: **Parse email → Extract PDF text → Build compact context → Call AI → Format & save results**

1. **Email parsing** (`internal/emailparser/parser.go`)
   - Reads `.eml` file, extracts MIME structure
   - Returns: metadata (From, To, Subject, Date), plain-text body (HTML tags stripped), and PDF attachments as raw bytes

2. **PDF text extraction** (`internal/pdfextractor/extractor.go`)
   - Uses `ledongthuc/pdf` to read text from PDF content streams
   - Works only on machine-generated PDFs; scanned/image-only PDFs produce warnings and are skipped
   - Returns: extracted text or empty string

3. **Context assembly** (`internal/textbuilder/builder.go`)
   - Combines email metadata, body text, and all PDF text into a single string
   - Caps output at `textbuilder.MaxContextChars` (12,000 characters) to minimize token usage
   - Truncates with a notice if longer

4. **AI extraction** (`internal/aiclient/client.go`)
   - Abstracts Claude and OpenAI REST APIs behind a `Client` interface
   - Sends context + structured extraction prompt to the LLM
   - Parses JSON response into `RemittanceData` struct
   - Supports dry-run mode (skips API call)

5. **Result output** (`main.go` and `cmd/gmail_sync/main.go`)
   - Prints extraction results to stdout
   - Saves a timestamped JSON to `results/` directory
   - Includes token usage and cost estimate

### Data structures
All types in `internal/models/models.go`:

- **RemittanceData** — structured output: payer, payment date, currency, total amount, bank reference, invoices list, notes
- **InvoiceLine** — invoice number, amount, optional description
- **EmailMetadata** — From, To, Subject, Date
- **ParsedEmail** — parsed email result: metadata, body text, PDF attachments
- **TokenUsage** — input/output token counts
- **ExtractionResult** — full result including data, tokens, cost, provider, model, timestamp

### AI providers
- **Claude** (default) — `ProviderClaude`, model: `claude-opus-4-5`, endpoint: `https://api.anthropic.com/v1/messages`
- **OpenAI** — `ProviderOpenAI`, model: `gpt-5.4-nano`, endpoint: `https://api.openai.com/v1/chat/completions`

Both providers use environment variables for API keys:
- Claude: `ANTHROPIC_API_KEY`
- OpenAI: `OPENAI_API_KEY`

Cost estimation is approximate and used for reporting only.

## Key design decisions

- **No binary PDF parsing** — PDFs are always extracted to text first to minimize token usage. Binary is never sent to the LLM.
- **Context cap at 12KB** — keeps token counts low and consistent across large emails with many PDFs.
- **Provider abstraction** — same pipeline works with Claude or OpenAI; only API call and token parsing differ.
- **Dry-run mode** — useful for testing parsing logic without incurring API costs.
- **OAuth2 for Gmail** — gmail_sync uses OAuth2 consent flow; token is auto-saved and refreshed.

## Environment setup

`.env.example` shows what to configure:

```
ANTHROPIC_API_KEY=sk-ant-...          # For Claude
OPENAI_API_KEY=sk-...                 # For OpenAI
GMAIL_CREDENTIALS_FILE=...            # For Gmail sync (Google Cloud Console JSON)
GMAIL_TOKEN_FILE=gmail_token.json     # Auto-created after first Gmail auth
```

The `.env` file is read at startup by `loadEnvFile()` and is never committed (listed in `.gitignore`).

## Testing & debugging

- No unit tests currently; the POC uses live API calls for validation.
- To debug parsing without API costs, use `-dry-run` flag.
- Results are saved to `results/` directory with timestamps; check these files to inspect AI responses.
- Console output includes context size, token usage, elapsed time, and cost.

## Limitations (documented scope)

- Scanned PDFs (image-only pages) produce no text; OCR is out of scope.
- Batch processing is not implemented (single email per run).
- No IMAP/live polling; only `.eml` files or Gmail API fetching.
