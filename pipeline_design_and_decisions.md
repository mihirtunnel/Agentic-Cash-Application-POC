# Cash Application AI Pipeline — Design Decisions & Technical Specification

**Prepared for:** Tauber Oil Internal Engineering & Treasury Teams
**Date:** April 2026
**Status:** Pre-POC Design Document

---

## Table of Contents

1. [Problem Statement (Quick Recap)](#1-problem-statement)
2. [What We Evaluated and Rejected](#2-what-we-evaluated-and-rejected)
3. [What We Are Using and Why](#3-what-we-are-using-and-why)
4. [Claude API Parameters Explained](#4-claude-api-parameters-explained)
5. [Prompt Caching vs Shared Context](#5-prompt-caching-vs-shared-context)
6. [The Final Pipeline (Step-by-Step Walkthrough)](#6-the-final-pipeline)
7. [Prompt Design for Each AI Call](#7-prompt-design-for-each-ai-call)
8. [Token Usage & Cost Estimation](#8-token-usage-and-cost-estimation)
9. [Extended Thinking — When and Why](#9-extended-thinking)
10. [Architecture Diagram](#10-architecture-diagram)
11. [OpenAI API — Parameters, Features & Comparison](#11-openai-api)

---

## 1. Problem Statement

A bank statement arrives with a list of credit entries (incoming payments). For each entry, we need to answer:

> **"Who sent this payment and which invoices does it pay off?"**

This is hard because:
- Bank entries say things like "ACME CORP INT WIRE" — not the exact customer name in our ERP
- Remittance emails come in different formats from different customers
- A single payment may cover multiple invoices
- Customers take early-pay discounts or make short-payments
- The treasury team currently does this manually by cross-referencing bank statements, ERP invoices, and email inboxes

**Goal:** Automate 70–90% of this matching. Route the rest to human review with an AI-generated suggestion pre-filled.

---

## 2. What We Evaluated and Rejected

We evaluated several modern AI approaches and rejected each for specific reasons.

### 2.1 RAG (Retrieval-Augmented Generation) — REJECTED

**What RAG is:** A search technique. You convert documents into mathematical vectors (embeddings), store them in a vector database, and when you need information, you search by vector similarity instead of keywords. The search results are then injected into the AI prompt as plain text.

**Why we considered it:** To find the right remittance email for a bank transaction.

**Why we rejected it:**

The core misunderstanding about RAG is that many people think embedding data and sending it to an LLM is somehow different from sending plain text. It is not. The LLM always receives plain text — it never sees embeddings. RAG is only a search method.

For our use case, remittance emails in B2B oil & gas payments almost always contain structured information — invoice numbers, amounts, dates. These are perfectly searchable with SQL:

```sql
SELECT * FROM remittance_emails
WHERE payment_amount BETWEEN 46000 AND 48500
  AND received_at >= '2024-04-01'
  AND sender_name ILIKE '%acme%'
```

RAG would only help if our emails contained vague, unstructured text with no invoice numbers or amounts — which is rare in B2B payments.

**Bottom line:** SQL search finds the right email. RAG adds embedding costs and complexity for no accuracy or cost benefit. The LLM sees the same context either way.

### 2.2 MCP (Model Context Protocol) — REJECTED

**What MCP is:** Anthropic's protocol for giving an AI model live access to external tools and data sources during a conversation.

**Why we considered it:** To let the AI pull data from our ERP or email system during the human review workflow.

**Why we rejected it:** Our pipeline is batch processing, not interactive. The Go/Java orchestrator already knows exactly what data to fetch and when. MCP adds latency and complexity without solving a problem we actually have. If we build an interactive review UI in the future (Phase 4+), we can reconsider.

### 2.3 Tool Use for Data Fetching — REJECTED

**What it is:** You define "tools" (functions) that Claude can call. Claude responds with "I want to call get_invoices(customer_id='CUST-0091')" and your code executes that function, sends the result back, and Claude continues reasoning.

**Why we considered it:** To let Claude decide what data it needs.

**Why we rejected it:**

In our pipeline, the orchestrator already knows what data Claude needs at each step:
- Call 1 needs: bank transaction + pre-filtered customer candidates (we already have this)
- Call 2 needs: confirmed customer + their invoices + remittance data (we already fetch this)

There is no decision point where Claude needs to say "I should go look up X." Tool use would add an extra API round-trip per call (doubling latency and cost) for zero benefit.

```
WITHOUT tool use:
  Go fetches data → builds prompt → 1 API call → result
  Cost: 1 API call

WITH tool use:
  Go sends prompt → API call 1 (Claude says "call get_invoices") →
  Go fetches data → API call 2 (Claude reasons over results) → result
  Cost: 2 API calls for the same result
```

### 2.4 Agentic Pipeline (Multi-Step AI Loops) — REJECTED

**What it is:** The AI drives the workflow — it calls tools, reasons over results, decides what to do next, calls more tools, loops until it solves the problem. The AI is "in charge."

**Why we considered it:** To let Claude autonomously investigate ambiguous payments.

**Why we rejected it:**

Our matching workflow is linear and predictable. Every step is predetermined:

| Step | Does AI need to decide what to do? | Answer |
|------|-------------------------------------|--------|
| Fetch bank transaction | No | Triggered by schedule |
| Pre-filter customers | No | Always run fuzzy match + amount search |
| Identify customer | No | Always uses same evidence | 
| Fetch invoices | No | Always fetch after customer ID |
| Match invoices | No | Always uses invoices + remittance |
| Post to ERP | No | Always post if confidence is high |

An agent adds 3–5x API calls, 3–5x latency, and 3–5x cost to handle a workflow that never varies. Our Go/Java orchestrator handles the routing far cheaper and faster.

**Future consideration:** For Phase 4+, agents could help with the exception handling queue — payments that fail automated matching and require creative investigation.

### 2.5 Shared Context Between Transactions — REJECTED

**What it is:** Sending the results of previous transaction matches as context when processing the next transaction, so the AI "remembers" what it already processed.

**Why we rejected it:**

The Claude API is stateless. There is no server-side memory. If you want Claude to "remember" previous calls, YOU must re-send all previous conversation data every time. This means:

```
Transaction 1:   900 input tokens
Transaction 2:   900 + previous ~1,000 = 1,900 input tokens
Transaction 3:   900 + previous ~2,500 = 3,400 input tokens
...
Transaction 50:  900 + previous ~50,000 = 50,900 input tokens
```

Costs explode. And the cross-transaction "knowledge" (like "this customer usually takes 2% discount") is better stored as structured data in your database and included directly in the prompt when needed.

Each bank transaction should be judged independently on its own evidence.

---

## 3. What We Are Using and Why

### 3.1 Structured Outputs (Tool Use for Output Schema) — YES

**Important distinction:** We rejected tool use for *data fetching* (Section 2.3), but we ARE using the tool use mechanism for a different purpose — forcing Claude to return valid, structured JSON.

**How it works:** You define a JSON schema as a "tool." You tell Claude it MUST use this tool. Claude is forced to return JSON that matches your schema exactly. Your code gets guaranteed-valid JSON every time.

**Why this matters for a financial system:**

Without structured outputs, Claude might return:
```
Based on my analysis, I believe this payment matches INV-312...

Or sometimes:
{"customer": "Acme", "invoices": [{"id": "INV-312"}]}

Or sometimes:
```json
{"customer": "Acme", ...}
``` 
```

Your code has to handle all these formats. Fragile. Dangerous for financial data.

With structured outputs, Claude ALWAYS returns:
```json
{
  "customer_id": "CUST-0091",
  "confidence": 0.97,
  "invoices": [
    {"id": "INV-312", "applied": 25000.00, "deduction": 0, "reason": null}
  ],
  "reasoning": "Amount matches exactly."
}
```

**Cost impact:** Zero extra tokens. The tool schema is sent as part of the input, but it's small (~300 tokens) and gets cached (see Section 5).

### 3.2 Prompt Caching — YES

**What it is:** A billing optimization from Anthropic. You mark parts of your prompt as cacheable. When the next API call sends the same cached content, Anthropic processes it faster and charges 90% less for those tokens.

**What gets cached in our pipeline:**
- System prompt (~200 tokens) — same for every transaction
- Tool schema definitions (~300 tokens) — same for every transaction

**What doesn't get cached (changes every transaction):**
- Bank transaction details
- Customer candidates
- Invoice lists
- Remittance email data

**Cost impact:** ~50% savings on input token costs. See Section 8 for exact numbers.

### 3.3 Temperature = 0 — YES

Financial matching decisions must be deterministic and reproducible. Same input should always produce the same output. Temperature 0 ensures this.

### 3.4 Two-Stage AI Call Chain — YES

Split the AI work into two small, focused calls rather than one large call:
- Call 1: Customer Identification (~300 input tokens)
- Call 2: Invoice Matching (~600 input tokens)

With a confidence gate between them so we don't waste money on Call 2 when Call 1 isn't confident.

### 3.5 Short-Circuit Rules (No AI) — YES

30–40% of transactions can be matched with zero AI calls:
- Exact account number + exact amount match → auto-post
- Invoice reference in bank memo matching one open invoice → auto-post
- Single customer with single invoice exact amount match → skip Call 1, go to Call 2

---

## 4. Claude API Parameters Explained

When your Go/Java code calls the Claude API, you send a JSON request with several parameters. Here's what each one does and what we set it to:

### `model`

Which Claude model to use.

```json
"model": "claude-sonnet-4-20250514"
```

We use Claude Sonnet — best balance of quality and cost. Claude Opus is smarter but 5x more expensive and unnecessary for our structured matching task.

### `max_tokens`

The maximum number of tokens Claude will generate in its response. This is a hard ceiling — Claude stops when it hits this limit, even mid-sentence.

**You only pay for tokens actually generated, not the max_tokens value.** If you set max_tokens to 1024 but Claude only generates 150 tokens, you pay for 150.

```
Call 1 response (customer ID): ~50-100 tokens → set max_tokens: 300
Call 2 response (invoice match): ~100-300 tokens → set max_tokens: 1024
```

If set too low, Claude's response gets cut off. With structured outputs, Claude plans its response to fit, but always leave buffer.

### `temperature`

Controls randomness in Claude's responses.

```
temperature: 0   → deterministic. Same input = same output every time.
temperature: 1   → creative/varied. Same input may give different outputs.
```

**We use 0.** Financial decisions must be reproducible. There's no creativity needed in invoice matching.

### `system`

Instructions that define Claude's role, rules, and constraints. Sent with every request.

```json
"system": "You are a treasury cash application specialist. 
Your job is to match incoming bank payments to customer invoices.
Rules: Always account for early-pay discounts (1-2%)..."
```

The system prompt is ~200 tokens. It's the same for every call, so we cache it (see Section 5).

### `messages`

The actual conversation. For our pipeline, this is always a single user message containing the transaction data and context.

```json
"messages": [
  {"role": "user", "content": "Payment: $47,230 from ACME CORP..."}
]
```

We never send multi-turn conversations. Each call is one user message → one assistant response.

### `tools` and `tool_choice`

Defines the output schema (structured outputs). `tool_choice` forces Claude to use the specified tool, guaranteeing the output format.

```json
"tools": [{"name": "submit_match", "input_schema": {...}}],
"tool_choice": {"type": "tool", "name": "submit_match"}
```

### `thinking` (Extended Thinking)

Optional. Gives Claude a private scratchpad to reason before answering. See Section 9 for details.

### Parameters We Do NOT Use

| Parameter | Why Not |
|-----------|---------|
| `stop_sequences` | Not needed with structured outputs (tool use handles stopping) |
| `top_p` / `top_k` | Not needed when temperature is 0 |
| `stream` | Not needed for batch processing (we wait for full response) |

---

## 5. Prompt Caching vs Shared Context

These two concepts are frequently confused. They are completely different.

### How the Claude API Actually Works

The Claude API is **stateless**. Every API call starts from zero. Claude has no memory of any previous call. There is no session, no server-side memory, no "context Claude keeps."

When you chat with Claude on claude.ai or in Cursor, it feels like Claude "remembers" your conversation. But what's actually happening is the application (claude.ai / Cursor) is **re-sending your entire conversation history** with every message:

```
Message 1: You send "look at this PDF"
  App sends to API: [system prompt + your message + PDF]

Message 2: You send "what about RAG?"
  App sends to API: [system prompt + message 1 + response 1 + your new message]
  (The app re-sends EVERYTHING. Claude reads it all fresh.)

Message 3: You send "what about caching?"
  App sends to API: [system prompt + msg1 + resp1 + msg2 + resp2 + your new message]
  (Even bigger input. Claude still has zero memory — it reads everything fresh.)
```

The "memory" is an illusion created by the application re-sending everything.

### Shared Context = Re-sending Previous Data (EXPENSIVE)

If we wanted Claude to "remember" previous transactions, we would have to re-send them:

```
Transaction 1: 900 input tokens → $0.0027
Transaction 2: 900 + prev 1,000 = 1,900 tokens → $0.0057
Transaction 10: 900 + prev 10,000 = 10,900 tokens → $0.033
Transaction 50: 900 + prev 50,000 = 50,900 tokens → $0.153
```

Cost grows linearly with every transaction. **We don't do this.**

### Prompt Caching = Billing Discount on Repeated Input (CHEAP)

Prompt caching does NOT mean Claude remembers anything. It means Anthropic recognizes "you sent me these exact bytes before" and charges less to process them.

```
Call 1:
  [System prompt (200 tokens) + Tools (300 tokens) + Txn data (400 tokens)]
   ├──── identical every call ────┤  ├── different ──┤
   
  First call: Anthropic processes all 900 tokens. 
  Saves system+tools in temporary cache. 
  Cost: 500 × $3.75/M (cache write surcharge) + 400 × $3/M = $0.003

Call 2 (within 5 minutes):
  [System prompt (200 tokens) + Tools (300 tokens) + Txn data (400 tokens)]
   ├──── CACHE HIT (90% off) ────┤  ├── full price ──┤
   
  Anthropic says: "I've seen these first 500 tokens before."
  Cost: 500 × $0.30/M (90% discount!) + 400 × $3/M = $0.0014

Claude still has ZERO memory of Call 1. It processes Call 2 completely fresh.
The only difference: Anthropic charges you less for the repeated bytes.
```

### Visual Comparison

```
SHARED CONTEXT (we don't do this):
  Call 1: [System + Tools + Txn A]                        → 900 tokens
  Call 2: [System + Tools + Txn A + Result A + Txn B]     → 1,900 tokens
  Call 3: [System + Tools + TxnA + ResA + TxnB + ResB + TxnC] → 3,400 tokens
  ❌ Growing input, exploding cost, context window limit

PROMPT CACHING (we do this):
  Call 1: [System + Tools + Txn A]  → 900 tokens (500 cached + 400 new)
  Call 2: [System + Tools + Txn B]  → 900 tokens (500 cached + 400 new)
  Call 3: [System + Tools + Txn C]  → 900 tokens (500 cached + 400 new)
  ✅ Constant size, discounted cost, no memory between calls
```

### Cache Mechanics

| Detail | Value |
|--------|-------|
| Cache TTL (time-to-live) | 5 minutes after last use |
| Minimum cacheable prefix | 1,024 tokens (for Claude Sonnet) |
| Cache write cost | 25% surcharge on first call |
| Cache read cost | 90% discount on subsequent calls |
| What can be cached | Any prefix of the input (system prompt, tools, static content) |
| Does Claude "remember" | No. Caching is purely a billing optimization |

**Note:** Because cache requires a minimum of 1,024 tokens to activate, we combine our system prompt (~200 tokens) + tool definitions (~300 tokens) + a few-shot example (~500+ tokens) to reach the threshold. The few-shot example is the same for every call and shows Claude an example of a good match, improving output quality while also enabling caching.

---

## 6. The Final Pipeline

This section walks through exactly what happens when a bank statement arrives, step by step, in plain language.

### Step 0 — Bank Statement Arrives

A bank statement file (MT940, BAI2, or CSV format) arrives via SFTP or API. It contains a list of credit entries — incoming payments.

Example entry from the statement:
```
Date:       2024-04-10
Amount:     $47,230.00
Direction:  Credit (incoming)
Counterparty: "ACME CORP INT WIRE"
Reference:  "PAYMENT REF 88421"
Memo:       "" (blank)
```

Your Go/Java ingestion service parses this file and creates a structured record for each entry. This is pure code — no AI involved.

### Step 1 — Check for Short-Circuit (No AI Needed)

Before spending any money on AI, your code checks three things against your local database:

**Check 1: Exact account number match + exact amount**
- Does "ACME CORP INT WIRE" come from a bank account we have on file for a known customer?
- Does that customer have open invoices totaling exactly $47,230.00?
- If YES to both → auto-post to ERP. Done. Zero AI cost.

**Check 2: Invoice reference in bank memo**
- Does the reference "PAYMENT REF 88421" or any part of the memo match an open invoice number?
- If YES and the amount matches → auto-post. Done. Zero AI cost.

**Check 3: Single customer, single invoice, exact match**
- Is there exactly one customer with exactly one open invoice for exactly $47,230.00?
- If YES → skip Call 1 (we already know the customer), go directly to Call 2.

**This handles 30–40% of all transactions with zero AI cost.**

If none of these short-circuits match, we proceed to pre-filtering.

### Step 2 — Go/Java Pre-Filtering (No AI, Runs in Milliseconds)

Your code narrows down the customer from 500+ possibilities to 3–8 candidates using three filters:

**Filter 1: Fuzzy Name Match**
Compare "ACME CORP INT WIRE" against all customer names using trigram similarity.
```
"ACME CORP INT WIRE" vs "Acme Corp Ltd"          → 0.72 similarity ✓
"ACME CORP INT WIRE" vs "Acme Corporation"       → 0.65 similarity ✓
"ACME CORP INT WIRE" vs "Acme Corp International" → 0.78 similarity ✓
"ACME CORP INT WIRE" vs "Baker Industries"       → 0.05 similarity ✗
```
Returns top matches above threshold. Pure string comparison — no AI.

**Filter 2: Amount Combination Search**
Find all customers whose open invoices (individually or in combination) sum to $47,230.00 within 2% tolerance.
```
Acme Corp Ltd: INV-312 ($25,000) + INV-287 ($22,730) = $47,730 → within 2% ✓
Acme Corp International: no matching combination ✗
```
Pure math — no AI.

**Filter 3: Reference Lookup**
Check if "PAYMENT REF 88421" or "ACME CORP" matches any account numbers, customer codes, or known aliases in your database. Pure database lookup.

**Result:** A shortlist of 3–8 candidate customers with scores from each filter.

### Step 3 — AI Call 1: Customer Identification

Now we call Claude with the pre-filtered candidates. This is a small, focused call.

**What we send:**
```
System prompt: "You are a treasury cash application specialist..." (200 tokens, cached)
Tool schema: structured output definition (300 tokens, cached)
Few-shot example: one example of good customer identification (500 tokens, cached)
Transaction data + candidates: (300 tokens, new)
```

**What Claude returns (structured output):**
```json
{
  "customer_id": "CUST-0091",
  "customer_name": "Acme Corp Ltd",
  "confidence": 0.93,
  "reasoning": "Name similarity highest for Acme Corp Ltd. Wire payment matches their typical pattern. Amount matches two of their open invoices."
}
```

**Total tokens:** ~1,300 input + ~80 output = ~1,380 tokens
**Cost:** ~$0.005 (first call with cache write) or ~$0.002 (subsequent calls with cache read)

### Step 4 — Confidence Gate (Go/Java Code, No AI)

Your code looks at the confidence score and routes:

| Confidence | Action |
|-----------|--------|
| ≥ 0.90 | Proceed to Call 2 with this customer |
| 0.70–0.89 | Send top 2 candidates to Call 2 (invoice evidence often resolves the tie) |
| < 0.70 | Skip Call 2 — route directly to human review queue |

This is a simple if/else in your code. No AI.

### Step 5 — Fetch Invoice & Remittance Data (Go/Java, No AI)

Now that we know (or strongly suspect) the customer, your code:

1. Fetches open invoices for this customer from the local ERP cache
2. Searches for matching remittance emails using SQL:
   ```sql
   SELECT * FROM remittance_emails
   WHERE (sender_name ILIKE '%acme%' OR customer_id = 'CUST-0091')
     AND payment_amount BETWEEN 46200 AND 48200
     AND received_at BETWEEN '2024-04-01' AND '2024-04-11'
   ORDER BY received_at DESC LIMIT 3
   ```
3. If remittance email has a PDF attachment, parses it using LayoutLM (CPU, free) or the existing parsed structured data

All of this is code + SQL. No AI cost.

### Step 6 — AI Call 2: Invoice Matching

Now we call Claude with the full evidence package.

**What we send:**
```
System prompt: (200 tokens, cached)
Tool schema: invoice match output definition (300 tokens, cached)
Few-shot example: one example of good invoice matching (500 tokens, cached)
Customer + invoices + remittance data: (600 tokens, new)
```

**What Claude returns (structured output):**
```json
{
  "customer_id": "CUST-0091",
  "confidence": 0.97,
  "invoices": [
    {
      "id": "INV-312",
      "original_amount": 25000.00,
      "applied": 25000.00,
      "deduction": 0,
      "reason": null
    },
    {
      "id": "INV-287",
      "original_amount": 22730.00,
      "applied": 22230.00,
      "deduction": 500.00,
      "reason": "early_pay_discount"
    }
  ],
  "total_applied": 47230.00,
  "unmatched_amount": 0,
  "reasoning": "Payment of $47,230 matches INV-312 ($25,000) and INV-287 ($22,730) exactly. The $500 deduction on INV-287 is consistent with Acme Corp's 2% early-pay discount on invoices paid within 10 days. Remittance email confirms both invoice numbers."
}
```

**Total tokens:** ~1,600 input + ~200 output = ~1,800 tokens
**Cost:** ~$0.007 (first call) or ~$0.004 (subsequent calls with cache)

### Step 7 — Programmatic Guardrails (Go/Java, No AI)

Before acting on Claude's response, your code validates:

1. **Amounts balance:** Do the applied amounts sum to the payment amount? ($25,000 + $22,230 = $47,230 ✓)
2. **IDs exist:** Do INV-312 and INV-287 exist in your database? Are they open? ✓
3. **Deductions are within policy:** Is the $500 deduction within the approved discount range for this customer? ✓
4. **No double-matching:** Have these invoices already been matched to another payment? ✓

If any check fails, override the confidence to 0 and route to human review regardless of what Claude said.

### Step 8 — Final Routing (Go/Java, No AI)

| Condition | Action |
|-----------|--------|
| Confidence ≥ 0.95 AND zero unmatched AND guardrails pass | Auto-post to ERP. No human needed. |
| Confidence 0.70–0.94 OR guardrails flagged a concern | Human review queue. AI suggestion pre-filled. One-click approve/adjust/reject. |
| Confidence < 0.70 | Manual investigation queue. Flagged for treasury team. |

### Step 9 — Logging (Go/Java, No AI)

Every single match decision is logged to your audit table:
- The bank transaction details
- The AI's response (full JSON)
- The confidence score
- Whether it was auto-posted, human-reviewed, or manually investigated
- If human-reviewed: what the human changed (approved as-is, adjusted invoices, rejected)
- Timestamp, user ID, etc.

This log is critical for:
- Financial audit compliance
- Measuring AI accuracy over time
- Detecting confidence drift
- Training data for future fine-tuning (Phase 5+)

---

## 7. Prompt Design for Each AI Call

### 7.1 Call 1 — Customer Identification

**System Prompt (sent with every call, cached):**

```
You are a treasury cash application specialist at an oil & gas company. 
Your task is to identify which customer sent a bank payment.

You will receive:
- A bank transaction with amount, date, and counterparty description
- A shortlist of 3-8 candidate customers pre-filtered by name similarity and invoice amounts

Rules:
- Return exactly one customer match with a confidence score between 0.0 and 1.0
- Confidence 0.9+ means you are very sure this is the right customer
- Confidence 0.7-0.89 means probable but not certain
- Confidence below 0.7 means you cannot reliably determine the customer
- Base your confidence on: name similarity, payment amount alignment, 
  payment method consistency, and any reference numbers
- Do not guess. If the evidence is ambiguous, return low confidence.
```

**User Message (changes per transaction):**

```
Bank transaction:
  Amount: $47,230.00
  Date: 2024-04-10
  Counterparty: "ACME CORP INT WIRE"
  Reference: "PAYMENT REF 88421"
  
Candidate customers:
1. Acme Corp Ltd (CUST-0091) — typical wire payer, usually pays 2-3 invoices 
   together, open balance: $52,730
2. Acme Corporation (CUST-0445) — typically pays via ACH, open balance: $31,200
3. Acme Corp International (CUST-0201) — no payments in last 90 days, 
   open balance: $47,230

Which customer sent this payment?
```

**Structured Output Schema (tool definition, cached):**

```json
{
  "name": "identify_customer",
  "description": "Identify which customer sent this bank payment",
  "input_schema": {
    "type": "object",
    "required": ["customer_id", "customer_name", "confidence", "reasoning"],
    "properties": {
      "customer_id": {
        "type": "string",
        "description": "The customer ID from the candidate list"
      },
      "customer_name": {
        "type": "string",
        "description": "The customer name"
      },
      "confidence": {
        "type": "number",
        "minimum": 0,
        "maximum": 1,
        "description": "How confident you are (0.0 to 1.0)"
      },
      "reasoning": {
        "type": "string",
        "description": "Brief explanation of why this customer was selected"
      }
    }
  }
}
```

### 7.2 Call 2 — Invoice Matching

**System Prompt (same as Call 1, cached):**

Same system prompt with additional rules for invoice matching:

```
You are a treasury cash application specialist at an oil & gas company.
Your task is to match a bank payment to specific open invoices.

You will receive:
- A confirmed customer and payment details
- That customer's open invoices
- Remittance advice (if found) from email or PDF

Rules:
- Match the payment to specific invoices. The applied amounts must sum to 
  the payment amount.
- Account for early-pay discounts (typically 1-2% of invoice amount)
- Account for short-payments — note the deduction amount and categorize 
  the reason
- Valid deduction reasons: early_pay_discount, damaged_goods, 
  short_shipment, pricing_dispute, credit_memo, unknown
- If remittance advice lists specific invoices, prioritize that information
- If you cannot match the full amount, report the unmatched_amount
- Confidence 0.95+ means amounts match perfectly with clear evidence
- Confidence 0.70-0.94 means reasonable match but some ambiguity
- Below 0.70 means you cannot reliably match
```

**User Message (changes per transaction):**

```
Payment: $47,230.00 from Acme Corp Ltd (CUST-0091) on 2024-04-10

Open invoices for Acme Corp Ltd:
  INV-312 — $25,000.00 — due 2024-04-15 — fuel delivery March 28
  INV-287 — $22,730.00 — due 2024-04-12 — fuel delivery March 15
  INV-301 — $18,500.00 — due 2024-04-30 — equipment rental April

Remittance advice (from email received 2024-04-09):
  From: ap@acmecorp.com
  Subject: "Payment notification - April batch"
  Body: "Please find attached remittance for wire transfer April 10.
         INV-312: $25,000.00
         INV-287: $22,230.00 (2% early payment discount applied)
         Total: $47,230.00"

Match this payment to invoices.
```

**Structured Output Schema (tool definition, cached):**

```json
{
  "name": "submit_invoice_match",
  "description": "Submit the invoice matching result for this payment",
  "input_schema": {
    "type": "object",
    "required": ["customer_id", "confidence", "invoices", "total_applied", 
                  "unmatched_amount", "reasoning"],
    "properties": {
      "customer_id": {"type": "string"},
      "confidence": {"type": "number", "minimum": 0, "maximum": 1},
      "invoices": {
        "type": "array",
        "items": {
          "type": "object",
          "required": ["id", "original_amount", "applied"],
          "properties": {
            "id": {"type": "string"},
            "original_amount": {"type": "number"},
            "applied": {"type": "number"},
            "deduction": {"type": "number", "default": 0},
            "reason": {
              "type": "string",
              "enum": ["early_pay_discount", "damaged_goods", 
                       "short_shipment", "pricing_dispute", 
                       "credit_memo", "unknown", null]
            }
          }
        }
      },
      "total_applied": {"type": "number"},
      "unmatched_amount": {"type": "number"},
      "reasoning": {"type": "string"}
    }
  }
}
```

---

## 8. Token Usage and Cost Estimation

### 8.1 Token Breakdown Per Transaction Type

**Type A: Short-Circuit (No AI)**
Exact account match, invoice reference match, or single-customer exact-amount match.

| Component | Tokens | Cost |
|-----------|--------|------|
| AI calls | 0 | $0.000 |
| **Total** | **0** | **$0.000** |

Estimated frequency: 30–40% of transactions.

**Type B: Skip Call 1 (Single Candidate)**
Pre-filtering returns exactly one strong candidate. Go directly to Call 2.

| Component | Input Tokens | Output Tokens | Cost |
|-----------|-------------|---------------|------|
| Call 2 (cached prefix) | ~1,000 (cached) | — | $0.0003 |
| Call 2 (new content) | ~600 | — | $0.0018 |
| Call 2 output | — | ~200 | $0.0030 |
| **Total** | **~1,600** | **~200** | **~$0.005** |

Estimated frequency: 15–20% of transactions.

**Type C: Full Two-Call Chain**
Standard flow — Call 1 for customer ID, then Call 2 for invoice matching.

| Component | Input Tokens | Output Tokens | Cost |
|-----------|-------------|---------------|------|
| Call 1 (cached prefix) | ~1,000 (cached) | — | $0.0003 |
| Call 1 (new content) | ~300 | — | $0.0009 |
| Call 1 output | — | ~80 | $0.0012 |
| Call 2 (cached prefix) | ~1,000 (cached) | — | $0.0003 |
| Call 2 (new content) | ~600 | — | $0.0018 |
| Call 2 output | — | ~200 | $0.0030 |
| **Total** | **~2,900** | **~280** | **~$0.008** |

Estimated frequency: 35–45% of transactions.

**Type D: Call 1 Only (Low Confidence)**
Call 1 returns confidence < 0.70. Stop. Route to human review.

| Component | Input Tokens | Output Tokens | Cost |
|-----------|-------------|---------------|------|
| Call 1 (cached prefix) | ~1,000 (cached) | — | $0.0003 |
| Call 1 (new content) | ~300 | — | $0.0009 |
| Call 1 output | — | ~80 | $0.0012 |
| **Total** | **~1,300** | **~80** | **~$0.002** |

Estimated frequency: 5–10% of transactions.

### 8.2 Daily Cost Estimate (500 Transactions/Day)

| Transaction Type | Volume | Cost Each | Daily Cost |
|-----------------|--------|-----------|------------|
| Type A: Short-circuit (no AI) | ~175 (35%) | $0.000 | $0.00 |
| Type B: Call 2 only | ~90 (18%) | $0.005 | $0.45 |
| Type C: Full chain (Call 1 + Call 2) | ~200 (40%) | $0.008 | $1.60 |
| Type D: Call 1 only (low confidence) | ~35 (7%) | $0.002 | $0.07 |
| **Daily Total** | **500** | | **~$2.12** |

### 8.3 Monthly and Yearly Estimates

| Period | AI Cost | App Infrastructure | Total |
|--------|---------|--------------------|-------|
| Per day | ~$2.12 | — | — |
| Per month | ~$64 | ~$150 | **~$214** |
| Per year | ~$770 | ~$1,800 | **~$2,570** |

**Note:** These estimates assume Claude Sonnet pricing at $3/M input tokens and $15/M output tokens, with prompt caching providing 90% discount on cached prefixes after the first call. Actual costs may vary based on prompt lengths and caching hit rates.

### 8.4 Cost Comparison

| Approach | Monthly Cost |
|----------|-------------|
| Manual (1 treasury analyst) | $4,000–$8,000 salary |
| Our AI pipeline (Claude API + infrastructure) | ~$214 |
| **Savings** | **95–97%** |

---

## 9. Extended Thinking

### What It Is

Extended thinking gives Claude a private "scratchpad" to reason through a problem before producing its answer. You can see this thinking in the response, but it happens before the final structured output.

Without extended thinking:
```
Claude sees prompt → immediately outputs answer
```

With extended thinking:
```
Claude sees prompt → thinks for up to N tokens → outputs answer
```

### When It Helps

Extended thinking helps when Claude needs to work through multiple possibilities — like figuring out which combination of invoices adds up to the payment amount when there are deductions involved.

**Example where it helps:**

```
Payment: $47,230
Invoices: INV-301 ($15,000), INV-302 ($12,500), INV-303 ($10,000), 
          INV-304 ($10,230), INV-305 ($9,730), INV-306 ($22,230)
No remittance email found.

Without thinking: Claude might pick the obvious pair and miss a better combination
With thinking: Claude systematically tries combinations and catches that 
               INV-301 + INV-306 = $37,230 + discount scenario
```

### Cost Impact

```
budget_tokens: 2000 (recommended for Call 2 testing)

Extra cost per Call 2: ~2,000 tokens × $15/M = ~$0.03
If applied to all Call 2 transactions (~290/day): ~$8.70/day or ~$261/month

That's significant. Only use if accuracy improvement justifies it.
```

### Our Recommendation

- **Call 1:** Do NOT use extended thinking. Customer identification is straightforward — the evidence is all in the prompt.
- **Call 2:** A/B test during Phase 3. Run 200 transactions with and 200 without. Compare accuracy. If extended thinking catches 5+ matches that the standard call misses, it's worth the cost. If not, skip it.

**How to enable it:**

```json
{
  "model": "claude-sonnet-4-20250514",
  "max_tokens": 8000,
  "thinking": {
    "type": "enabled",
    "budget_tokens": 2000
  },
  "tools": [...],
  "tool_choice": {"type": "tool", "name": "submit_invoice_match"},
  "messages": [...]
}
```

**Important:** When extended thinking is enabled, `max_tokens` must be larger than `budget_tokens`. The thinking tokens count toward the `max_tokens` limit. Set `max_tokens` to `budget_tokens + expected_output` (e.g., 2000 + 1024 = 3024, round up to 4000 or 8000 for safety).

---

## 10. Architecture Diagram

### The Full System

```
┌─────────────────────────────────────────────────────────────────────┐
│                     EXTERNAL SYSTEMS                                │
│  Bank SFTP/API    │    ERP (SAP/NetSuite)    │    Email Server      │
└────────┬──────────┴────────────┬──────────────┴──────────┬──────────┘
         │                      │                         │
         ▼                      ▼                         ▼
┌─────────────────────────────────────────────────────────────────────┐
│                     YOUR INFRASTRUCTURE                             │
│                                                                     │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │                  INGESTION LAYER                              │   │
│  │                                                               │   │
│  │  Bank File Parser ──→ Postgres ←── ERP Sync ←── Email Sync  │   │
│  │  (MT940/BAI2/CSV)     (local cache)                          │   │
│  └──────────────────────────┬───────────────────────────────────┘   │
│                             │                                       │
│                             ▼                                       │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │               MATCHING ORCHESTRATOR (Go/Java)                │   │
│  │                                                               │   │
│  │  For each bank credit entry:                                  │   │
│  │                                                               │   │
│  │  1. Short-circuit check ──── match found? ──→ AUTO-POST      │   │
│  │     │ no match                                                │   │
│  │     ▼                                                         │   │
│  │  2. Pre-filter (fuzzy name + amount search + ref lookup)      │   │
│  │     │ 3-8 candidates                                          │   │
│  │     ▼                                                         │   │
│  │  3. AI Call 1: Customer ID ─── confidence < 0.70? ──→ HUMAN  │   │
│  │     │ confident                                               │   │
│  │     ▼                                                         │   │
│  │  4. Fetch invoices + search remittance emails (SQL)           │   │
│  │     │ data ready                                              │   │
│  │     ▼                                                         │   │
│  │  5. AI Call 2: Invoice Match                                  │   │
│  │     │ result                                                  │   │
│  │     ▼                                                         │   │
│  │  6. Guardrails (amounts balance? IDs exist? policy OK?)       │   │
│  │     │ validated                                               │   │
│  │     ▼                                                         │   │
│  │  7. Route: ≥0.95 → AUTO-POST │ 0.70-0.94 → HUMAN │ <0.70 → MANUAL │
│  │                                                               │   │
│  │  8. Log everything to audit table                             │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                             │                                       │
│              ┌──────────────┼──────────────┐                       │
│              ▼              ▼              ▼                        │
│         AUTO-POST     HUMAN REVIEW    MANUAL QUEUE                 │
│         (ERP API)     (React UI)      (Treasury team)              │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
                              │
            Only for AI Call 1 & Call 2:
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│                     CLAUDE API (HTTPS)                              │
│                                                                     │
│  Receives: Small pre-filtered prompt (~1,000-1,600 tokens)         │
│  Returns:  Structured JSON (via tool use schema)                   │
│  Never sees: Full ERP database, bank accounts, employee data       │
│                                                                     │
│  Settings:                                                          │
│    model: claude-sonnet-4                                           │
│    temperature: 0                                                   │
│    prompt caching: enabled on system prompt + tool schemas          │
│    structured outputs: forced via tool_choice                       │
└─────────────────────────────────────────────────────────────────────┘
```

### What Each Layer Does

| Layer | Technology | Cost | Handles |
|-------|-----------|------|---------|
| Ingestion | Go/Java + Postgres | ~$150/month (servers + DB) | Data movement, parsing, caching |
| Orchestrator | Go/Java | (runs on same servers) | Pre-filtering, routing, guardrails |
| AI (Claude API) | HTTPS call | ~$64/month | Customer ID + invoice matching only |
| Human Review | React UI | ~$5/month (S3 hosting) | Exception handling |
| **Total** | | **~$214/month** | **70-90% automated matching** |

---

## 11. OpenAI API — Parameters, Features & Comparison with Claude

Our pipeline is designed to be provider-agnostic. The Go/Java orchestrator calls an AI API endpoint — the same code works for Claude, OpenAI, or a self-hosted model. This section covers OpenAI's API parameters, unique features, and how they compare.

### 11.1 OpenAI API Parameters

#### `model`

Which OpenAI model to use. OpenAI has multiple model families:

| Model | Input $/1M | Cached Input $/1M | Output $/1M | Best For |
|-------|-----------|-------------------|-------------|----------|
| gpt-5.4 | $2.50 | $0.25 | $15.00 | Highest quality, complex reasoning |
| gpt-5.4-mini | $0.75 | $0.075 | $4.50 | Good quality, lower cost |
| gpt-5.4-nano | $0.20 | $0.02 | $1.25 | Cheapest, simple tasks |
| gpt-4o | $2.50 | $1.25 | $10.00 | Previous gen, still capable |
| gpt-4o-mini | $0.15 | $0.075 | $0.60 | Previous gen, very cheap |

**For our pipeline:** `gpt-5.4-nano` is the most interesting option. At $0.20/1M input and $1.25/1M output, it's significantly cheaper than Claude Sonnet ($3.00/$15.00). The question is whether it's accurate enough for invoice matching — this needs A/B testing.

#### `max_completion_tokens` (NOT `max_tokens`)

**Important breaking change:** OpenAI's GPT-5.x models renamed `max_tokens` to `max_completion_tokens`. Using the old name returns a 400 error. This is the maximum number of tokens the model will generate.

```json
// WRONG — will fail on GPT-5.x
{"max_tokens": 1024}

// CORRECT for GPT-5.x
{"max_completion_tokens": 1024}

// CORRECT for older models (GPT-4o, GPT-4o-mini)
{"max_tokens": 1024}
```

If you're building a provider-agnostic system, detect the model family and send the right parameter name.

#### `temperature`

Same concept as Claude. Range is 0–2 (Claude is 0–1).

```json
"temperature": 0   // deterministic — same as Claude
```

**Important:** On GPT-5.x reasoning models, `temperature` is only supported when `reasoning.effort` is set to `"none"`. With any other reasoning effort, temperature is ignored and the request will error if you include it.

#### `seed`

OpenAI offers a `seed` parameter that Claude does not have. When set to a fixed integer, the model attempts to produce deterministic output.

```json
"seed": 42,
"temperature": 0
```

**How it works:** With the same `seed`, same `temperature`, and same prompt, OpenAI will try to return the same output. However, this is "best effort" — not guaranteed. The response includes a `system_fingerprint` field; if this value changes between requests (due to infrastructure updates), outputs may differ.

**For our pipeline:** Useful for testing and debugging. Set `seed: 42` (or any fixed number) during development so you can reproduce results. In production, it provides an extra layer of determinism on top of `temperature: 0`.

**Claude comparison:** Claude does not offer a `seed` parameter. With `temperature: 0`, Claude is reasonably deterministic, but has no explicit mechanism for reproducibility.

#### `response_format` (Structured Outputs — OpenAI's approach)

OpenAI offers two ways to get structured output:

**Method 1: Via Tools (same concept as Claude)**
```json
{
  "tools": [{
    "type": "function",
    "function": {
      "name": "submit_match",
      "strict": true,
      "parameters": {
        "type": "object",
        "properties": {
          "customer_id": {"type": "string"},
          "confidence": {"type": "number"}
        },
        "required": ["customer_id", "confidence"]
      }
    }
  }],
  "tool_choice": {"type": "function", "function": {"name": "submit_match"}}
}
```

Note the `"strict": true` — this is OpenAI's way of enforcing schema adherence. Without it, the model may return JSON that doesn't match your schema.

**Method 2: Via response_format (OpenAI-only feature)**

OpenAI also lets you enforce structured output without tools, using `response_format`:

```json
{
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "match_result",
      "strict": true,
      "schema": {
        "type": "object",
        "properties": {
          "customer_id": {"type": "string"},
          "confidence": {"type": "number"},
          "invoices": {"type": "array", "items": {...}}
        },
        "required": ["customer_id", "confidence", "invoices"]
      }
    }
  }
}
```

**This is simpler than the tool approach** — you don't need to define a "tool" just to get structured output. The model returns the JSON directly in its response text, no tool_use parsing needed.

**Claude comparison:** Claude only supports structured output via the tool use mechanism. OpenAI's `response_format` with `json_schema` is a cleaner API for cases where you just want structured output without pretending it's a tool call.

**Recommendation:** If using OpenAI, prefer `response_format` with `json_schema` for our pipeline. It's simpler and produces the same result.

#### `reasoning` (OpenAI's version of Extended Thinking)

GPT-5.x models have built-in reasoning capabilities controlled by the `reasoning` parameter:

```json
{
  "model": "gpt-5.4-nano",
  "reasoning": {
    "effort": "low"
  }
}
```

| Effort Level | When to Use | Token Impact |
|-------------|-------------|-------------|
| `none` | Simple extraction, routing — no thinking needed | Lowest cost, lowest latency |
| `minimal` | Fast responses, light reasoning | Very low overhead |
| `low` | Simple matching tasks (like our Call 1) | Low overhead |
| `medium` | Moderate complexity (standard Call 2) | Moderate overhead |
| `high` | Complex multi-invoice matching with deductions | Higher cost, better accuracy |
| `xhigh` | Hardest problems — rarely needed | Highest cost |

**Key difference from Claude:** Claude's extended thinking uses `budget_tokens` — you specify a maximum number of thinking tokens. OpenAI uses discrete effort levels (`none` through `xhigh`) — you tell the model how hard to think, not how many tokens to use.

**For our pipeline:**
- Call 1 (Customer ID): `reasoning.effort: "low"` or `"none"` — this is a simple identification task
- Call 2 (Invoice Match): `reasoning.effort: "medium"` — needs some reasoning for deduction handling

**Important:** When using reasoning effort other than `"none"`, you cannot use `temperature`, `top_p`, or `logprobs`. The model controls its own sampling during reasoning.

### 11.2 OpenAI Prompt Caching — Key Differences from Claude

OpenAI's prompt caching works differently from Claude's in important ways:

| Feature | Claude | OpenAI |
|---------|--------|--------|
| How to enable | Manually add `cache_control` flag to content blocks | **Automatic** — no code changes needed |
| Minimum tokens to cache | 1,024 tokens | 1,024 tokens |
| Cache TTL (default) | 5 minutes | 5–10 minutes |
| Extended cache TTL | Not available | **24 hours** (set `prompt_cache_retention: "24h"`) |
| Cache write cost | 25% surcharge on first write | **No surcharge** — free to write |
| Cache read discount | 90% off | 90% off |
| Cache verification | Check response headers | Check `usage.prompt_tokens_details.cached_tokens` |

**OpenAI advantages:**
1. **Automatic** — no code changes needed. If your prompt is >1,024 tokens and you send the same prefix repeatedly, caching just works.
2. **No write surcharge** — Claude charges 25% extra on the first cache write. OpenAI doesn't.
3. **24-hour retention** — you can set `prompt_cache_retention: "24h"` so your cache persists overnight. Claude's cache expires after 5 minutes of inactivity. This is useful if your bank statement processing spans multiple hours or runs at different times of day.

**For our pipeline:** OpenAI's automatic caching with 24-hour retention is simpler and potentially cheaper than Claude's manual cache control.

```json
{
  "model": "gpt-5.4-nano",
  "prompt_cache_retention": "24h",
  "messages": [
    {"role": "system", "content": "You are a treasury cash application specialist..."},
    {"role": "user", "content": "...transaction specific data..."}
  ]
}
```

No `cache_control` annotations needed — OpenAI figures it out automatically.

### 11.3 Batch API — 50% Cost Discount (OpenAI-Only Feature)

This is the most impactful OpenAI-only feature for our use case.

**What it is:** Instead of sending API requests one at a time (synchronous), you upload a file containing all your requests at once. OpenAI processes them in the background and returns results within 24 hours — at **50% off all token costs**.

**Why this is perfect for our pipeline:**
- Bank statements arrive in batches (once or twice per day)
- We don't need real-time responses — processing within a few hours is fine
- 500 transactions per day is a perfect batch workload

**How it works:**

Step 1: Build a JSONL file with one request per line:
```jsonl
{"custom_id": "txn-001-call1", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-5.4-nano", "max_completion_tokens": 300, "temperature": 0, "messages": [{"role": "system", "content": "You are a treasury..."}, {"role": "user", "content": "Bank transaction: $47,230..."}], "tools": [...]}}
{"custom_id": "txn-002-call1", "method": "POST", "url": "/v1/chat/completions", "body": {"model": "gpt-5.4-nano", "max_completion_tokens": 300, "temperature": 0, "messages": [{"role": "system", "content": "You are a treasury..."}, {"role": "user", "content": "Bank transaction: $12,500..."}], "tools": [...]}}
```

Step 2: Upload the file and create a batch:
```go
// Upload the JSONL file
file, _ := client.Files.Create(ctx, openai.FileCreateParams{
    File:    os.Open("batch_call1.jsonl"),
    Purpose: "batch",
})

// Create the batch job
batch, _ := client.Batches.Create(ctx, openai.BatchCreateParams{
    InputFileID:      file.ID,
    Endpoint:         "/v1/chat/completions",
    CompletionWindow: "24h",
})
```

Step 3: Poll for completion and download results:
```go
// Check status
batch, _ := client.Batches.Retrieve(ctx, batch.ID)
if batch.Status == "completed" {
    results, _ := client.Files.Content(ctx, batch.OutputFileID)
    // Parse results — each line has custom_id + response
}
```

**Cost impact with Batch API:**

| Component | Synchronous (real-time) | Batch API (50% off) |
|-----------|------------------------|---------------------|
| gpt-5.4-nano input | $0.20/1M | $0.10/1M |
| gpt-5.4-nano cached input | $0.02/1M | $0.01/1M |
| gpt-5.4-nano output | $1.25/1M | $0.625/1M |

**Combined savings (Batch API + caching) on gpt-5.4-nano:**

```
Per transaction (full two-call chain):
  Call 1: (1,000 cached × $0.01/M) + (300 new × $0.10/M) + (80 output × $0.625/M) = $0.00009
  Call 2: (1,000 cached × $0.01/M) + (600 new × $0.10/M) + (200 output × $0.625/M) = $0.0002

  Total per transaction: ~$0.0003

  Compare to Claude Sonnet (synchronous, with caching): ~$0.006 per transaction
  
  OpenAI gpt-5.4-nano + Batch is ~20x cheaper than Claude Sonnet.
```

**Daily/Monthly cost with Batch API on gpt-5.4-nano:**

| Period | Cost |
|--------|------|
| Per day (500 txns) | ~$0.05 – $0.15 |
| Per month | ~$1.50 – $4.50 |
| Per year | ~$18 – $54 |

**The catch:** Batch API has a 24-hour completion window. Usually finishes much faster (minutes to a couple hours), but no SLA for speed. If you need same-hour processing, use synchronous calls. If next-day is fine, use Batch API for massive savings.

**Claude comparison:** Anthropic offers a similar Message Batches API with 50% discount, but OpenAI's Batch API is more mature and widely used. Both are viable — use whichever provider you choose.

**Pipeline modification for Batch API:**

```
Current pipeline (synchronous — one at a time):
  Transaction 1 → Call 1 → wait → Call 2 → wait → done
  Transaction 2 → Call 1 → wait → Call 2 → wait → done
  ...
  Total time: ~500 × 2 seconds = ~17 minutes

Batch pipeline (all at once):
  Step 1: Build all Call 1 requests → upload batch → wait for results (~minutes)
  Step 2: Process Call 1 results, build Call 2 requests for confident matches
  Step 3: Upload Call 2 batch → wait for results (~minutes)  
  Step 4: Process Call 2 results → route to auto-post or human review
  Total time: ~10-30 minutes for all 500 transactions
```

This two-step batch approach means two batch jobs per bank statement, but cuts cost in half.

### 11.4 OpenAI `seed` + `system_fingerprint` — Reproducibility

OpenAI provides a stronger reproducibility mechanism than Claude:

```json
{
  "model": "gpt-5.4-nano",
  "temperature": 0,
  "seed": 42,
  "messages": [...]
}
```

The response includes:
```json
{
  "system_fingerprint": "fp_abc123",
  "choices": [...]
}
```

**How to use for our pipeline:**
1. Set a fixed `seed` (e.g., 42) for all requests
2. Log the `system_fingerprint` from every response
3. If you re-process a transaction and get a different result, check if `system_fingerprint` changed — if it did, OpenAI updated their infrastructure (this happens a few times per year)

**This is valuable for financial audit compliance** — you can prove that the same input produces the same output, and detect when infrastructure changes might have affected results.

### 11.5 Other OpenAI Parameters

#### `frequency_penalty` and `presence_penalty`

These reduce repetition in generated text. Range: -2.0 to 2.0.

**For our pipeline:** Not useful. Our outputs are structured JSON, not prose. Set both to 0 (default).

#### `logprobs`

Returns the probability the model assigned to each token it generated. Only supported with `reasoning.effort: "none"`.

**For our pipeline:** Potentially useful for confidence calibration. Instead of trusting the model's self-reported confidence score, you could look at the actual token probabilities to see how "sure" the model was. This is an advanced optimization for Phase 4+.

#### `top_p` (Nucleus Sampling)

Alternative to temperature for controlling randomness. If using temperature, leave top_p at default (1.0). Don't use both simultaneously.

**For our pipeline:** Not needed. We use temperature 0.

### 11.6 Predicted Outputs (OpenAI-Only)

**What it is:** If you know roughly what the output will look like, you can send a "prediction" and OpenAI skips generating tokens that match your prediction. Reduces latency.

**For our pipeline:** Not applicable. Predicted Outputs doesn't work with tool/function calling or structured outputs, which we use. Also, our outputs vary per transaction so we can't predict them.

### 11.7 Full OpenAI API Request Example

Here's the complete request for our Call 1 using OpenAI:

```json
{
  "model": "gpt-5.4-nano",
  "max_completion_tokens": 300,
  "temperature": 0,
  "seed": 42,
  "prompt_cache_retention": "24h",
  "messages": [
    {
      "role": "system",
      "content": "You are a treasury cash application specialist at an oil & gas company. Your task is to identify which customer sent a bank payment.\n\nRules:\n- Return exactly one customer match with a confidence score between 0.0 and 1.0\n- Confidence 0.9+ means very sure\n- Confidence 0.7-0.89 means probable but not certain\n- Below 0.7 means you cannot reliably determine the customer\n- Do not guess. If evidence is ambiguous, return low confidence."
    },
    {
      "role": "user",
      "content": "Bank transaction:\n  Amount: $47,230.00\n  Date: 2024-04-10\n  Counterparty: \"ACME CORP INT WIRE\"\n  Reference: \"PAYMENT REF 88421\"\n\nCandidate customers:\n1. Acme Corp Ltd (CUST-0091) — typical wire payer, open balance: $52,730\n2. Acme Corporation (CUST-0445) — typically pays via ACH, open balance: $31,200\n3. Acme Corp International (CUST-0201) — no recent activity, open balance: $47,230\n\nWhich customer sent this payment?"
    }
  ],
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "customer_identification",
      "strict": true,
      "schema": {
        "type": "object",
        "required": ["customer_id", "customer_name", "confidence", "reasoning"],
        "properties": {
          "customer_id": {"type": "string"},
          "customer_name": {"type": "string"},
          "confidence": {"type": "number"},
          "reasoning": {"type": "string"}
        },
        "additionalProperties": false
      }
    }
  }
}
```

Note: Using `response_format` instead of `tools` — simpler, and the JSON comes back directly in the message content instead of a tool_use block.

### 11.8 Claude vs OpenAI — Side-by-Side Comparison for Our Pipeline

| Feature | Claude (Anthropic) | OpenAI |
|---------|-------------------|--------|
| **Best model for our use case** | Claude Sonnet 4 | gpt-5.4-nano or gpt-5.4-mini |
| **Input pricing** | $3.00/1M | $0.20/1M (nano) or $0.75/1M (mini) |
| **Output pricing** | $15.00/1M | $1.25/1M (nano) or $4.50/1M (mini) |
| **Structured outputs** | Via tool use only | Via tool use OR response_format (simpler) |
| **Prompt caching** | Manual (add cache_control flag) | Automatic (no code changes) |
| **Cache write cost** | 25% surcharge | Free |
| **Cache TTL** | 5 minutes | 5-10 min default, or **24 hours** |
| **Batch API** | Message Batches (50% off) | Batch API (50% off) |
| **Determinism** | temperature: 0 | temperature: 0 + seed parameter |
| **Reasoning control** | budget_tokens (token count) | reasoning.effort (discrete levels) |
| **Quality for our task** | Likely best accuracy | Needs testing — nano may be sufficient |

### 11.9 Cost Comparison — Same Pipeline, Different Providers

All estimates assume 500 transactions/day with the transaction type distribution from Section 8.

**Scenario 1: Claude Sonnet (synchronous, with caching)**

| Period | AI Cost |
|--------|---------|
| Per day | ~$2.12 |
| Per month | ~$64 |
| Per year | ~$770 |

**Scenario 2: OpenAI gpt-5.4-nano (synchronous, with automatic caching)**

| Period | AI Cost |
|--------|---------|
| Per day | ~$0.30 |
| Per month | ~$9 |
| Per year | ~$108 |

**Scenario 3: OpenAI gpt-5.4-nano (Batch API, with caching — cheapest option)**

| Period | AI Cost |
|--------|---------|
| Per day | ~$0.15 |
| Per month | ~$4.50 |
| Per year | ~$54 |

**Scenario 4: OpenAI gpt-5.4-mini (synchronous, with caching — better quality)**

| Period | AI Cost |
|--------|---------|
| Per day | ~$0.85 |
| Per month | ~$26 |
| Per year | ~$312 |

### 11.10 Recommendation: Which Provider to Use

**Phase 1 (POC):** Use Claude Sonnet. It has the best matching quality, and at ~$64/month the cost is negligible while you're validating accuracy. Don't optimize cost until you know the pipeline works.

**Phase 2 (Production):** A/B test OpenAI gpt-5.4-nano and gpt-5.4-mini against Claude Sonnet on 200+ real transactions. Compare:
- Match accuracy (% of correct customer identifications)
- Invoice matching accuracy (% of correct invoice-to-payment matches)
- Deduction handling accuracy (correctly identified discount amounts and reasons)

If gpt-5.4-nano achieves >95% of Claude's accuracy, switch to it for a ~7x cost reduction.

If accuracy is critical and nano isn't good enough, use gpt-5.4-mini for a ~2.5x cost reduction while maintaining quality.

**Phase 3 (Optimization):** Switch to Batch API if same-day (not same-hour) processing is acceptable. This cuts whatever provider cost you're paying in half.

**Provider-agnostic code:** Design your Go/Java orchestrator to be provider-agnostic from day one. Both APIs use similar request formats. The main differences to abstract:

```go
type AIProvider interface {
    CallCustomerID(prompt string) (*CustomerIDResult, error)
    CallInvoiceMatch(prompt string) (*InvoiceMatchResult, error)
}

type ClaudeProvider struct { /* uses tool_use for structured output */ }
type OpenAIProvider struct { /* uses response_format for structured output */ }
```

This lets you switch providers or A/B test without changing the rest of your pipeline.

---

## Appendix: Decision Summary

| Approach | Decision | Key Reason |
|----------|----------|------------|
| RAG | ❌ Rejected | SQL search works for structured B2B remittance data. Same LLM context either way. |
| MCP | ❌ Rejected | Batch pipeline, not interactive. Orchestrator already knows what to fetch. |
| Tool Use (data fetching) | ❌ Rejected | Adds round-trip API calls. Orchestrator already assembles all context. |
| Agentic pipeline | ❌ Rejected | Workflow is linear and predictable. 3-5x cost for no benefit. |
| Shared context between txns | ❌ Rejected | Cost grows linearly. Cross-txn intelligence better handled in DB. |
| Structured outputs | ✅ Accepted | Zero extra cost. Guaranteed valid JSON. Essential for financial system. |
| Prompt caching | ✅ Accepted | ~50% input cost savings. Free to implement. |
| Temperature 0 | ✅ Accepted | Deterministic financial decisions. Reproducible results. |
| Two-stage call chain | ✅ Accepted | Cheaper per call, focused tasks, confidence gating between calls. |
| Short-circuit rules | ✅ Accepted | 30-40% of transactions resolved with zero AI cost. |
| Programmatic guardrails | ✅ Accepted | Safety net before ERP posting. Validates AI output programmatically. |
| Extended thinking | ⏳ A/B test in Phase 3 | Potential accuracy gain on complex matches. Cost increase needs justification. |
| Fine-tuning | ⏳ Phase 5+ | Requires 6+ months of labeled data. Reduces cost and improves accuracy long-term. |
