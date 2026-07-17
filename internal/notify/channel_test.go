package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The three payloads have to be the shape each provider actually accepts, because a malformed
// body is a 400 the worker cannot tell from a network blip — it just retries and then gives up.
func TestChannelPayloadsAreWellFormed(t *testing.T) {
	d := Data{
		Event: DeployFailed, Subject: "Deploy failed: billing",
		Title: "Deploy failed: billing", Summary: "It exited 1.",
		Detail: "pull access denied", Link: "https://ops.example.com/x", Failed: true,
	}

	for _, kind := range []string{"slack", "discord", "webhook"} {
		body, err := RenderChannel(kind, d)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		var into map[string]any
		if err := json.Unmarshal([]byte(body), &into); err != nil {
			t.Fatalf("%s produced invalid JSON: %v\n%s", kind, err, body)
		}
	}

	// Slack: the fallback text is the top level, the rich content is in a block.
	slack, _ := RenderChannel("slack", d)
	if !strings.Contains(slack, `"text":"Deploy failed: billing"`) || !strings.Contains(slack, `"blocks"`) {
		t.Errorf("slack payload missing fallback text or blocks: %s", slack)
	}

	// Discord: a red embed for a failure (0xe74c3c = 15158332).
	discord, _ := RenderChannel("discord", d)
	if !strings.Contains(discord, "15158332") {
		t.Errorf("a failed discord message should carry the red colour: %s", discord)
	}

	// Webhook: the structured event, by its documented field names.
	hook, _ := RenderChannel("webhook", d)
	if !strings.Contains(hook, `"event":"deploy.failed"`) || !strings.Contains(hook, `"failed":true`) {
		t.Errorf("webhook payload is not the structured event: %s", hook)
	}

	if _, err := RenderChannel("carrier-pigeon", d); err == nil {
		t.Error("an unknown channel kind was rendered rather than refused")
	}
}

// PostChannel must treat only 2xx as delivered — a 4xx from a chat webhook means the message did
// NOT arrive, and calling that success is how an alert silently evaporates.
func TestPostChannelOnlyAcceptsTwoXX(t *testing.T) {
	var got string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	if err := PostChannel(context.Background(), ok.Client(), ok.URL, `{"text":"hi"}`); err != nil {
		t.Fatalf("a 200 was treated as failure: %v", err)
	}
	if got != `{"text":"hi"}` {
		t.Errorf("the body was not delivered verbatim: %q", got)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no_service"))
	}))
	defer bad.Close()

	err := PostChannel(context.Background(), bad.Client(), bad.URL, `{}`)
	if err == nil {
		t.Fatal("a 404 was treated as a successful delivery")
	}
	// The provider's own words must survive into the error, since that sentence is the whole
	// difference between a fixable dead letter and a mystery.
	if !strings.Contains(err.Error(), "no_service") {
		t.Errorf("the provider's error was dropped: %v", err)
	}
}
