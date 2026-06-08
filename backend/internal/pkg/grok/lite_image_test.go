package grok

import (
	"errors"
	"testing"
)

func TestExtractLiteImageEvent(t *testing.T) {
	data := `{"result":{"response":{"cardAttachment":{"jsonData":"{\"image_chunk\":{\"progress\":100,\"imageUrl\":\"generated/foo.jpg\",\"imageUuid\":\"img-1\",\"moderated\":false}}"}}}}`

	event, ok, err := extractLiteImageEvent(data)
	if err != nil {
		t.Fatalf("extractLiteImageEvent error: %v", err)
	}
	if !ok {
		t.Fatal("extractLiteImageEvent ok=false, want true")
	}
	if event.Progress != 100 {
		t.Fatalf("progress=%d, want 100", event.Progress)
	}
	if event.ID != "img-1" {
		t.Fatalf("id=%q, want img-1", event.ID)
	}
	if event.URL != "https://assets.grok.com/generated/foo.jpg" {
		t.Fatalf("url=%q", event.URL)
	}
}

func TestGrokErrorClassifiers(t *testing.T) {
	invalid := &UpstreamError{StatusCode: 401, Message: `{"error":{"message":"Failed to look up session ID. [WKE=unauthenticated:invalid-credentials]"}}`}
	if !IsInvalidSessionError(invalid) {
		t.Fatal("IsInvalidSessionError=false, want true")
	}

	limited := &UpstreamError{StatusCode: 502, Message: "rate_limit_exceeded: Image rate limit exceeded"}
	if !IsRateLimitError(limited) {
		t.Fatal("IsRateLimitError=false, want true")
	}

	if IsRateLimitError(errors.New("temporary upstream disconnect")) {
		t.Fatal("IsRateLimitError=true for non-rate-limit error")
	}
}
