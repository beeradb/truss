package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Telegram sends a message to a Telegram chat.
type Telegram struct {
	BotToken string
	ChatID   string
	HTTP     *http.Client

	// BaseURL overrides the API host. Empty is the real one.
	//
	// ⚠️ ITS ABSENCE IS A BUG RATHER THAN A MISSING CONVENIENCE, AND THIS
	// PROJECT HAS ALREADY PAID FOR THE LESSON ONCE. internal/secrets' probe
	// had no such override and every test run therefore posted to the real
	// Cloudflare API (see the note on runExpirySweep). A command that builds
	// its own Telegram -- `truss skip` does, because it must announce before
	// it acts -- has no other seam a test can reach, so without this the
	// choice is between an untested announcement and a test suite that
	// messages a real chat.
	BaseURL string
}

// apiBase is the host Send posts to.
func (t Telegram) apiBase() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return "https://api.telegram.org"
}

// Send posts the text to Telegram, form-encoded. A send failure is non-fatal
// and returns an error; the token is never included in that error.
func (t Telegram) Send(ctx context.Context, text string) error {
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", t.apiBase(), t.BotToken)

	data := url.Values{}
	data.Set("chat_id", t.ChatID)
	data.Set("text", text)

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: %v", redactToken(err.Error(), t.BotToken))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %v", redactToken(err.Error(), t.BotToken))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram: %d %s", resp.StatusCode, redactToken(string(body), t.BotToken))
	}

	return nil
}

// redactToken replaces the bot token with a placeholder in an error message.
func redactToken(msg string, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "***")
}
