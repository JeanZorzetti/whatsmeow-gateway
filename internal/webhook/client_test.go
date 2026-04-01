package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSend_SuccessfulDelivery(t *testing.T) {
	var received atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Store(true)
		w.WriteHeader(200)
	}))
	defer server.Close()

	c := NewClient(server.URL, "")
	defer c.Shutdown()

	err := c.Send(Event{Type: "test", InstanceID: "inst-1", Data: "hello"})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if !received.Load() {
		t.Error("server did not receive request")
	}
}

func TestSend_HMACSignature(t *testing.T) {
	secret := "my-secret"
	var receivedSig string
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSig = r.Header.Get("X-Webhook-Signature")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer server.Close()

	c := NewClient(server.URL, secret)
	defer c.Shutdown()

	c.Send(Event{Type: "test", InstanceID: "inst-1", Data: "payload"})

	// Verify signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(receivedBody)
	expected := hex.EncodeToString(mac.Sum(nil))

	if receivedSig != expected {
		t.Errorf("signature mismatch: got %s, want %s", receivedSig, expected)
	}
}

func TestSend_EmptyURL(t *testing.T) {
	c := NewClient("", "secret")
	defer c.Shutdown()

	err := c.Send(Event{Type: "test"})
	if err != nil {
		t.Errorf("empty URL should return nil, got %v", err)
	}
}

func TestSend_RetryOn5xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()

	c := NewClient(server.URL, "")
	defer c.Shutdown()

	// First delivery fails (500), queued for retry
	c.Send(Event{Type: "retry-test", InstanceID: "inst-1"})

	// Wait for retries
	time.Sleep(6 * time.Second)

	if attempts.Load() < 2 {
		t.Errorf("expected at least 2 attempts, got %d", attempts.Load())
	}
}

func TestSend_NoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(400)
	}))
	defer server.Close()

	c := NewClient(server.URL, "")
	defer c.Shutdown()

	c.Send(Event{Type: "4xx-test", InstanceID: "inst-1"})

	time.Sleep(3 * time.Second)

	if attempts.Load() > 1 {
		t.Errorf("4xx should not retry, got %d attempts", attempts.Load())
	}
}

func TestEventJSON(t *testing.T) {
	evt := Event{Type: "message", InstanceID: "inst-1", Timestamp: 1234567890, Data: map[string]string{"text": "hi"}}
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	var parsed Event
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Type != "message" || parsed.InstanceID != "inst-1" {
		t.Errorf("unexpected parsed event: %+v", parsed)
	}
}
