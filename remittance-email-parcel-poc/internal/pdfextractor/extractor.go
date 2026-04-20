package pdfextractor

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"
)

// ExtractText extracts all readable plain text from PDF bytes.
// Returns an empty string (with no error) for image-only/scanned PDFs,
// which produce no content streams.
func ExtractText(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty PDF data")
	}

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("opening PDF: %w", err)
	}

	reader, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("extracting PDF text: %w", err)
	}

	raw, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("reading PDF text: %w", err)
	}

	text := strings.TrimSpace(string(raw))
	return text, nil
}
