package gmail

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The write direction: a fixture becomes an RFC 5322 message.
//
// The seeded emails are the third hop of the multi-hop story, and the thing
// that makes them useful is their dates: Nordwind's delay notice has to sit in
// June, before the launch-date decision it caused. BuildRFC5322 writing a
// malformed Date header, or declaring an encoding that does not match the body,
// would be discovered as "the agent cannot answer the acceptance question" long
// after the fact.

func TestBuildRFC5322(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, time.June, 8, 8, 47, 0, 0, time.FixedZone("CEST", 2*60*60))

	tests := []struct {
		name         string
		msg          FixtureMessage
		wantHeaders  []string
		wantBody     string
		wantMissing  []string
		wantEncoding string
	}{
		{
			name: "ascii body is sent 7bit and verbatim",
			msg: FixtureMessage{
				From:    "Ines Brandt <ines.brandt@nordwindpayments.example>",
				To:      "priya@vantagelabs.example",
				Subject: "v3 refunds sandbox delayed",
				Date:    when,
				Body:    "The sandbox will not be available on 29 May.\n\nRevised: late July.",
			},
			wantHeaders: []string{
				"From: Ines Brandt <ines.brandt@nordwindpayments.example>",
				"To: priya@vantagelabs.example",
				"Subject: v3 refunds sandbox delayed",
				"Date: Mon, 08 Jun 2026 08:47:00 +0200",
				`Content-Type: text/plain; charset="UTF-8"`,
			},
			wantEncoding: "7bit",
			wantBody:     "The sandbox will not be available on 29 May.\r\n\r\nRevised: late July.",
		},
		{
			name: "cc is emitted only when set",
			msg: FixtureMessage{
				From: "a@x.example", To: "b@x.example",
				Cc:      "Marcus Whitfield <marcus@vantagelabs.example>",
				Subject: "with cc", Date: when, Body: "body",
			},
			wantHeaders:  []string{"Cc: Marcus Whitfield <marcus@vantagelabs.example>"},
			wantEncoding: "7bit",
		},
		{
			name: "no cc header when empty",
			msg: FixtureMessage{
				From: "a@x.example", To: "b@x.example",
				Subject: "no cc", Date: when, Body: "body",
			},
			wantMissing:  []string{"Cc:"},
			wantEncoding: "7bit",
		},
		{
			name: "non-ascii body switches to base64 rather than lying about 7bit",
			msg: FixtureMessage{
				From: "a@x.example", To: "b@x.example",
				Subject: "accents", Date: when,
				Body: "Der Zahlungsdienstleister hat die Verzögerung bestätigt.",
			},
			wantEncoding: "base64",
			// The raw text must NOT appear: it is encoded.
			wantMissing: []string{"Verzögerung"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := BuildRFC5322(tc.msg)
			if err != nil {
				t.Fatalf("BuildRFC5322() error = %v", err)
			}

			for _, header := range tc.wantHeaders {
				if !strings.Contains(raw, header+"\r\n") {
					t.Errorf("message is missing header %q\n--- got ---\n%s", header, raw)
				}
			}
			for _, absent := range tc.wantMissing {
				if strings.Contains(raw, absent) {
					t.Errorf("message unexpectedly contains %q\n--- got ---\n%s", absent, raw)
				}
			}
			if tc.wantEncoding != "" {
				want := "Content-Transfer-Encoding: " + tc.wantEncoding + "\r\n"
				if !strings.Contains(raw, want) {
					t.Errorf("message does not declare %q", want)
				}
			}
			if tc.wantBody != "" {
				_, body, found := strings.Cut(raw, "\r\n\r\n")
				if !found {
					t.Fatalf("message has no header/body separator:\n%s", raw)
				}
				if body != tc.wantBody {
					t.Errorf("body = %q, want %q", body, tc.wantBody)
				}
			}

			// Every line must be CRLF-terminated; a bare LF is not valid in a
			// message and Gmail's insert rejects it.
			for line := range strings.SplitSeq(strings.TrimSuffix(raw, "\r\n"), "\r\n") {
				if strings.Contains(line, "\n") {
					t.Errorf("line %q contains a bare newline", line)
				}
			}
		})
	}
}

// TestBuildRFC5322RejectsIncompleteFixtures covers the two failures that would
// otherwise produce a message with no timeline value.
func TestBuildRFC5322RejectsIncompleteFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msg         FixtureMessage
		wantErrPart string
	}{
		{
			name:        "no date",
			msg:         FixtureMessage{From: "a@x.example", To: "b@x.example", Subject: "undated"},
			wantErrPart: "no date",
		},
		{
			name:        "no recipient",
			msg:         FixtureMessage{From: "a@x.example", Subject: "no to", Date: time.Now()},
			wantErrPart: "From and To",
		},
		{
			name:        "no sender",
			msg:         FixtureMessage{To: "b@x.example", Subject: "no from", Date: time.Now()},
			wantErrPart: "From and To",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := BuildRFC5322(tc.msg); err == nil {
				t.Fatal("BuildRFC5322() succeeded, want an error")
			} else if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErrPart)
			}
		})
	}
}

// TestEncodeAddress checks that only the display name is encoded. An
// encoded-word inside the angle brackets is not a valid address, and Gmail
// rejects the whole message rather than the header.
func TestEncodeAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "bare address is untouched", input: "priya@vantagelabs.example", want: "priya@vantagelabs.example"},
		{
			name:  "ascii display name is untouched",
			input: "Ines Brandt <ines@nordwindpayments.example>",
			want:  "Ines Brandt <ines@nordwindpayments.example>",
		},
		{
			name:  "non-ascii display name is encoded, address is not",
			input: "Jörg Müller <joerg@nordwindpayments.example>",
			want:  "=?utf-8?q?J=C3=B6rg_M=C3=BCller?= <joerg@nordwindpayments.example>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := encodeAddress(tc.input); got != tc.want {
				t.Errorf("encodeAddress(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// Regression: a seeded fixture must be reachable by an ordinary search.
//
// The cross-source acceptance test failed with all sixteen fixtures present, correct,
// and correctly dated, because they were inserted with only the custom
// "Vantage Labs" label. Gmail put them in the mailbox but outside the scope a
// normal query reaches — `Nordwind` returned nothing while `in:anywhere
// Nordwind` returned everything — so the agent could not find the email hop no
// matter how it phrased the search.
//
// The invariant that was missing: whatever else a fixture carries, it carries
// INBOX.
func TestFixtureLabelIDsAlwaysIncludeInbox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		fixtureLabelID string
		wantContains   []string
		wantLen        int
	}{
		{
			name:           "with a fixture label",
			fixtureLabelID: "Label_8842",
			wantContains:   []string{LabelInbox, LabelUnread, "Label_8842"},
			wantLen:        3,
		},
		{
			name:           "without a fixture label",
			fixtureLabelID: "",
			wantContains:   []string{LabelInbox, LabelUnread},
			wantLen:        2,
		},
		{
			name:           "a blank fixture label is not appended",
			fixtureLabelID: "   ",
			wantContains:   []string{LabelInbox, LabelUnread},
			wantLen:        2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := FixtureLabelIDs(tc.fixtureLabelID)
			if len(got) != tc.wantLen {
				t.Fatalf("FixtureLabelIDs(%q) = %v, want %d labels", tc.fixtureLabelID, got, tc.wantLen)
			}
			for _, want := range tc.wantContains {
				if !slices.Contains(got, want) {
					t.Errorf("FixtureLabelIDs(%q) = %v, missing %q", tc.fixtureLabelID, got, want)
				}
			}
		})
	}
}
