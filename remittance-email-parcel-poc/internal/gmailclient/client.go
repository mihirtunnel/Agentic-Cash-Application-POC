// Package gmailclient handles Gmail OAuth2 authentication and raw message fetching.
//
// First run: opens a browser auth URL, prompts you to paste the code, then
// saves a token file for all future runs (auto-refreshed when expired).
package gmailclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// RawMessage holds the RFC 2822 bytes and key headers of a Gmail message.
type RawMessage struct {
	ID      string
	Subject string
	Raw     []byte
}

// GetService creates an authenticated Gmail API service.
//
//   - credentialsFile — path to the OAuth2 credentials JSON downloaded from
//     Google Cloud Console (the "installed" / desktop app type).
//   - tokenFile — where to persist the access/refresh token between runs.
//     Created automatically on first auth; subsequent runs reload it silently.
func GetService(credentialsFile, tokenFile string) (*gmail.Service, error) {
	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading credentials file %q: %w\n"+
			"  Tip: pass -credentials with the correct path, or set GMAIL_CREDENTIALS_FILE in .env", credentialsFile, err)
	}

	config, err := google.ConfigFromJSON(b, gmail.GmailReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials JSON: %w", err)
	}

	httpClient, err := getHTTPClient(config, tokenFile)
	if err != nil {
		return nil, err
	}

	svc, err := gmail.NewService(context.Background(), option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("creating Gmail service: %w", err)
	}
	return svc, nil
}

// FetchRawMessages queries Gmail and returns the full RFC 2822 bytes for each
// matching message (suitable for passing directly to emailparser.ParseEML).
func FetchRawMessages(svc *gmail.Service, query string, maxResults int64) ([]RawMessage, error) {
	resp, err := svc.Users.Messages.List("me").Q(query).MaxResults(maxResults).Do()
	if err != nil {
		return nil, fmt.Errorf("listing Gmail messages: %w", err)
	}
	if len(resp.Messages) == 0 {
		return nil, nil
	}

	var out []RawMessage
	for _, m := range resp.Messages {
		msg, err := svc.Users.Messages.Get("me", m.Id).Format("raw").Do()
		if err != nil {
			return nil, fmt.Errorf("fetching message %s: %w", m.Id, err)
		}

		// Gmail uses URL-safe base64; try both variants.
		raw, err := base64.URLEncoding.DecodeString(msg.Raw)
		if err != nil {
			raw, err = base64.StdEncoding.DecodeString(msg.Raw)
			if err != nil {
				return nil, fmt.Errorf("decoding message %s: %w", m.Id, err)
			}
		}

		// msg.Payload is nil when Format("raw") is used — parse subject from
		// the decoded RFC 2822 bytes instead.
		subject := subjectFromRaw(raw)
		out = append(out, RawMessage{
			ID:      m.Id,
			Subject: subject,
			Raw:     raw,
		})
	}
	return out, nil
}

// ── OAuth2 helpers ────────────────────────────────────────────────────────────

func getHTTPClient(config *oauth2.Config, tokenFile string) (*http.Client, error) {
	tok, err := readToken(tokenFile)
	if err != nil {
		// No valid token on disk — run the interactive consent flow.
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
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	fmt.Println("── Gmail authorisation required ─────────────────────────────────────────")
	fmt.Println("Open this URL in your browser:")
	fmt.Println()
	fmt.Println(authURL)
	fmt.Println()
	fmt.Print("After granting access, paste the authorisation code here and press Enter: ")

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, fmt.Errorf("reading authorisation code: %w", err)
	}

	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("exchanging authorisation code: %w\n"+
			"  Make sure you copied the full code from the browser redirect URL", err)
	}
	fmt.Println()
	return tok, nil
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
// Used because msg.Payload is nil when the Gmail API returns Format("raw").
func subjectFromRaw(raw []byte) string {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return ""
	}
	return msg.Header.Get("Subject")
}
