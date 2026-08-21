package gmail

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TEST-3.3 — Gmail multipart body extraction.
//
// REQ-3.2 says gmail_get_message returns "headers + plain-text body
// (multipart: prefer text/plain, strip HTML fallback)". Three rules follow, and
// each is a way a real message breaks a naive reader:
//
//   - A multipart/alternative carries the same words twice. Taking the HTML one
//     spends the prompt budget on markup and risks the agent quoting a
//     stylesheet as evidence.
//   - Some senders ship HTML only. Dropping them would make a vendor's
//     announcement look like an empty email.
//   - Bodies arrive base64url-encoded, and parts still declare their original
//     Content-Transfer-Encoding — quoted-printable included, whose soft line
//     breaks split words in half if left undecoded.

// b64url encodes a body the way the Gmail API does.
func b64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// parsePayload decodes a recorded payload fixture, so the JSON field mapping
// (mimeType, filename, body.data, parts) is exercised alongside the walk.
func parsePayload(t *testing.T, data string) *messagePart {
	t.Helper()
	var part messagePart
	if err := json.Unmarshal([]byte(data), &part); err != nil {
		t.Fatalf("decode payload fixture: %v", err)
	}
	return &part
}

func TestExtractBody(t *testing.T) {
	const htmlOnly = `<html><head><style>p{color:red}</style></head><body>` +
		`<p>Nordwind Payments &amp; Co</p><p>Sandbox slips to 2026-07-15</p></body></html>`

	tests := []struct {
		name    string
		payload string
		// want is the exact extracted body; used when wantContains is nil.
		want         string
		wantContains []string
		wantMissing  []string
		wantFromHTML bool
	}{
		{
			name: "single text/plain part at the root",
			payload: `{
			  "partId":"","mimeType":"text/plain","filename":"",
			  "headers":[{"name":"Content-Type","value":"text/plain; charset=UTF-8"}],
			  "body":{"size":42,"data":"` + b64url("Sandbox slips to 2026-07-15.") + `"}
			}`,
			want: "Sandbox slips to 2026-07-15.",
		},
		{
			name: "multipart/alternative prefers text/plain over text/html",
			payload: `{
			  "mimeType":"multipart/alternative","filename":"",
			  "parts":[
			    {"partId":"0","mimeType":"text/plain","filename":"",
			     "body":{"data":"` + b64url("Nordwind: the v3 refunds sandbox slips to 2026-07-15.") + `"}},
			    {"partId":"1","mimeType":"text/html","filename":"",
			     "body":{"data":"` + b64url("<p>Nordwind: the v3 refunds sandbox slips to 2026-07-15.</p>") + `"}}
			  ]
			}`,
			want:        "Nordwind: the v3 refunds sandbox slips to 2026-07-15.",
			wantMissing: []string{"<p>"},
		},
		{
			name: "html-only message falls back to stripped html",
			payload: `{
			  "mimeType":"multipart/alternative","filename":"",
			  "parts":[
			    {"partId":"0","mimeType":"text/html","filename":"",
			     "body":{"data":"` + b64url(htmlOnly) + `"}}
			  ]
			}`,
			want:         "Nordwind Payments & Co\nSandbox slips to 2026-07-15",
			wantFromHTML: true,
		},
		{
			name: "text/html at the root is stripped",
			payload: `{
			  "mimeType":"text/html","filename":"",
			  "body":{"data":"` + b64url("<div>Launch moves to <b>2026-08-14</b></div>") + `"}
			}`,
			want:         "Launch moves to 2026-08-14",
			wantFromHTML: true,
		},
		{
			name: "nested multipart/mixed finds the deep text/plain",
			payload: `{
			  "mimeType":"multipart/mixed","filename":"",
			  "parts":[
			    {"partId":"0","mimeType":"multipart/alternative","filename":"",
			     "parts":[
			       {"partId":"0.0","mimeType":"text/plain","filename":"",
			        "body":{"data":"` + b64url("Revised timeline attached.") + `"}},
			       {"partId":"0.1","mimeType":"text/html","filename":"",
			        "body":{"data":"` + b64url("<p>Revised timeline attached.</p>") + `"}}
			     ]},
			    {"partId":"1","mimeType":"application/pdf","filename":"timeline.pdf",
			     "body":{"size":90210,"attachmentId":"ANGjdJ_attachment_id"}}
			  ]
			}`,
			want: "Revised timeline attached.",
		},
		{
			name: "a text/plain attachment is not the message body",
			payload: `{
			  "mimeType":"multipart/mixed","filename":"",
			  "parts":[
			    {"partId":"0","mimeType":"text/plain","filename":"notes.txt",
			     "body":{"data":"` + b64url("ATTACHED NOTES, NOT THE BODY") + `"}},
			    {"partId":"1","mimeType":"text/html","filename":"",
			     "body":{"data":"` + b64url("<p>The sandbox date moved.</p>") + `"}}
			  ]
			}`,
			want:         "The sandbox date moved.",
			wantMissing:  []string{"ATTACHED NOTES"},
			wantFromHTML: true,
		},
		{
			name: "base64url alphabet decodes",
			payload: `{
			  "mimeType":"text/plain","filename":"",
			  "body":{"data":"` +
				base64.RawURLEncoding.EncodeToString([]byte("Nordwind 🚚 delay: sandbox slips 🧿 to 2026-07-15")) + `"}
			}`,
			want: "Nordwind 🚚 delay: sandbox slips 🧿 to 2026-07-15",
		},
		{
			name: "padded base64url decodes",
			payload: `{
			  "mimeType":"text/plain","filename":"",
			  "body":{"data":"` +
				base64.URLEncoding.EncodeToString([]byte("Nordwind 🚚 delay: sandbox slips 🧿 to 2026-07-15")) + `"}
			}`,
			want: "Nordwind 🚚 delay: sandbox slips 🧿 to 2026-07-15",
		},
		{
			name: "standard base64 from a non-conforming client decodes",
			payload: `{
			  "mimeType":"text/plain","filename":"",
			  "body":{"data":"` +
				base64.StdEncoding.EncodeToString([]byte("Nordwind 🚚 delay: sandbox slips 🧿 to 2026-07-15")) + `"}
			}`,
			want: "Nordwind 🚚 delay: sandbox slips 🧿 to 2026-07-15",
		},
		{
			name: "quoted-printable text is decoded",
			payload: `{
			  "mimeType":"text/plain","filename":"",
			  "headers":[
			    {"name":"Content-Type","value":"text/plain; charset=UTF-8"},
			    {"name":"Content-Transfer-Encoding","value":"quoted-printable"}
			  ],
			  "body":{"data":"` +
				b64url("Nordwind said the sandbox slips =\r\nto 2026-07-15 (=E2=89=883 weeks).") + `"}
			}`,
			want: "Nordwind said the sandbox slips to 2026-07-15 (≈3 weeks).",
		},
		{
			name: "quoted-printable header spelling is matched case-insensitively",
			payload: `{
			  "mimeType":"text/plain","filename":"",
			  "headers":[{"name":"content-transfer-encoding","value":"Quoted-Printable"}],
			  "body":{"data":"` + b64url("Launch date =\r\nmoved to 2026-08-14.") + `"}
			}`,
			want: "Launch date moved to 2026-08-14.",
		},
		{
			name: "quoted-printable html is decoded then stripped",
			payload: `{
			  "mimeType":"multipart/alternative","filename":"",
			  "parts":[
			    {"partId":"0","mimeType":"text/html","filename":"",
			     "headers":[{"name":"Content-Transfer-Encoding","value":"quoted-printable"}],
			     "body":{"data":"` +
				b64url("<p>Sandbox slips =\r\nto 2026-07-15 (=E2=89=883 weeks)</p>") + `"}}
			  ]
			}`,
			want:         "Sandbox slips to 2026-07-15 (≈3 weeks)",
			wantFromHTML: true,
		},
		{
			name: "an empty text/plain part does not shadow a real html body",
			payload: `{
			  "mimeType":"multipart/alternative","filename":"",
			  "parts":[
			    {"partId":"0","mimeType":"text/plain","filename":"","body":{"size":0,"data":""}},
			    {"partId":"1","mimeType":"text/html","filename":"",
			     "body":{"data":"` + b64url("<p>The sandbox date moved.</p>") + `"}}
			  ]
			}`,
			want:         "The sandbox date moved.",
			wantFromHTML: true,
		},
		{
			name: "attachment-only message yields no body",
			payload: `{
			  "mimeType":"multipart/mixed","filename":"",
			  "parts":[
			    {"partId":"0","mimeType":"application/pdf","filename":"invoice.pdf",
			     "body":{"size":1024,"attachmentId":"ANGjdJ_attachment_id"}}
			  ]
			}`,
			want: "",
		},
		{
			name: "script and style contents never reach the model",
			payload: `{
			  "mimeType":"text/html","filename":"",
			  "body":{"data":"` +
				b64url(`<html><head><style>.x{color:#fff}</style>`+
					`<script>window.track("open")</script></head>`+
					`<body><p>Delay confirmed</p></body></html>`) + `"}
			}`,
			wantContains: []string{"Delay confirmed"},
			wantMissing:  []string{"color:#fff", "window.track", "<script"},
			wantFromHTML: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, fromHTML := extractBody(parsePayload(t, tc.payload))

			if fromHTML != tc.wantFromHTML {
				t.Errorf("fromHTML = %t, want %t", fromHTML, tc.wantFromHTML)
			}
			if tc.wantContains == nil {
				if body != tc.want {
					t.Errorf("body mismatch\ngot:  %q\nwant: %q", body, tc.want)
				}
			} else {
				for _, want := range tc.wantContains {
					if !strings.Contains(body, want) {
						t.Errorf("body is missing %q\ngot: %q", want, body)
					}
				}
			}
			for _, missing := range tc.wantMissing {
				if strings.Contains(body, missing) {
					t.Errorf("body leaks %q\ngot: %q", missing, body)
				}
			}
		})
	}
}

// A nil payload is what a metadata-format fetch hands back; it must not panic.
func TestExtractBodyNilPayload(t *testing.T) {
	body, fromHTML := extractBody(nil)
	if body != "" || fromHTML {
		t.Errorf("extractBody(nil) = (%q, %t), want (\"\", false)", body, fromHTML)
	}
}

// Undecodable data must not surface as garbage the model would treat as text.
func TestExtractBodyIgnoresUndecodableData(t *testing.T) {
	payload := parsePayload(t, `{
	  "mimeType":"text/plain","filename":"",
	  "body":{"data":"!!!! not base64 at all !!!!"}
	}`)
	if body, _ := extractBody(payload); body != "" {
		t.Errorf("body = %q, want empty for undecodable data", body)
	}
}

// One pathological thread must not blow the prompt budget for every later
// iteration of the agent loop, and a body that was cut must say so — otherwise
// the model reads a truncated thread as a complete one.
func TestExtractBodyIsBounded(t *testing.T) {
	long := strings.Repeat("Nordwind delay notice. ", 2000)
	payload := parsePayload(t, `{
	  "mimeType":"text/plain","filename":"",
	  "body":{"data":"`+b64url(long)+`"}
	}`)

	body, _ := extractBody(payload)
	if n := len([]rune(body)); n >= len([]rune(long)) {
		t.Errorf("body runes = %d, want it capped below the source length %d", n, len([]rune(long)))
	}
	if !strings.Contains(body, "truncated") {
		t.Errorf("a cut body must be marked as truncated\ngot tail: %q", body[max(0, len(body)-80):])
	}
}

// Header lookup is case-insensitive: RFC 5322 header names are, and Gmail
// echoes whatever the sender wrote.
func TestHeaderValueIsCaseInsensitive(t *testing.T) {
	headers := []messageHeader{
		{Name: "SUBJECT", Value: "  Nordwind v3 refunds sandbox delay  "},
		{Name: "from", Value: "ops@nordwind.example"},
	}
	if got := headerValue(headers, "Subject"); got != "Nordwind v3 refunds sandbox delay" {
		t.Errorf("Subject = %q, want the trimmed value", got)
	}
	if got := headerValue(headers, "From"); got != "ops@nordwind.example" {
		t.Errorf("From = %q, want ops@nordwind.example", got)
	}
	if got := headerValue(headers, "Bcc"); got != "" {
		t.Errorf("missing header = %q, want empty", got)
	}
}
