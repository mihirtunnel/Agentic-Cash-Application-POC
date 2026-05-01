# Outlook Integration Plan

## Overview

Add Outlook email support alongside the existing Gmail integration. The Outlook flow mirrors Gmail exactly:
**Azure App credentials → OAuth2 token → Microsoft Graph API → raw email bytes → existing pipeline**

Everything after fetching raw bytes (PDF extraction, AI extraction, results) is reused unchanged.

---

## Part 1 — Getting API Credentials (Personal Outlook)

You need a free Azure App Registration. No paid subscription required for personal `@outlook.com` / `@hotmail.com` / `@live.com` accounts.

### Step-by-step: Register an Azure App

**1. Sign in to Azure Portal**

- Go to [https://portal.azure.com](https://portal.azure.com) and sign in with your **personal Microsoft account** (the same one that owns the Outlook inbox).

**2. Create an App Registration**

- Search for "App registrations" in the top search bar → click it
- Click **"+ New registration"**
- Fill in:
  - **Name**: `remittance-poc` (or any name you like)
  - **Supported account types**: Select **"Personal Microsoft accounts only"** (the third option — Outlook.com, Hotmail, etc.)
  - **Redirect URI**: Choose **"Public client/native (mobile & desktop)"** and enter:
    ```
    http://localhost:8080/callback
    ```
- Click **Register**

**3. Copy your Client ID**

- After registration, you land on the app's Overview page
- Copy the **"Application (client) ID"** — this is your `OUTLOOK_CLIENT_ID`
- For personal accounts the tenant is `consumers` (not your Directory/Tenant ID)

**4. Add API Permissions**

- Left sidebar → **"API permissions"** → **"+ Add a permission"**
- Choose **"Microsoft Graph"** → **"Delegated permissions"**
- Search and add:
  - `Mail.Read`
  - `offline_access` (required for refresh tokens so you don't re-auth every hour)
- Click **"Add permissions"**
- No admin consent needed for personal accounts

**5. Enable public client flow**

- Left sidebar → **"Authentication"**
- Scroll to **"Advanced settings"** → set **"Allow public client flows"** to **Yes**
- Click **Save**

**6. Add to your `.env` file**

```
OUTLOOK_CLIENT_ID=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
OUTLOOK_TOKEN_FILE=outlook_token.json
```

No client secret is needed — this uses the PKCE public client flow (same security, no secret to store or rotate).

---

## Part 2 — Code Changes

### New files to create


| File                               | Purpose                                              |
| ---------------------------------- | ---------------------------------------------------- |
| `internal/outlookclient/client.go` | Auth + fetch layer (mirrors `gmailclient/client.go`) |
| `cmd/outlook_sync/main.go`         | CLI entry point (mirrors `cmd/gmail_sync/main.go`)   |


### Files to update


| File           | Change                                                |
| -------------- | ----------------------------------------------------- |
| `.env.example` | Add `OUTLOOK_CLIENT_ID` and `OUTLOOK_TOKEN_FILE` vars |
| `CLAUDE.md`    | Document the new `outlook_sync` command               |


### No new Go dependencies needed

`golang.org/x/oauth2` is already in `go.mod` (used by Gmail). Microsoft's OAuth2 endpoint is available via the same package.

---

## Part 3 — Implementation Details

### `internal/outlookclient/client.go`


| Gmail                                       | Outlook equivalent                                                                                                                    |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `google.ConfigFromJSON(b, scope)`           | Manual `oauth2.Config` with `ClientID`, `Endpoint: microsoft.AzureADEndpoint("consumers")`, `Scopes: ["Mail.Read", "offline_access"]` |
| `svc.Users.Messages.List(...).Q(query)`     | `GET https://graph.microsoft.com/v1.0/me/messages?$filter=...&$select=id,subject`                                                     |
| `svc.Users.Messages.Get(...).Format("raw")` | `GET https://graph.microsoft.com/v1.0/me/messages/{id}/$value` (returns raw RFC 2822 bytes directly — no base64 decode needed)        |
| Copy-paste code auth flow                   | Local HTTP server on `:8080` receives OAuth2 callback automatically (browser opens, token saved silently)                             |
| `gmail_token.json`                          | `outlook_token.json` — same JSON structure, same auto-refresh logic                                                                   |


### `cmd/outlook_sync/main.go` — CLI flags

```
-client-id   string   Azure App client ID (or OUTLOOK_CLIENT_ID env var)
-token       string   Token file path (default: outlook_token.json)
-query       string   OData $filter string (default: hasAttachments eq true)
-max         int      Max emails to fetch (default: 10)
-provider    string   claude or openai (default: claude)
-dry-run     bool     Skip AI calls, just parse
```

### Query syntax difference


| Gmail flag                                   | Outlook equivalent                                                 |
| -------------------------------------------- | ------------------------------------------------------------------ |
| `-query "has:attachment subject:remittance"` | `-query "hasAttachments eq true"` + `$search="subject:remittance"` |


Attachment MIME-type filtering (PDF only) is done in code after fetching, same as Gmail.

### Result filenames

Gmail saves as `gmail_<id>_<timestamp>.json`. Outlook will save as `outlook_<id>_<timestamp>.json`. Same `results/` directory.

---

## Part 4 — Running the Outlook sync

```bash
# First run — opens browser for OAuth2 consent, saves token
go run ./cmd/outlook_sync -client-id <your-client-id>

# Subsequent runs — token auto-reloaded, no browser
go run ./cmd/outlook_sync

# Custom query
go run ./cmd/outlook_sync -query "hasAttachments eq true" -max 20

# Dry-run
go run ./cmd/outlook_sync -dry-run

# Use OpenAI
go run ./cmd/outlook_sync -provider openai
```

---

## Implementation Order

1. Create `internal/outlookclient/client.go`
2. Create `cmd/outlook_sync/main.go`
3. Update `.env.example`
4. Update `CLAUDE.md`

