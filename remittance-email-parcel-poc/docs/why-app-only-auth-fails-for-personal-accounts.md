# Why App-Only Auth (Client Credentials) Doesn't Work for Personal Outlook Accounts

## The Short Answer

Microsoft explicitly blocks the OAuth2 Client Credentials flow for personal Microsoft accounts
(`@outlook.com`, `@hotmail.com`, `@live.com`). It is a hard platform restriction — no amount of
Azure Portal configuration or permission changes will make it work for a personal mailbox.

---

## What We Tried

We registered an Azure App (`remittance-poc`, client ID `2d002725-...`) and configured:

- `Mail.Read` — **Application** permission (not Delegated)
- Admin consent granted for the Default Directory
- Tenant ID, Client Secret, and Mailbox set in `.env`

The app successfully obtained a Bearer token from Microsoft's token endpoint. But every call to
Microsoft Graph API to read the mailbox returned:

```
GET /users/viatunnel.test@outlook.com/messages  →  401 Unauthorized (empty body)
GET /users/viatunnel.test@outlook.com           →  403 Insufficient privileges
GET /users                                       →  403 Insufficient privileges
```

---

## Why This Happens

Microsoft runs two completely separate identity systems:

| System | Account types | Infrastructure |
|--------|--------------|----------------|
| **Azure Active Directory (Azure AD)** | Work/school accounts — `user@company.com`, Microsoft 365 Business/Enterprise | Azure AD tenant servers |
| **Microsoft Account (MSA)** | Personal accounts — `@outlook.com`, `@hotmail.com`, `@live.com` | Microsoft consumer identity servers |

The OAuth2 **Client Credentials Grant** (app-only auth) is a feature of **Azure AD only**. When an
app authenticates using a client ID + client secret, it gets a token that is scoped to the Azure AD
tenant. That token has no authority over personal Microsoft Account mailboxes, which live on
entirely different servers.

Even though `viatunnel.test@outlook.com` was used to create the Azure App Registration (so it
appears as the "owner"), the mailbox itself is hosted on Microsoft's consumer infrastructure and is
invisible to Azure AD application tokens.

---

## The Microsoft Documentation Stance

Microsoft's own documentation states:

> "Client credentials flow is not supported for personal Microsoft accounts."

Source: [Microsoft identity platform and the OAuth 2.0 client credentials flow](https://learn.microsoft.com/en-us/azure/active-directory/develop/v2-oauth2-client-creds-grant-flow)

The `Mail.Read` **Application** permission description in Azure Portal says *"Read mail in all
mailboxes"* — but "all mailboxes" means all mailboxes **within the Azure AD tenant**, not personal
consumer accounts.

---

## What Account Type Is Needed for App-Only Auth

To use the Client Credentials flow with Microsoft Graph Mail API, the target mailbox must be a
**Microsoft 365 work or school account** — a user that is a full member of an Azure AD tenant with
an Exchange Online mailbox. Examples:

- `remittance@yourcompany.com` — a Microsoft 365 Business account
- `finance@yourorg.onmicrosoft.com` — an Azure AD tenant account

These accounts have mailboxes hosted in Exchange Online (Azure AD-managed), which app-only tokens
can access.

---

## What Works for Personal Accounts Instead

For personal `@outlook.com` accounts, use **delegated (interactive) OAuth2**:

1. The app opens a browser once for the user to sign in and grant consent.
2. Microsoft issues an access token **plus a refresh token**.
3. The refresh token is saved to `outlook_token.json`.
4. All future runs load and auto-refresh the token — no browser, fully automatic.

This is the `-auth interactive` mode in this project:

```bash
# First run — opens browser once
go run ./cmd/outlook_sync -auth interactive

# All subsequent runs — no browser, token auto-refreshed
go run ./cmd/outlook_sync -auth interactive
```

The only difference from app-only auth is the one-time manual login step. After that, day-to-day
operation is identical: no human interaction required.

---

## Summary

| | App-only (`-auth app`) | Interactive (`-auth interactive`) |
|---|---|---|
| **Account type** | Microsoft 365 work/school only | Personal OR work accounts |
| **First-run setup** | No browser needed | Browser login once |
| **Subsequent runs** | Fully automatic | Fully automatic (token auto-refreshed) |
| **Works with `@outlook.com`** | No — hard Microsoft block | Yes |
| **Works with `@company.com` (M365)** | Yes | Yes |
| **Requires client secret** | Yes | No |
| **Token stored on disk** | No (fetched fresh each run) | Yes (`outlook_token.json`) |
