package emailparser

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"remittance-poc/internal/models"
)

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)
var multipleNewlinesRe = regexp.MustCompile(`\n{3,}`)

// ParseEML parses a raw .eml file (RFC 2822) into a ParsedEmail.
// It extracts the plain-text body and any PDF attachments.
func ParseEML(raw []byte) (*models.ParsedEmail, error) {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("reading email: %w", err)
	}

	meta := parseMetadata(msg.Header)

	contentType := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Fall back to reading as plain text if no Content-Type.
		body, _ := io.ReadAll(msg.Body)
		return &models.ParsedEmail{
			Metadata: meta,
			BodyText: strings.TrimSpace(string(body)),
		}, nil
	}

	parsed := &models.ParsedEmail{Metadata: meta}

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(msg.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading MIME part: %w", err)
			}

			partType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			encoding := strings.ToLower(part.Header.Get("Content-Transfer-Encoding"))

			data, err := io.ReadAll(part)
			if err != nil {
				return nil, fmt.Errorf("reading part data: %w", err)
			}

			switch {
			case strings.HasPrefix(partType, "text/plain"):
				charset := partParams["charset"]
				text := decodeTransfer(data, encoding)
				parsed.BodyText = normalizeText(string(text), charset)

			case strings.HasPrefix(partType, "text/html") && parsed.BodyText == "":
				text := decodeTransfer(data, encoding)
				parsed.BodyText = stripHTML(string(text))

			case isPDFPart(partType, part.FileName()):
				pdfBytes := decodeTransfer(data, encoding)
				filename := part.FileName()
				if filename == "" {
					filename = "attachment.pdf"
				}
				parsed.Attachments = append(parsed.Attachments, models.PDFAttachment{
					Filename: filename,
					Data:     pdfBytes,
				})
			}
		}
	} else {
		// Single-part email.
		data, _ := io.ReadAll(msg.Body)
		encoding := strings.ToLower(msg.Header.Get("Content-Transfer-Encoding"))
		text := decodeTransfer(data, encoding)
		if strings.HasPrefix(mediaType, "text/html") {
			parsed.BodyText = stripHTML(string(text))
		} else {
			parsed.BodyText = normalizeText(string(text), params["charset"])
		}
	}

	return parsed, nil
}

func parseMetadata(h mail.Header) models.EmailMetadata {
	meta := models.EmailMetadata{
		From:    h.Get("From"),
		To:      h.Get("To"),
		Subject: h.Get("Subject"),
	}
	if dateStr := h.Get("Date"); dateStr != "" {
		if t, err := mail.ParseDate(dateStr); err == nil {
			meta.Date = t
		} else {
			meta.Date = time.Now()
		}
	}
	return meta
}

func isPDFPart(mediaType, filename string) bool {
	if strings.Contains(mediaType, "pdf") {
		return true
	}
	if strings.Contains(mediaType, "octet-stream") && strings.HasSuffix(strings.ToLower(filename), ".pdf") {
		return true
	}
	return false
}

func decodeTransfer(data []byte, encoding string) []byte {
	switch encoding {
	case "base64":
		cleaned := strings.ReplaceAll(string(data), "\r\n", "")
		cleaned = strings.ReplaceAll(cleaned, "\n", "")
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			// Try URL-safe variant.
			decoded, _ = base64.RawStdEncoding.DecodeString(cleaned)
		}
		return decoded
	case "quoted-printable":
		return decodeQuotedPrintable(data)
	default:
		return data
	}
}

func decodeQuotedPrintable(data []byte) []byte {
	// Minimal QP decoder sufficient for plain text content.
	s := strings.ReplaceAll(string(data), "=\r\n", "")
	s = strings.ReplaceAll(s, "=\n", "")
	return []byte(s)
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

func normalizeText(s, _ string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = multipleNewlinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
