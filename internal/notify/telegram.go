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
}

// Send posts the text to Telegram, form-encoded. A send failure is non-fatal
// and returns an error; the token is never included in that error.
func (t Telegram) Send(ctx context.Context, text string) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.BotToken)

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
