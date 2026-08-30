package gmail_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"cortex/internal/tools/gmail"
)

// TestInsertMessageSendsInboxLabel checks the label set actually reaches Gmail
// on the wire, not merely that the helper computes it.
func TestInsertMessageSendsInboxLabel(t *testing.T) {
	t.Parallel()

	fake := newFakeGmail(t, map[string]*route{
		"/gmail/v1/users/me/messages": bodyRoute(200, `{"id":"18ff01","threadId":"18ff01"}`),
	})
	client := fake.client(testQueryScope)

	id, err := client.InsertMessage(context.Background(), gmail.FixtureMessage{
		From:    "Ines Brandt <ines@nordwindpayments.example>",
		To:      "priya@vantagelabs.example",
		Subject: "v3 refunds sandbox delayed",
		Date:    time.Date(2026, time.June, 8, 8, 47, 0, 0, time.UTC),
		Body:    "The sandbox has slipped.",
	}, gmail.FixtureLabelIDs("Label_8842"))
	if err != nil {
		t.Fatalf("InsertMessage() error = %v", err)
	}
	if id != "18ff01" {
		t.Errorf("id = %q, want 18ff01", id)
	}

	requests := fake.requestsTo("/gmail/v1/users/me/messages")
	if len(requests) != 1 {
		t.Fatalf("got %d insert requests, want 1", len(requests))
	}
	req := requests[0]

	// Backdating is the other half of what makes a fixture useful.
	if got := req.query.Get("internalDateSource"); got != "dateHeader" {
		t.Errorf("internalDateSource = %q, want dateHeader — without it Gmail stamps the message today", got)
	}

	var sent struct {
		Raw      string   `json:"raw"`
		LabelIDs []string `json:"labelIds"`
	}
	if err := json.Unmarshal([]byte(req.body), &sent); err != nil {
		t.Fatalf("decode insert body: %v", err)
	}
	if !slices.Contains(sent.LabelIDs, gmail.LabelInbox) {
		t.Errorf("insert sent labelIds %v, which omits %q — the message would be unreachable by search",
			sent.LabelIDs, gmail.LabelInbox)
	}
	if !slices.Contains(sent.LabelIDs, "Label_8842") {
		t.Errorf("insert sent labelIds %v, which omits the fixture label", sent.LabelIDs)
	}
}
