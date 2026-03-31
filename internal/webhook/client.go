package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type Client struct {
	url    string
	secret string
	http   *http.Client
}

func NewClient(url, secret string) *Client {
	return &Client{
		url:    url,
		secret: secret,
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type Event struct {
	Type       string `json:"type"`
	InstanceID string `json:"instanceId"`
	Timestamp  int64  `json:"timestamp"`
	Data       any    `json:"data"`
}

func (c *Client) Send(evt Event) error {
	if c.url == "" {
		return nil
	}

	evt.Timestamp = time.Now().Unix()

	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	req, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if c.secret != "" {
		mac := hmac.New(sha256.New, []byte(c.secret))
		mac.Write(body)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Webhook-Signature", sig)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("webhook delivery failed", "url", c.url, "error", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Warn("webhook returned error", "url", c.url, "status", resp.StatusCode)
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}

	return nil
}
