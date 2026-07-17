package mailer

import (
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"strings"
	"testing"
)

// The plain-text part MUST be quoted-printable encoded, and this is why.
//
// An '=' is the quoted-printable escape character. Passing text through unencoded while
// declaring Content-Transfer-Encoding: quoted-printable makes the receiving client read
// every '=' in the body as the start of an escape sequence — so a URL with a query string
// arrives corrupted, or truncated, or silently dropped.
//
// The HTML part looks fine, because most people read the HTML. So the bug presents as "SOME
// people's links don't work", which is a miserable thing to debug. It cost the platform an
// afternoon. This test is the fence.
func TestTextPartSurvivesEqualsSigns(t *testing.T) {
	link := "https://ops.example.com/stacks/abc?run=42&tab=logs"

	body := buildMIME(Message{
		From:    "daffa@example.com",
		To:      "ops@example.com",
		Subject: "Deploy failed",
		Text:    "The deploy failed.\n\n" + link + "\n",
		HTML:    `<p>The deploy failed. <a href="` + link + `">Open</a></p>`,
	})

	text, html := decodeParts(t, body)

	if !strings.Contains(text, link) {
		t.Errorf("the link did not survive the text part.\n got: %q\nwant it to contain: %q\n\n"+
			"If this fails, the text part is not quoted-printable encoded, and every '=' in a "+
			"URL is being eaten by the receiving client.", text, link)
	}
	if !strings.Contains(html, link) {
		t.Errorf("the link did not survive the HTML part.\n got: %q", html)
	}
}

// A UTF-8 body must survive too — an em-dash in a subject or a curly quote in a summary is
// not exotic, it is what the templates actually contain.
func TestUTF8Survives(t *testing.T) {
	text := "The deploy of “billing” failed — exit code 1."

	body := buildMIME(Message{From: "a@b", To: "c@d", Subject: "x", Text: text})
	got, _ := decodeParts(t, body)

	if !strings.Contains(got, text) {
		t.Errorf("UTF-8 did not survive:\n got: %q\nwant: %q", got, text)
	}
}

func TestFromHeaderQuotesTheDisplayName(t *testing.T) {
	if got := addressHeader("Daffa", "d@example.com"); got != `"Daffa" <d@example.com>` {
		t.Errorf("addressHeader = %q", got)
	}
	// No name ⇒ the bare address, not `"" <addr>`.
	if got := addressHeader("", "d@example.com"); got != "d@example.com" {
		t.Errorf("addressHeader with no name = %q; want the bare address", got)
	}
	// A comma in a display name is exactly what the quoting is for: unquoted, it would be
	// read as an address separator and the message would go somewhere unexpected.
	if got := addressHeader("Daffa, Ops", "d@example.com"); !strings.HasPrefix(got, `"Daffa, Ops"`) {
		t.Errorf("a display name with a comma was not quoted: %q", got)
	}
}

// decodeParts reads the message the way a mail client would: parse the multipart, then
// decode each part's transfer encoding.
func decodeParts(t *testing.T, raw []byte) (text, html string) {
	t.Helper()

	headers, rest, ok := strings.Cut(string(raw), "\r\n\r\n")
	if !ok {
		t.Fatal("no header/body split")
	}

	var ctype string
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "content-type:") {
			ctype = strings.TrimSpace(line[len("content-type:"):])
		}
	}

	_, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		t.Fatalf("parsing Content-Type %q: %v", ctype, err)
	}

	mr := multipart.NewReader(strings.NewReader(rest), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading part: %v", err)
		}

		var body []byte
		if p.Header.Get("Content-Transfer-Encoding") == "quoted-printable" {
			body, err = io.ReadAll(quotedprintable.NewReader(p))
		} else {
			body, err = io.ReadAll(p)
		}
		if err != nil {
			t.Fatalf("decoding part: %v", err)
		}

		switch {
		case strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain"):
			text = string(body)
		case strings.HasPrefix(p.Header.Get("Content-Type"), "text/html"):
			html = string(body)
		}
	}
	return text, html
}
