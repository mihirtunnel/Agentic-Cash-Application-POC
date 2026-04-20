package gmailclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"remittance-poc/internal/models"
)

// remittanceKeywords are matched against subject and body (case-insensitive)
// to decide whether an email is likely a remittance / payment notification.
var remittanceKeywords = []string{
	"remittance", "payment advice", "payment notification",
	"payment initiated", "payment detail", "remit advice",
	"invoice payment", "ach payment", "wire transfer", "eft payment",
	"bank transfer", "wire confirmation", "check payment",
	"remit", "invoice", "payment enclosed",
}

var (
	htmlTagRe         = regexp.MustCompile(`<[^>]+>`)
	multipleNewlinesRe = regexp.MustCompile(`\n{3,}`)
)

// Client wraps the Gmail API service.
type Client struct {
	svc *gmailapi.Service
}

// New creates a Gmail API client using OAuth2.
//   - credentialsFile: path to the Google Cloud OAuth2 client credentials JSON
//     (downloaded from Cloud Console → APIs & Services → Credentials).
//   - tokenFile: path where the obtained user token is saved/loaded.
func New(credentialsFile, tokenFile string) (*Client, error) {
	credBytes, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading credentials file %q: %w", credentialsFile, err)
	}

	cfg, err := google.ConfigFromJSON(credBytes, gmailapi.GmailReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials JSON: %w", err)
	}

	tok, err := loadToken(tokenFile)
	if err != nil {
		// No saved token — run the interactive browser OAuth flow.
		tok, err = runOAuthFlow(cfg)
		if err != nil {
			return nil, fmt.Errorf("OAuth2 flow failed: %w", err)
		}
		if saveErr := saveToken(tokenFile, tok); saveErr != nil {
			fmt.Printf("[WARN] Could not save token to %q: %v\n", tokenFile, saveErr)
		} else {
			fmt.Printf("OAuth token saved to %q\n", tokenFile)
		}
	}

	httpClient := cfg.Client(context.Background(), tok)
	svc, err := gmailapi.NewService(context.Background(), option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("creating Gmail service: %w", err)
	}

	return &Client{svc: svc}, nil
}

// FetchEmailsSince returns all Gmail messages received after `since`.
// It returns the parsed email structs along with their corresponding Gmail message IDs.
func (c *Client) FetchEmailsSince(since time.Time) ([]*models.ParsedEmail, []string, error) {
	// Gmail search operator: after:<unix-epoch-seconds>
	query := fmt.Sprintf("after:%d", since.Unix())

	var (
		emails     []*models.ParsedEmail
		messageIDs []string
		pageToken  string
	)

	for {
		req := c.svc.Users.Messages.List("me").Q(query).MaxResults(100)
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}
		listRes, err := req.Do()
		if err != nil {
			return nil, nil, fmt.Errorf("listing Gmail messages: %w", err)
		}

		for _, stub := range listRes.Messages {
			parsed, err := c.GetMessage(stub.Id)
			if err != nil {
				fmt.Printf("[WARN] Skipping message %s: %v\n", stub.Id, err)
				continue
			}
			emails = append(emails, parsed)
			messageIDs = append(messageIDs, stub.Id)
		}

		if listRes.NextPageToken == "" {
			break
		}
		pageToken = listRes.NextPageToken
	}

	return emails, messageIDs, nil
}

// GetMessage fetches a single Gmail message by ID and converts it to a ParsedEmail.
func (c *Client) GetMessage(messageID string) (*models.ParsedEmail, error) {
	msg, err := c.svc.Users.Messages.Get("me", messageID).Format("full").Do()
	if err != nil {
		return nil, fmt.Errorf("fetching message %s: %w", messageID, err)
	}

	parsed := &models.ParsedEmail{}

	// Extract RFC 2822 headers from the root payload.
	for _, h := range msg.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			parsed.Metadata.From = h.Value
		case "to":
			parsed.Metadata.To = h.Value
		case "subject":
			parsed.Metadata.Subject = h.Value
		case "date":
			if t, err := mail.ParseDate(h.Value); err == nil {
				parsed.Metadata.Date = t
			} else {
				parsed.Metadata.Date = time.Now()
			}
		}
	}

	// Recursively walk all MIME parts.
	c.walkParts(msg.Payload, parsed, messageID)

	return parsed, nil
}

// walkParts recursively processes a Gmail message part, populating parsed.
func (c *Client) walkParts(part *gmailapi.MessagePart, parsed *models.ParsedEmail, msgID string) {
	mimeType := strings.ToLower(part.MimeType)

	if strings.HasPrefix(mimeType, "multipart/") {
		for _, sub := range part.Parts {
			c.walkParts(sub, parsed, msgID)
		}
		return
	}

	switch {
	case strings.HasPrefix(mimeType, "text/plain"):
		if part.Body != nil && part.Body.Data != "" && parsed.BodyText == "" {
			text, err := decodeURLBase64(part.Body.Data)
			if err == nil {
				parsed.BodyText = normalizeText(string(text))
			}
		}

	case strings.HasPrefix(mimeType, "text/html"):
		if parsed.BodyText == "" && part.Body != nil && part.Body.Data != "" {
			text, err := decodeURLBase64(part.Body.Data)
			if err == nil {
				parsed.BodyText = stripHTML(string(text))
			}
		}

	case isPDF(mimeType, part.Filename):
		pdfBytes, err := c.resolvePDFBytes(part, msgID)
		if err != nil {
			fmt.Printf("[WARN] Could not fetch PDF %q: %v\n", part.Filename, err)
			return
		}
		if len(pdfBytes) == 0 {
			return
		}
		filename := part.Filename
		if filename == "" {
			filename = "attachment.pdf"
		}
		parsed.Attachments = append(parsed.Attachments, models.PDFAttachment{
			Filename: filename,
			Data:     pdfBytes,
		})
	}
}

// resolvePDFBytes returns the raw bytes of a PDF part, fetching from the
// attachment endpoint if the data is not inlined.
func (c *Client) resolvePDFBytes(part *gmailapi.MessagePart, msgID string) ([]byte, error) {
	if part.Body == nil {
		return nil, nil
	}
	if part.Body.Data != "" {
		return decodeURLBase64(part.Body.Data)
	}
	if part.Body.AttachmentId != "" {
		att, err := c.svc.Users.Messages.Attachments.Get("me", msgID, part.Body.AttachmentId).Do()
		if err != nil {
			return nil, err
		}
		return decodeURLBase64(att.Data)
	}
	return nil, nil
}

// IsRemittanceEmail returns true when the email subject or body contains
// at least one remittance-related keyword.
func IsRemittanceEmail(parsed *models.ParsedEmail) bool {
	subject := strings.ToLower(parsed.Metadata.Subject)
	body := strings.ToLower(parsed.BodyText)

	for _, kw := range remittanceKeywords {
		if strings.Contains(subject, kw) || strings.Contains(body, kw) {
			return true
		}
	}
	return false
}

// ── helpers ───────────────────────────────────────────────────────────────────

// decodeURLBase64 decodes Gmail's URL-safe, no-padding base64 data.
func decodeURLBase64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func isPDF(mimeType, filename string) bool {
	if strings.Contains(mimeType, "pdf") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(filename), ".pdf")
}

func stripHTML(html string) string {
	text := htmlTagRe.ReplaceAllString(html, " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&quot;", `"`)
	text = multipleNewlinesRe.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = multipleNewlinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ── OAuth2 token management ───────────────────────────────────────────────────

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
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

func saveToken(path string, tok *oauth2.Token) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

// runOAuthFlow starts a local HTTP server on a random available port,
// opens the browser for user consent, and returns the obtained token.
func runOAuthFlow(cfg *oauth2.Config) (*oauth2.Token, error) {
	// Pick a free port dynamically.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting local listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cfg.RedirectURL = fmt.Sprintf("http://localhost:%d/oauth/callback", port)

	state := fmt.Sprintf("state-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)

	fmt.Println("\n── Gmail Authorization Required ─────────────────────")
	fmt.Println("Opening your browser for Gmail access consent...")
	fmt.Printf("If it doesn't open automatically, visit:\n\n  %s\n\n", authURL)
	openBrowser(authURL)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	server := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}

	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("OAuth state mismatch")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no authorization code received")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body>
<h2 style="font-family:sans-serif;color:#2e7d32">Authorization successful!</h2>
<p style="font-family:sans-serif">You can close this tab and return to the terminal.</p>
</body></html>`)
		codeCh <- code
	})

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Printf("Waiting for browser authorization (listening on localhost:%d)…\n", port)

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for OAuth authorization (5 min)")
	}

	// Shutdown background server gracefully.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server.Shutdown(ctx) //nolint:errcheck

	tok, err := cfg.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("exchanging authorization code: %w", err)
	}
	return tok, nil
}

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	case "windows":
		cmd = "rundll32"
		exec.Command(cmd, "url.dll,FileProtocolHandler", url).Start() //nolint:errcheck
		return
	default:
		return
	}
	exec.Command(cmd, url).Start() //nolint:errcheck
}
