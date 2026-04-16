## Usage

```
go run main.go -data ./data -mode <mode> [-txn TXN-001] [-verbose]
```

| Flag       | Default  | Options                                                  |
| ---------- | -------- | -------------------------------------------------------- |
| `-data`    | `/data`  | path to your data folder                                 |
| `-mode`    | `single` | `single`, `multi`, `orchestrator`, `orchestrator-v2`, `funnel`, or `reactive` |
| `-txn`     | (all)    | e.g. `TXN-001` to run one transaction                    |
| `-verbose` | off      | add flag to print full API request/response              |

## Modes

### `single` — Single Agent

One agent does everything end-to-end.

```
Transaction → Agent (has all tools) → loops: call tools, read results → submit_match_result
```

- 1 agent, all tools available
- ~3-6 API calls per transaction (tool-calling rounds)

```bash
go run main.go -data ./frost-data
go run main.go -data ./data -txn TXN-001 -verbose

go run main.go -data ./frost-data -txn FROST-020 -verbose
```

### `multi` — Multi-Agent (parallel subagents + merger)

3 specialist subagents always run in parallel, a merger agent combines their findings.

```
Transaction → NameAgent (has name tools)    ──┐
            → AmountAgent (has amount tools) ──┼→ MergerAgent (has verification tools) → submit_match_result
            → EmailAgent (has email tools)   ──┘
```

- Always runs all 3 subagents (each has tools, makes 1-3 API calls)
- Merger agent verifies and decides (1-4 API calls)
- ~8-13 API calls total

```bash
go run main.go -data ./data -mode multi
go run main.go -data ./data -mode multi -txn TXN-001 -verbose
```

### `orchestrator` — Multi-Agent Orchestrator

Main agent plans which subagents to use with guidance, subagents investigate with tools, main agent synthesizes.

```
Transaction → Planner (1 API call, picks subagents + guidance)
            → Selected subagents run in parallel (each has tools, 1-3 API calls)
            → Synthesizer (has verification tools) → submit_match_result
```

- Planner decides which subagents to dispatch (can skip unnecessary ones)
- Each subagent still has tools and makes its own tool calls
- ~5-11 API calls total

```bash
go run main.go -data ./data -mode orchestrator
go run main.go -data ./data -mode orchestrator -txn TXN-001 -verbose
```

### `orchestrator-v2` — Orchestrator V2 (data-fetch-then-reason)

Main agent plans what data to fetch, tools run locally (0 API calls), analyst subagents receive pre-fetched data with no tools (1 API call each), main agent synthesizes.

```
Transaction → Planner (1 API call: decides tools + analyst assignments)
            → Execute tools locally (0 API calls)
            → Analyst subagents in parallel (1 API call each, NO tools, data-only)
            → Synthesizer (has verification tools) → submit_match_result
```

- Separates data-fetching (cheap, local) from reasoning (LLM)
- Analyst subagents have NO tools — they just analyze pre-fetched data and give a structured opinion
- Smallest context windows for subagents (no tool definitions in prompt)
- ~4-7 API calls total

```bash
go run main.go -data ./frost-data -mode orchestrator-v2
go run main.go -data ./data -mode orchestrator-v2 -txn TXN-001 -verbose
```

### `funnel` — Amount-Anchored Funnel (pre-fetch → triage → resolve)

Pre-fetches amount matches and remittance emails deterministically (0 LLM calls), then a triage agent decides if the pre-fetched data is enough to match — if yes, done in 1 LLM call. If not, it requests specific follow-up tools which run locally, then a resolver agent makes the final decision with all data (2 LLM calls total).

```
Transaction → Phase 0: Deterministic pre-fetch (0 LLM calls)
                ├── search_customers_by_amount (±3%)
                └── search_remittance_emails (amount ±5%, date ±7 days)
            → Phase 1: Triage agent (1 LLM call, NO tools, data-only)
                ├── decision="match" → DONE (1 LLM call total)
                └── decision="need_more" + tool shopping list
            → Phase 2a: Execute requested tools locally (0 LLM calls)
            → Phase 2b: Resolver agent (1 LLM call, NO tools) → final match
```

- NO LLM ever has API tools — all tool execution is deterministic/local
- Easy transactions resolve in 1 LLM call (triage matches directly from pre-fetched data)
- Hard transactions resolve in 2 LLM calls (triage + resolver)
- ~1-2 API calls total (cheapest pipeline)

```bash
go run main.go -data ./data -mode funnel
go run main.go -data ./data -mode funnel -txn TXN-001 -verbose
```

### `reactive` — Reactive Chain (pre-fetch → agent with tools → escalate)

Pre-fetches amount matches and remittance emails deterministically, then a single agent receives the pre-fetched data AND has access to interpretation-dependent tools (name search, reference lookup, customer details). The agent is instructed to use pre-fetched data first and only call tools if needed (max 3 rounds). If it doesn't match or has low confidence (< 0.80), a second escalation agent picks up with wider tools and 3 more rounds.

```
Transaction → Phase 0: Deterministic pre-fetch (0 LLM calls)
                ├── search_customers_by_amount (±3%)
                └── search_remittance_emails (amount ±5%, date ±7 days)
            → Phase 1: Primary agent (has tools, pre-fed data, max 3 rounds)
                ├── Confident match (≥0.80) → DONE
                └── No match / low confidence → escalate
            → Phase 2: Escalation agent (wider tools, max 3 rounds) → final match
```

- Like the single agent but starts pre-informed with amount + email data
- Primary agent's tool set excludes pre-fed tools (no wasted calls)
- Escalation agent gets the primary agent's work summary + wider tool access
- Easy transactions: 1 LLM call, 0 tool calls (matches from pre-fetched data alone)
- Hard transactions: 2 LLM calls, 1-6 tool calls across both agents
- ~1-2 API calls total (comparable to funnel, but with tool flexibility)

```bash
go run main.go -data ./data -mode reactive
go run main.go -data ./data -mode reactive -txn TXN-001 -verbose
```
