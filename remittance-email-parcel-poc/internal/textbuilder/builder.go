package textbuilder

import (
	"fmt"
	"strings"

	"remittance-poc/internal/models"
)

// MaxContextChars is the soft cap on the total context string sent to the LLM.
// Set high enough to hold large remittance PDFs (e.g. 71+ invoices ≈ 6–10 KB of text).
// Raise further only if the model's context window allows it.
const MaxContextChars = 30_000

// Build assembles a compact, LLM-ready context string from a parsed email
// and the extracted text of each PDF attachment.
//
// Layout:
//
//	=== EMAIL METADATA ===
//	From / To / Subject / Date
//
//	=== EMAIL BODY ===
//	<plain text>
//
//	=== PDF ATTACHMENT: filename.pdf ===
//	<extracted text>
//	...
func Build(parsed *models.ParsedEmail, pdfTexts map[string]string) string {
	var sb strings.Builder

	// --- Email metadata ---
	sb.WriteString("=== EMAIL METADATA ===\n")
	if parsed.Metadata.From != "" {
		fmt.Fprintf(&sb, "From:    %s\n", parsed.Metadata.From)
	}
	if parsed.Metadata.To != "" {
		fmt.Fprintf(&sb, "To:      %s\n", parsed.Metadata.To)
	}
	if parsed.Metadata.Subject != "" {
		fmt.Fprintf(&sb, "Subject: %s\n", parsed.Metadata.Subject)
	}
	if !parsed.Metadata.Date.IsZero() {
		fmt.Fprintf(&sb, "Date:    %s\n", parsed.Metadata.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	}

	// --- Email body ---
	if parsed.BodyText != "" {
		sb.WriteString("\n=== EMAIL BODY ===\n")
		sb.WriteString(parsed.BodyText)
		sb.WriteString("\n")
	}

	// --- PDF attachments ---
	for _, att := range parsed.Attachments {
		text, ok := pdfTexts[att.Filename]
		if !ok || text == "" {
			fmt.Fprintf(&sb, "\n=== PDF ATTACHMENT: %s ===\n", att.Filename)
			sb.WriteString("[No extractable text — may be a scanned/image-only PDF]\n")
			continue
		}
		fmt.Fprintf(&sb, "\n=== PDF ATTACHMENT: %s ===\n", att.Filename)
		sb.WriteString(text)
		sb.WriteString("\n")
	}

	context := sb.String()

	// Truncate if over the character cap, adding a clear notice.
	if len(context) > MaxContextChars {
		context = context[:MaxContextChars] +
			"\n\n[... content truncated to stay within token budget ...]"
	}

	return context
}
