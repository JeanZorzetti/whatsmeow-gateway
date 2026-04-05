package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/JeanZorzetti/whatsmeow-gateway/internal/webhook"
)

const (
	streamKeyPrefix = "wa:sync:"
	batchSize       = 500
	flushTimeout    = 5 * time.Minute
)

// pushToStream enqueues a message payload into a Redis Stream (sub-ms write).
// Returns false if Redis is unavailable (caller should fall back to direct webhook).
func (m *Manager) pushToStream(instanceID string, payload map[string]any) bool {
	if m.redis == nil {
		return false
	}

	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal payload for redis", "error", err)
		return false
	}

	ctx := context.Background()
	streamKey := streamKeyPrefix + instanceID

	_, err = m.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]interface{}{
			"payload": string(data),
		},
	}).Result()

	if err != nil {
		slog.Error("redis XADD failed — falling back to direct webhook", "error", err)
		return false
	}

	return true
}

// flushSyncBuffer reads all messages from the Redis Stream for an instance,
// batches them, and sends to the CRM via webhook. Called after OfflineSyncCompleted.
func (m *Manager) flushSyncBuffer(inst *Instance) {
	if m.redis == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()

	streamKey := streamKeyPrefix + inst.ID
	totalFlushed := 0
	lastID := "0-0"

	for {
		results, err := m.redis.XRange(ctx, streamKey, lastID, "+").Result()
		if err != nil {
			slog.Error("redis XRANGE failed", "error", err, "instance", inst.ID)
			break
		}

		if len(results) == 0 {
			break
		}

		// Skip already-processed entries
		if lastID != "0-0" {
			// Remove the first result since it matches lastID (inclusive range)
			if len(results) > 0 && results[0].ID == lastID {
				results = results[1:]
			}
			if len(results) == 0 {
				break
			}
		}

		// Process in batches
		for i := 0; i < len(results); i += batchSize {
			end := i + batchSize
			if end > len(results) {
				end = len(results)
			}
			batch := results[i:end]

			messages := make([]map[string]any, 0, len(batch))
			ids := make([]string, 0, len(batch))

			for _, r := range batch {
				var payload map[string]any
				raw, ok := r.Values["payload"].(string)
				if !ok {
					continue
				}
				if err := json.Unmarshal([]byte(raw), &payload); err != nil {
					continue
				}
				messages = append(messages, payload)
				ids = append(ids, r.ID)
			}

			if len(messages) == 0 {
				continue
			}

			// Send batch to CRM
			m.webhook.Send(webhook.Event{
				Type:       "history.sync.batch",
				InstanceID: inst.ID,
				Data: map[string]any{
					"organizationId": inst.OrgID,
					"messages":       messages,
					"count":          len(messages),
					"isFinal":        end >= len(results) && len(results) < batchSize,
				},
			})

			// Remove processed entries
			if len(ids) > 0 {
				m.redis.XDel(ctx, streamKey, ids...)
			}

			totalFlushed += len(messages)
		}

		lastID = results[len(results)-1].ID
	}

	// Clean up the stream
	m.redis.Del(ctx, streamKey)

	slog.Info(fmt.Sprintf("sync buffer flushed: %d messages sent to CRM", totalFlushed),
		"instance", inst.ID,
	)
}

// delayedFlush waits for history sync blobs to arrive (they often come AFTER
// OfflineSyncCompleted), then flushes the Redis buffer to the CRM.
func (m *Manager) delayedFlush(inst *Instance) {
	// Wait 15s for history blobs to arrive
	time.Sleep(15 * time.Second)

	// Check if more blobs arrived; keep waiting if sync is still in progress
	for i := 0; i < 12; i++ { // max 3 more minutes (12 x 15s)
		inst.Sync.mu.RLock()
		inProgress := inst.Sync.InProgress
		inst.Sync.mu.RUnlock()

		if !inProgress {
			break
		}
		slog.Info("sync still in progress — waiting before flush", "instance", inst.ID)
		time.Sleep(15 * time.Second)
	}

	inst.Sync.mu.Lock()
	inst.Sync.InProgress = false
	inst.Sync.mu.Unlock()

	buffered := m.streamLen(inst.ID)
	slog.Info("delayed flush triggered", "instance", inst.ID, "buffered", buffered)

	if buffered > 0 {
		m.flushSyncBuffer(inst)
	}

	m.webhook.Send(webhook.Event{
		Type:       "sync.completed",
		InstanceID: inst.ID,
		Data: map[string]any{
			"organizationId":     inst.OrgID,
			"totalMessages":      inst.Sync.TotalMessages,
			"totalConversations": inst.Sync.TotalConversations,
		},
	})
}

// streamLen returns the number of pending messages in the Redis Stream for an instance.
func (m *Manager) streamLen(instanceID string) int64 {
	if m.redis == nil {
		return 0
	}
	n, _ := m.redis.XLen(context.Background(), streamKeyPrefix+instanceID).Result()
	return n
}
