package gmail_test

import (
	"strings"
	"testing"

	"cortex/internal/tools"
	"cortex/internal/tools/gmail"
)

// Day 8 (REQ-8.4): a user-connected mailbox is the user's own, so the
// registry builder constructs its client with AllowUnscoped instead of a
// QueryScope pin. The flag is deliberately narrow: it permits an EMPTY scope
// and nothing else — combining it with a pin is a contradiction NewClient
// must refuse, so a demo-workspace client can never be quietly widened.
// (TestNewClientRequiresQueryScope in tools_test.go covers the third corner:
// empty scope without the flag is refused.)

func TestNewClientAllowUnscopedContradictsAQueryScope(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{})
	_, err := gmail.NewClient(gmail.Config{
		TokenSource:   fake.tokenSource(),
		QueryScope:    testQueryScope,
		AllowUnscoped: true,
	})
	if err == nil {
		t.Fatal("NewClient accepted AllowUnscoped together with a QueryScope")
	}
	if !strings.Contains(err.Error(), "AllowUnscoped") {
		t.Errorf("error %q does not name AllowUnscoped", err)
	}
}

func TestAllowUnscopedClientSearchesWithoutAScopePin(t *testing.T) {
	fake := newFakeGmail(t, map[string]*route{
		pathMessages: fixtureRoute("messages_list.json"),
		pathMessage1: fixtureRoute("message_meta_1.json"),
		pathMessage2: fixtureRoute("message_meta_2.json"),
	})
	client, err := gmail.NewClient(gmail.Config{
		BaseURL:       fake.server.URL,
		TokenSource:   fake.tokenSource(),
		AllowUnscoped: true,
		HTTPClient:    fake.server.Client(),
		MaxRetries:    1,
		MinInterval:   -1,
		Logger:        discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewClient with AllowUnscoped and no scope: %v", err)
	}

	var search tools.Tool
	for _, tool := range gmail.NewTools(client) {
		if tool.Name() == "gmail_search" {
			search = tool
		}
	}
	if search == nil {
		t.Fatal("gmail_search missing from the tool set")
	}
	mustExecute(t, search, `{"query":"from:nordwind.example refunds"}`)

	list := fake.requestsTo(pathMessages)
	if len(list) != 1 {
		t.Fatalf("requests to %s = %d, want 1", pathMessages, len(list))
	}
	// The user's query goes through verbatim: no label pin prepended, no
	// parenthesis wrapping — there is nothing to confine the search to.
	if got, want := list[0].query.Get("q"), "from:nordwind.example refunds"; got != want {
		t.Errorf("q = %q, want the unscoped query %q", got, want)
	}
}
