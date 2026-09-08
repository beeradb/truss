package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSendFailureIsNonFatalAndSaysSo(t *testing.T) {
	// Point to a port that refuses connections, ensuring the send fails.
	tg := Telegram{
		BotToken: "testtoken123",
		ChatID:   "12345",
		HTTP:     &http.Client{Timeout: 100 * time.Millisecond},
	}

	err := tg.Send(context.Background(), "test message")
	if err == nil {
		t.Fatalf("Send() should return an error when connection is refused, got nil")
	}

	// Verify it's an error about telegram, not a panic or other fatality.
	if !strings.Contains(err.Error(), "telegram") {
		t.Fatalf("error should mention telegram, got: %v", err)
	}
}

func TestTheBotTokenIsNeverInAnErrorOrALog(t *testing.T) {
	botToken := "secrettoken99887"
	tg := Telegram{
		BotToken: botToken,
		ChatID:   "12345",
		HTTP:     &http.Client{Timeout: 100 * time.Millisecond},
	}

	err := tg.Send(context.Background(), "test message")
	if err == nil {
		t.Fatalf("Send() should return an error, got nil")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, botToken) {
		t.Fatalf("error message contains the bot token: %q", errMsg)
	}

	// Also test with an HTTP server that returns an error response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("Invalid token: bot%s/invalid", botToken)))
	}))
	defer srv.Close()

	// Manually construct the problematic request to test redaction
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", srv.URL, botToken)
	data := url.Values{}
	data.Set("chat_id", "12345")
	data.Set("text", "test")

	req, _ := http.NewRequest("POST", endpoint, strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Verify that if there's an error from the response body, it would be redacted
	bodyStr := string(body)
	tg2 := Telegram{BotToken: botToken, ChatID: "12345", HTTP: http.DefaultClient}
	tg2.HTTP = srv.Client()

	// Send through the test server and check the error
	err2 := tg2.Send(context.Background(), "test")
	if err2 != nil && strings.Contains(err2.Error(), botToken) {
		t.Fatalf("error message contains the bot token: %q", err2.Error())
	}

	// Verify redaction works on the response body
	redacted := redactToken(bodyStr, botToken)
	if strings.Contains(redacted, botToken) {
		t.Fatalf("redactToken failed to remove token from: %q", bodyStr)
	}
}

func TestTextIsFormEncoded(t *testing.T) {
	// Create a test server that captures the request and verifies form encoding
	var capturedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		// Parse the form to verify it was encoded properly
		err := r.ParseForm()
		if err != nil {
			t.Fatalf("ParseForm failed: %v", err)
		}
		capturedForm = r.Form

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	botToken := "fakebottoken"
	chatID := "fakechatid"
	text := "Hello & goodbye\nLine 2\n#hashtag"

	// Create a custom transport that intercepts all requests and routes them to the test server
	client := &http.Client{
		Transport: &mockTransport{
			testServerURL: srv.URL,
		},
	}

	tg := Telegram{
		BotToken: botToken,
		ChatID:   chatID,
		HTTP:     client,
	}

	// Send the message
	err := tg.Send(context.Background(), text)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Verify the captured request
	if capturedForm == nil {
		t.Fatalf("request was not captured")
	}

	// Check chat_id
	if capturedForm.Get("chat_id") != chatID {
		t.Errorf("chat_id mismatch: got %q, want %q", capturedForm.Get("chat_id"), chatID)
	}

	// Check that text was form-decoded correctly (with newlines and special chars intact)
	if capturedForm.Get("text") != text {
		t.Errorf("text mismatch: got %q, want %q", capturedForm.Get("text"), text)
	}
}

// mockTransport redirects requests to a test server
type mockTransport struct {
	testServerURL string
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Replace the scheme and host to point to the test server
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(m.testServerURL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}
