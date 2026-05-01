// App-only (client credentials) authentication for Microsoft Graph API.
//
// Use this when you have a client secret and want non-interactive access to a
// specific mailbox. Requires a Microsoft 365 work/school account — personal
// @outlook.com accounts are not supported by Microsoft for this flow.
package outlookclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2/clientcredentials"
	"golang.org/x/oauth2/microsoft"
)

// GetAppClient returns an HTTP client authenticated via the OAuth2 client
// credentials grant (app-only, no browser/user interaction required).
//
//   - clientID     — Azure App Registration "Application (client) ID"
//   - clientSecret — Azure App client secret value (from Certificates & secrets)
//   - tenantID     — Azure AD "Directory (tenant) ID" — NOT "consumers"
func GetAppClient(clientID, clientSecret, tenantID string) (*http.Client, error) {
	if clientID == "" || clientSecret == "" || tenantID == "" {
		return nil, fmt.Errorf("GetAppClient: clientID, clientSecret, and tenantID are all required")
	}

	endpoint := microsoft.AzureADEndpoint(tenantID)

	cfg := clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     endpoint.TokenURL,
		Scopes:       []string{"https://graph.microsoft.com/.default"},
	}

	// Verify we can fetch a token before returning.
	_, err := cfg.Token(context.Background())
	if err != nil {
		return nil, fmt.Errorf("app-only auth failed: %w\n"+
			"  Check that:\n"+
			"  • clientID / clientSecret / tenantID are correct\n"+
			"  • The app has Mail.Read Application permission (not Delegated)\n"+
			"  • Admin consent has been granted in the Azure Portal")
	}

	return cfg.Client(context.Background()), nil
}

// FetchRawMessagesForMailbox fetches RFC 2822 email bytes from a specific
// mailbox using app-only auth (GET /users/{mailbox}/messages).
//
//   - mailbox    — email address of the target mailbox, e.g. "remittance@company.com"
//   - filter     — OData $filter expression, e.g. "hasAttachments eq true"
//   - maxResults — maximum number of messages to return
func FetchRawMessagesForMailbox(httpClient *http.Client, mailbox, filter string, maxResults int) ([]RawMessage, error) {
	if mailbox == "" {
		return nil, fmt.Errorf("FetchRawMessagesForMailbox: mailbox address is required")
	}

	params := url.Values{}
	params.Set("$filter", filter)
	params.Set("$top", fmt.Sprintf("%d", maxResults))
	params.Set("$select", "id,subject")

	listURL := fmt.Sprintf("%s/users/%s/messages?%s", graphBase, url.PathEscape(mailbox), params.Encode())

	resp, err := httpClient.Get(listURL)
	if err != nil {
		return nil, fmt.Errorf("listing messages for %s: %w", mailbox, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Graph API list error %d for mailbox %s: %s", resp.StatusCode, mailbox, string(body))
	}

	var listResp struct {
		Value []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("decoding message list: %w", err)
	}

	if len(listResp.Value) == 0 {
		return nil, nil
	}

	var out []RawMessage
	for _, m := range listResp.Value {
		raw, err := fetchRawMessageForMailbox(httpClient, mailbox, m.ID)
		if err != nil {
			return nil, err
		}
		subject := m.Subject
		if subject == "" {
			subject = subjectFromRaw(raw)
		}
		out = append(out, RawMessage{
			ID:      m.ID,
			Subject: subject,
			Raw:     raw,
		})
	}
	return out, nil
}

func fetchRawMessageForMailbox(httpClient *http.Client, mailbox, id string) ([]byte, error) {
	rawURL := fmt.Sprintf("%s/users/%s/messages/%s/$value", graphBase, url.PathEscape(mailbox), id)

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for message %s: %w", id, err)
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching message %s: %w", id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Graph API fetch error %d for message %s: %s", resp.StatusCode, id, string(body))
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading message body %s: %w", id, err)
	}
	return raw, nil
}
