// generate_sample creates data/sample.pdf and data/sample.eml for testing.
// Run once: go run cmd/generate_sample/main.go
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	os.MkdirAll("data", 0755)

	pdf := buildPDF()
	if err := os.WriteFile("data/sample.pdf", pdf, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write sample.pdf: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Written: data/sample.pdf (%d bytes)\n", len(pdf))

	eml := buildEML(pdf)
	if err := os.WriteFile("data/sample.eml", []byte(eml), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write sample.eml: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Written: data/sample.eml (%d bytes)\n", len(eml))
	fmt.Println("Done — run the main program with: go run main.go")
}

// buildPDF constructs a minimal but fully valid PDF/1.4 document that contains
// remittance text readable by any standard PDF text extractor.
func buildPDF() []byte {
	// Content stream — the visible text operators.
	// Each line is a separate Tj call so spacing is reliable.
	stream := buildStream()
	streamLen := len(stream)

	var buf bytes.Buffer
	offsets := make([]int, 7) // objects 0–6 (0 is the null entry)

	buf.WriteString("%PDF-1.4\n")

	// Object 1 — Catalog
	offsets[1] = buf.Len()
	buf.WriteString("1 0 obj\n<</Type /Catalog /Pages 2 0 R>>\nendobj\n")

	// Object 2 — Pages
	offsets[2] = buf.Len()
	buf.WriteString("2 0 obj\n<</Type /Pages /Kids [3 0 R] /Count 1>>\nendobj\n")

	// Object 3 — Page
	offsets[3] = buf.Len()
	buf.WriteString("3 0 obj\n<</Type /Page /Parent 2 0 R /MediaBox [0 0 612 792]\n" +
		"  /Resources <</Font <</F1 4 0 R /F2 5 0 R>>>> /Contents 6 0 R>>\nendobj\n")

	// Object 4 — Helvetica-Bold (for headings)
	offsets[4] = buf.Len()
	buf.WriteString("4 0 obj\n<</Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold>>\nendobj\n")

	// Object 5 — Helvetica (for body)
	offsets[5] = buf.Len()
	buf.WriteString("5 0 obj\n<</Type /Font /Subtype /Type1 /BaseFont /Helvetica>>\nendobj\n")

	// Object 6 — Content stream
	offsets[6] = buf.Len()
	fmt.Fprintf(&buf, "6 0 obj\n<</Length %d>>\nstream\n%sendstream\nendobj\n",
		streamLen, stream)

	// Cross-reference table
	xrefOffset := buf.Len()
	buf.WriteString("xref\n")
	fmt.Fprintf(&buf, "0 7\n")
	buf.WriteString("0000000000 65535 f\r\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n\r\n", offsets[i])
	}

	// Trailer
	buf.WriteString("trailer\n<</Size 7 /Root 1 0 R>>\n")
	fmt.Fprintf(&buf, "startxref\n%d\n%%%%EOF\n", xrefOffset)

	return buf.Bytes()
}

func buildStream() string {
	var sb strings.Builder

	line := func(text string) {
		// Escape parentheses, backslashes for PDF string literals.
		text = strings.ReplaceAll(text, `\`, `\\`)
		text = strings.ReplaceAll(text, "(", `\(`)
		text = strings.ReplaceAll(text, ")", `\)`)
		fmt.Fprintf(&sb, "(%s) Tj\n", text)
		sb.WriteString("0 -18 Td\n")
	}

	heading := func(text string) {
		sb.WriteString("/F1 13 Tf\n")
		text = strings.ReplaceAll(text, `\`, `\\`)
		text = strings.ReplaceAll(text, "(", `\(`)
		text = strings.ReplaceAll(text, ")", `\)`)
		fmt.Fprintf(&sb, "(%s) Tj\n", text)
		sb.WriteString("0 -22 Td\n")
		sb.WriteString("/F2 11 Tf\n")
	}

	sb.WriteString("BT\n")
	sb.WriteString("/F1 16 Tf\n")
	sb.WriteString("50 730 Td\n")

	heading("REMITTANCE ADVICE")
	line("Global Supplies Inc.")
	line("123 Commerce Blvd, Chicago, IL 60601")
	line("remittance@globalsupplies.com")
	line("")
	line("Date of Payment: April 15, 2026")
	line("Bank Wire Reference: WIRE-20260415-8842")
	line("Payment Method: Domestic Wire Transfer")
	line("Paying To: Tauber Oil Corporation")
	line("")
	heading("Invoice Summary")
	line("INV-2026-001   Office Supplies Q1 2026          $5,250.00")
	line("INV-2026-002   Warehouse Safety Equipment        $3,750.00")
	line("INV-2026-003   IT Infrastructure Renewal         $6,000.00")
	line("")
	line("--------------------------------------------------")
	line("TOTAL PAYMENT AMOUNT: $15,000.00  USD")
	line("--------------------------------------------------")
	line("")
	heading("Notes")
	line("Please apply payment to the above invoices.")
	line("Contact ap@globalsupplies.com for any discrepancies.")

	sb.WriteString("ET\n")
	return sb.String()
}

func buildEML(pdfData []byte) string {
	boundary := "==REMITTANCE_BOUNDARY_20260415=="
	b64PDF := base64.StdEncoding.EncodeToString(pdfData)

	// Wrap base64 at 76 chars per line (MIME standard).
	b64Lines := wrapBase64(b64PDF, 76)

	date := time.Date(2026, 4, 15, 14, 30, 0, 0, time.UTC).Format("Mon, 02 Jan 2006 15:04:05 -0700")

	var sb strings.Builder
	fmt.Fprintf(&sb, "From: AP Department <remittance@globalsupplies.com>\r\n")
	fmt.Fprintf(&sb, "To: accounts.receivable@tauberoil.com\r\n")
	fmt.Fprintf(&sb, "Subject: Remittance Advice - April 2026 Payment - Wire WIRE-20260415-8842\r\n")
	fmt.Fprintf(&sb, "Date: %s\r\n", date)
	fmt.Fprintf(&sb, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&sb, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n", boundary)
	fmt.Fprintf(&sb, "\r\n")

	// Plain-text body part
	fmt.Fprintf(&sb, "--%s\r\n", boundary)
	fmt.Fprintf(&sb, "Content-Type: text/plain; charset=UTF-8\r\n")
	fmt.Fprintf(&sb, "Content-Transfer-Encoding: 7bit\r\n")
	fmt.Fprintf(&sb, "\r\n")
	sb.WriteString("Dear Accounts Receivable Team,\r\n")
	sb.WriteString("\r\n")
	sb.WriteString("Please find attached our remittance advice for the wire transfer we sent on April 15, 2026.\r\n")
	sb.WriteString("\r\n")
	sb.WriteString("Payment Summary:\r\n")
	sb.WriteString("  Company:          Global Supplies Inc.\r\n")
	sb.WriteString("  Payment Date:     April 15, 2026\r\n")
	sb.WriteString("  Total Amount:     $15,000.00 USD\r\n")
	sb.WriteString("  Wire Reference:   WIRE-20260415-8842\r\n")
	sb.WriteString("\r\n")
	sb.WriteString("This payment covers the following invoices:\r\n")
	sb.WriteString("  - INV-2026-001: $5,250.00 (Office Supplies Q1 2026)\r\n")
	sb.WriteString("  - INV-2026-002: $3,750.00 (Warehouse Safety Equipment)\r\n")
	sb.WriteString("  - INV-2026-003: $6,000.00 (IT Infrastructure Renewal)\r\n")
	sb.WriteString("\r\n")
	sb.WriteString("Please confirm receipt and application of these funds.\r\n")
	sb.WriteString("\r\n")
	sb.WriteString("Best regards,\r\n")
	sb.WriteString("Sarah Mitchell\r\n")
	sb.WriteString("Accounts Payable Manager\r\n")
	sb.WriteString("Global Supplies Inc.\r\n")
	sb.WriteString("Tel: +1 (312) 555-0198\r\n")
	fmt.Fprintf(&sb, "\r\n")

	// PDF attachment part
	fmt.Fprintf(&sb, "--%s\r\n", boundary)
	fmt.Fprintf(&sb, "Content-Type: application/pdf; name=\"remittance_april2026.pdf\"\r\n")
	fmt.Fprintf(&sb, "Content-Disposition: attachment; filename=\"remittance_april2026.pdf\"\r\n")
	fmt.Fprintf(&sb, "Content-Transfer-Encoding: base64\r\n")
	fmt.Fprintf(&sb, "\r\n")
	sb.WriteString(b64Lines)
	fmt.Fprintf(&sb, "\r\n")

	// Closing boundary
	fmt.Fprintf(&sb, "--%s--\r\n", boundary)

	return sb.String()
}

func wrapBase64(s string, width int) string {
	var sb strings.Builder
	for len(s) > width {
		sb.WriteString(s[:width])
		sb.WriteString("\r\n")
		s = s[width:]
	}
	if len(s) > 0 {
		sb.WriteString(s)
		sb.WriteString("\r\n")
	}
	return sb.String()
}
