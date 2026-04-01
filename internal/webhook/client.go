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
	"sync"
	"time"
)

const (
	maxRetries     = 3
	initialBackoff = 2 * time.Second
	maxBackoff     = 30 * time.Second
	queueSize      = 1000
)

type Client struct {
	url    string
	secret string
	http   *http.Client

	// Async retry queue
	queue chan retryItem
	wg    sync.WaitGroup
	done  chan struct{}

	// Dead-letter callback (optional, for DB persistence)
	onDeadLetter func(evt Event, lastErr error)
}

type retryItem struct {
	event   Event
	body    []byte
	attempt int
}

func NewClient(url, secret string) *Client {
	c := &Client{
		url:    url,
		secret: secret,
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
		queue: make(chan retryItem, queueSize),
		done:  make(chan struct{}),
	}

	// Start retry worker
	c.wg.Add(1)
	go c.retryWorker()

	return c
}

// SetDeadLetterHandler sets a callback for events that fail after all retries.
func (c *Client) SetDeadLetterHandler(fn func(evt Event, lastErr error)) {
	c.onDeadLetter = fn
}

// Shutdown gracefully drains the retry queue.
func (c *Client) Shutdown() {
	close(c.done)
	c.wg.Wait()
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

	// Try first delivery synchronously
	if err := c.deliver(body); err != nil {
		slog.Warn("webhook first delivery failed, queuing for retry",
			"type", evt.Type, "instance", evt.InstanceID, "error", err)

		// Queue for async retry
		select {
		case c.queue <- retryItem{event: evt, body: body, attempt: 1}:
		default:
			slog.Error("webhook retry queue full, dropping event",
				"type", evt.Type, "instance", evt.InstanceID)
			if c.onDeadLetter != nil {
				c.onDeadLetter(evt, fmt.Errorf("retry queue full"))
			}
		}
		return err
	}

	return nil
}

func (c *Client) deliver(body []byte) error {
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
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("webhook returned %d (server error)", resp.StatusCode)
	}

	if resp.StatusCode >= 400 {
		// 4xx errors are not retryable (bad request, auth, etc.)
		slog.Warn("webhook returned client error (not retrying)", "status", resp.StatusCode)
		return nil
	}

	return nil
}

func (c *Client) retryWorker() {
	defer c.wg.Done()

	for {
		select {
		case <-c.done:
			// Drain remaining items
			c.drainQueue()
			return
		case item := <-c.queue:
			c.processRetry(item)
		}
	}
}

func (c *Client) processRetry(item retryItem) {
	backoff := initialBackoff
	for i := 0; i < item.attempt; i++ {
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	// Wait before retry
	select {
	case <-time.After(backoff):
	case <-c.done:
		return
	}

	err := c.deliver(item.body)
	if err == nil {
		slog.Info("webhook retry succeeded",
			"type", item.event.Type, "instance", item.event.InstanceID,
			"attempt", item.attempt+1)
		return
	}

	item.attempt++
	if item.attempt >= maxRetries {
		slog.Error("webhook delivery failed after all retries (dead-letter)",
			"type", item.event.Type, "instance", item.event.InstanceID,
			"attempts", item.attempt+1, "lastError", err)

		if c.onDeadLetter != nil {
			c.onDeadLetter(item.event, err)
		}
		return
	}

	slog.Warn("webhook retry failed, re-queuing",
		"type", item.event.Type, "instance", item.event.InstanceID,
		"attempt", item.attempt, "error", err)

	select {
	case c.queue <- item:
	default:
		slog.Error("webhook retry queue full during re-queue",
			"type", item.event.Type, "instance", item.event.InstanceID)
		if c.onDeadLetter != nil {
			c.onDeadLetter(item.event, fmt.Errorf("queue full on re-queue"))
		}
	}
}

func (c *Client) drainQueue() {
	for {
		select {
		case item := <-c.queue:
			// One last attempt for each remaining item
			if err := c.deliver(item.body); err != nil {
				slog.Warn("webhook drain delivery failed",
					"type", item.event.Type, "instance", item.event.InstanceID, "error", err)
				if c.onDeadLetter != nil {
					c.onDeadLetter(item.event, err)
				}
			}
		default:
			return
		}
	}
}
