// Package outlookclient handles Microsoft Outlook OAuth2 authentication and
// raw message fetching via the Microsoft Graph API.
//
// First run: opens a browser auth URL on http://localhost:8080/callback, then
// saves a token file for all future runs (auto-refreshed when expired).
package outlookclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"
)

const graphBase = "https://graph.microsoft.com/v1.0"

// RawMessage holds the RFC 2822 bytes and key headers of an Outlook message.
type RawMessage struct {
	ID      string
	Subject string
	Raw     []byte
}

// GetClient creates an authenticated HTTP client for Microsoft Graph API.
//
//   - clientID   — Application (client) ID from Azure App Registration.
//   - tokenFile  — where to persist the access/refresh token between runs.
//     Created automatically on first auth; subsequent runs reload it silently.
func GetClient(clientID, tokenFile string) (*http.Client, error) {
	config := &oauth2.Config{
		ClientID:    clientID,
		Endpoint:    microsoft.AzureADEndpoint("consumers"),
		Scopes:      []string{"Mail.Read", "offline_access"},
		RedirectURL: "http://localhost:8080/callback",
	}

	return getHTTPClient(config, tokenFile)
}

// FetchRawMessages queries the Microsoft Graph API and returns the full RFC 2822
// bytes for each matching message (suitable for passing to emailparser.ParseEML).
//
//   - filter     — OData $filter expression, e.g. "hasAttachments eq true"
//   - maxResults — maximum number of messages to return
func FetchRawMessages(httpClient *http.Client, filter string, maxResults int) ([]RawMessage, error) {
	params := url.Values{}
	params.Set("$filter", filter)
	params.Set("$top", fmt.Sprintf("%d", maxResults))
	params.Set("$select", "id,subject")

	listURL := fmt.Sprintf("%s/me/messages?%s", graphBase, params.Encode())

	resp, err := httpClient.Get(listURL)
	if err != nil {
		return nil, fmt.Errorf("listing Outlook messages: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Graph API list error %d: %s", resp.StatusCode, string(body))
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
		raw, err := fetchRawMessage(httpClient, m.ID)
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

// fetchRawMessage fetches a single message as raw RFC 2822 bytes via the $value endpoint.
func fetchRawMessage(httpClient *http.Client, id string) ([]byte, error) {
	rawURL := fmt.Sprintf("%s/me/messages/%s/$value", graphBase, id)

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

// ── OAuth2 helpers ────────────────────────────────────────────────────────────

func getHTTPClient(config *oauth2.Config, tokenFile string) (*http.Client, error) {
	tok, err := readToken(tokenFile)
	if err != nil {
		tok, err = runAuthFlow(config)
		if err != nil {
			return nil, err
		}
		if saveErr := saveToken(tokenFile, tok); saveErr != nil {
			fmt.Fprintf(os.Stderr, "[WARN] Could not save token to %s: %v\n", tokenFile, saveErr)
		} else {
			fmt.Printf("Token saved to %s — future runs will skip the browser step.\n\n", tokenFile)
		}
	}
	return config.Client(context.Background(), tok), nil
}

func runAuthFlow(config *oauth2.Config) (*oauth2.Token, error) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no authorisation code in callback: %s", r.URL.RawQuery)
			fmt.Fprintln(w, "Error: no authorisation code received. You can close this tab.")
			return
		}
		codeCh <- code
		fmt.Fprintln(w, "Authorisation successful! You can close this tab and return to the terminal.")
	})

	listener, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		return nil, fmt.Errorf("starting local auth server on :8080: %w\n"+
			"  Make sure nothing else is running on port 8080", err)
	}

	go func() {
		if serveErr := srv.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	fmt.Println("── Outlook authorisation required ───────────────────────────────────────")
	fmt.Println("Opening browser for Microsoft sign-in…")
	fmt.Println()
	fmt.Println("If the browser does not open automatically, visit this URL:")
	fmt.Println()
	fmt.Println(authURL)
	fmt.Println()

	openBrowser(authURL)
	fmt.Println("Waiting for authorisation callback on http://localhost:8080/callback …")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
	case authErr := <-errCh:
		srv.Shutdown(context.Background())
		return nil, authErr
	case <-ctx.Done():
		srv.Shutdown(context.Background())
		return nil, fmt.Errorf("timed out waiting for authorisation (2-minute limit)")
	}

	srv.Shutdown(context.Background())

	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchanging authorisation code: %w\n"+
			"  Make sure you completed the sign-in before the 2-minute timeout", err)
	}
	fmt.Println()
	return tok, nil
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func readToken(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tok oauth2.Token
	if err := json.NewDecoder(f).Decode(&tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func saveToken(file string, tok *oauth2.Token) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

// subjectFromRaw parses the Subject header from raw RFC 2822 email bytes.
func subjectFromRaw(raw []byte) string {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return ""
	}
	return msg.Header.Get("Subject")
}
