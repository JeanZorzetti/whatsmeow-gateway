package whatsapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/JeanZorzetti/whatsmeow-gateway/internal/webhook"
	storeDB "github.com/JeanZorzetti/whatsmeow-gateway/internal/store"
)

// Manager manages multiple whatsmeow client instances.
type Manager struct {
	mu        sync.RWMutex
	clients   map[string]*Instance
	container *sqlstore.Container
	db        *storeDB.DB
	webhook   *webhook.Client
}

// qrSubscriber receives QR codes via a channel.
type qrSubscriber struct {
	ch chan string
}

// SyncStats tracks history sync progress.
type SyncStats struct {
	mu               sync.RWMutex
	TotalMessages    int64     `json:"totalMessages"`
	TotalConversations int64   `json:"totalConversations"`
	LastSyncAt       time.Time `json:"lastSyncAt,omitempty"`
	SyncType         string    `json:"lastSyncType,omitempty"`
	InProgress       bool      `json:"inProgress"`
}

// Instance wraps a whatsmeow client with gateway metadata.
type Instance struct {
	Client     *whatsmeow.Client
	ID         string
	Name       string
	OrgID      string
	Status     string
	cancelFunc context.CancelFunc

	// QR broadcast: last QR is cached and sent to all new subscribers
	qrMu          sync.RWMutex
	lastQR        string
	qrSubscribers map[*qrSubscriber]struct{}

	// History sync tracking
	Sync SyncStats
}

// SubscribeQR returns a channel that receives QR codes.
// The current QR (if any) is immediately sent.
func (inst *Instance) SubscribeQR() (<-chan string, func()) {
	sub := &qrSubscriber{ch: make(chan string, 5)}

	inst.qrMu.Lock()
	inst.qrSubscribers[sub] = struct{}{}
	lastQR := inst.lastQR
	inst.qrMu.Unlock()

	// Send cached QR immediately
	if lastQR != "" {
		select {
		case sub.ch <- lastQR:
		default:
		}
	}

	unsubscribe := func() {
		inst.qrMu.Lock()
		delete(inst.qrSubscribers, sub)
		inst.qrMu.Unlock()
		close(sub.ch)
	}

	return sub.ch, unsubscribe
}

// broadcastQR sends a QR code to all subscribers and caches it.
func (inst *Instance) broadcastQR(code string) {
	inst.qrMu.Lock()
	inst.lastQR = code
	subs := make([]*qrSubscriber, 0, len(inst.qrSubscribers))
	for sub := range inst.qrSubscribers {
		subs = append(subs, sub)
	}
	inst.qrMu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- code:
		default:
			// subscriber not consuming, skip
		}
	}
}

// clearQR clears the cached QR and signals end to subscribers.
func (inst *Instance) clearQR() {
	inst.qrMu.Lock()
	inst.lastQR = ""
	inst.qrMu.Unlock()
}

func NewManager(container *sqlstore.Container, db *storeDB.DB, wh *webhook.Client) *Manager {
	return &Manager{
		clients:   make(map[string]*Instance),
		container: container,
		db:        db,
		webhook:   wh,
	}
}

// Create initializes a new whatsmeow instance and starts connecting.
func (m *Manager) Create(id, name, orgID string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.clients[id]; exists {
		return nil, fmt.Errorf("instance %s already exists", id)
	}

	// Get or create device store
	device, err := m.getOrCreateDevice(id)
	if err != nil {
		return nil, fmt.Errorf("device store: %w", err)
	}

	client := whatsmeow.NewClient(device, nil)
	ctx, cancel := context.WithCancel(context.Background())

	inst := &Instance{
		Client:        client,
		ID:            id,
		Name:          name,
		OrgID:         orgID,
		Status:        "disconnected",
		cancelFunc:    cancel,
		qrSubscribers: make(map[*qrSubscriber]struct{}),
	}

	m.clients[id] = inst

	// Register event handler
	client.AddEventHandler(func(evt interface{}) {
		m.handleEvent(inst, evt)
	})

	// Start connection in background
	go m.connect(ctx, inst)

	return inst, nil
}

// Get returns an instance by ID.
func (m *Manager) Get(id string) (*Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.clients[id]
	return inst, ok
}

// ListAll returns all in-memory instances.
func (m *Manager) ListAll() []*Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Instance, 0, len(m.clients))
	for _, inst := range m.clients {
		result = append(result, inst)
	}
	return result
}

// Remove disconnects and removes an instance.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.clients[id]
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.cancelFunc()
	inst.Client.Disconnect()
	delete(m.clients, id)

	return nil
}

// Restart reconnects an existing instance, generating new QR if needed.
func (m *Manager) Restart(id string) error {
	m.mu.Lock()
	inst, ok := m.clients[id]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.cancelFunc()
	inst.Client.Disconnect()
	inst.clearQR()
	time.Sleep(time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	inst.cancelFunc = cancel
	go m.connect(ctx, inst)

	return nil
}

func (m *Manager) connect(ctx context.Context, inst *Instance) {
	if inst.Client.Store.ID == nil {
		// No session — need QR code
		m.connectWithQR(ctx, inst)
	} else {
		// Has session — reconnect directly
		m.reconnect(ctx, inst)
	}
}

func (m *Manager) connectWithQR(ctx context.Context, inst *Instance) {
	qrChan, _ := inst.Client.GetQRChannel(ctx)
	err := inst.Client.Connect()
	if err != nil {
		slog.Error("failed to connect", "instance", inst.ID, "error", err)
		m.setStatus(inst, "error")
		return
	}

	m.setStatus(inst, "qr_pending")

	for evt := range qrChan {
		if ctx.Err() != nil {
			return
		}
		switch evt.Event {
		case "code":
			slog.Info("QR code generated", "instance", inst.ID)
			inst.broadcastQR(evt.Code)
		case "success":
			slog.Info("QR code scanned successfully", "instance", inst.ID)
			inst.clearQR()
			return
		case "timeout":
			slog.Warn("QR code timeout, auto-restarting", "instance", inst.ID)
			inst.clearQR()
			m.setStatus(inst, "qr_timeout")

			// Auto-restart for new QR after timeout
			if ctx.Err() == nil {
				inst.Client.Disconnect()
				time.Sleep(2 * time.Second)
				go m.connectWithQR(ctx, inst)
			}
			return
		}
	}
}

func (m *Manager) reconnect(ctx context.Context, inst *Instance) {
	err := inst.Client.Connect()
	if err != nil {
		slog.Error("failed to reconnect", "instance", inst.ID, "error", err)
		m.setStatus(inst, "error")

		// Auto-reconnect with backoff
		go m.autoReconnect(ctx, inst)
	}
}

func (m *Manager) autoReconnect(ctx context.Context, inst *Instance) {
	backoff := time.Second
	maxBackoff := 60 * time.Second
	maxAttempts := 30 // ~15 min of retries before alerting
	attempts := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if inst.Client.IsConnected() {
			return
		}

		attempts++
		slog.Info("attempting reconnect", "instance", inst.ID, "backoff", backoff, "attempt", attempts)
		err := inst.Client.Connect()
		if err == nil {
			return
		}

		slog.Warn("reconnect failed", "instance", inst.ID, "error", err, "attempt", attempts)

		// Alert CRM when reconnection keeps failing
		if attempts == maxAttempts {
			m.webhook.Send(webhook.Event{
				Type:       "connection.alert",
				InstanceID: inst.ID,
				Data: map[string]any{
					"status":         "reconnect_failed",
					"attempts":       attempts,
					"organizationId": inst.OrgID,
					"message":        "Instance failed to reconnect after multiple attempts. Manual intervention may be required.",
				},
			})
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// DisconnectAll gracefully disconnects all active WhatsApp clients.
func (m *Manager) DisconnectAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.clients))
	for id := range m.clients {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	for _, id := range ids {
		m.mu.Lock()
		inst, ok := m.clients[id]
		m.mu.Unlock()
		if ok {
			slog.Info("disconnecting instance", "id", id)
			inst.cancelFunc()
			inst.Client.Disconnect()
		}
	}
}

func (m *Manager) setStatus(inst *Instance, status string) {
	inst.Status = status
	if err := m.db.UpdateInstanceStatus(inst.ID, status); err != nil {
		slog.Error("failed to update instance status", "error", err)
	}

	m.webhook.Send(webhook.Event{
		Type:       "connection.update",
		InstanceID: inst.ID,
		Data: map[string]string{
			"status":         status,
			"organizationId": inst.OrgID,
		},
	})
}

func (m *Manager) handleEvent(inst *Instance, rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.Connected:
		slog.Info("connected", "instance", inst.ID)
		m.setStatus(inst, "connected")
		if inst.Client.Store.ID != nil {
			jid := inst.Client.Store.ID.String()
			phone := inst.Client.Store.ID.User
			m.db.UpdateInstanceJID(inst.ID, jid, phone)
		}

	case *events.Disconnected:
		slog.Warn("disconnected", "instance", inst.ID)
		m.setStatus(inst, "disconnected")

	case *events.LoggedOut:
		slog.Warn("logged out", "instance", inst.ID, "reason", evt.Reason)
		m.setStatus(inst, "logged_out")

	case *events.TemporaryBan:
		slog.Error("temporary ban", "instance", inst.ID, "code", evt.Code, "expire", evt.Expire)
		m.setStatus(inst, "banned")

	case *events.HistorySync:
		m.handleHistorySync(inst, evt)

	case *events.Message:
		m.handleMessage(inst, evt)

	case *events.Receipt:
		m.handleReceipt(inst, evt)

	case *events.PushName:
		m.webhook.Send(webhook.Event{
			Type:       "contact.update",
			InstanceID: inst.ID,
			Data: map[string]any{
				"jid":      evt.JID.String(),
				"pushName": evt.NewPushName,
			},
		})

	case *events.ChatPresence:
		m.webhook.Send(webhook.Event{
			Type:       "chat.presence",
			InstanceID: inst.ID,
			Data: map[string]any{
				"jid":   evt.MessageSource.Chat.String(),
				"state": string(evt.State),
			},
		})

	}
}

func (m *Manager) handleHistorySync(inst *Instance, evt *events.HistorySync) {
	data := evt.Data
	if data == nil {
		return
	}

	conversations := data.GetConversations()
	syncType := data.GetSyncType().String()
	slog.Info("history sync received",
		"instance", inst.ID,
		"syncType", syncType,
		"conversations", len(conversations),
	)

	inst.Sync.mu.Lock()
	inst.Sync.InProgress = true
	inst.Sync.SyncType = syncType
	inst.Sync.TotalConversations += int64(len(conversations))
	inst.Sync.mu.Unlock()

	var msgCount int64
	for _, conv := range conversations {
		remoteJid := conv.GetID()
		pushName := conv.GetDisplayName()
		if pushName == "" {
			pushName = conv.GetName()
		}

		messages := conv.GetMessages()
		if len(messages) == 0 {
			continue
		}

		batch := make([]map[string]any, 0, len(messages))
		for _, histMsg := range messages {
			wmi := histMsg.GetMessage()
			if wmi == nil {
				continue
			}

			info := wmi.GetKey()
			if info == nil {
				continue
			}

			msgData := map[string]any{
				"messageId": info.GetID(),
				"remoteJid": remoteJid,
				"pushName":  pushName,
				"fromMe":    info.GetFromMe(),
				"timestamp": wmi.GetMessageTimestamp(),
				"text":      extractText(wmi.GetMessage()),
				"mediaType": extractMediaType(wmi.GetMessage()),
			}
			batch = append(batch, msgData)
		}

		msgCount += int64(len(batch))

		if len(batch) > 0 {
			m.webhook.Send(webhook.Event{
				Type:       "history.sync",
				InstanceID: inst.ID,
				Data: map[string]any{
					"organizationId": inst.OrgID,
					"remoteJid":      remoteJid,
					"pushName":       pushName,
					"messages":       batch,
				},
			})
		}
	}

	inst.Sync.mu.Lock()
	inst.Sync.TotalMessages += msgCount
	inst.Sync.LastSyncAt = time.Now()
	inst.Sync.InProgress = false
	inst.Sync.mu.Unlock()
}

func (m *Manager) handleMessage(inst *Instance, evt *events.Message) {
	// Handle reaction messages separately
	if evt.Message.GetReactionMessage() != nil {
		rm := evt.Message.GetReactionMessage()
		reactedMsgID := ""
		remoteJid := evt.Info.Chat.String()
		if rm.GetKey() != nil {
			reactedMsgID = rm.GetKey().GetID()
			if rm.GetKey().GetRemoteJID() != "" {
				remoteJid = rm.GetKey().GetRemoteJID()
			}
		}
		m.webhook.Send(webhook.Event{
			Type:       "reaction",
			InstanceID: inst.ID,
			Data: map[string]any{
				"organizationId": inst.OrgID,
				"remoteJid":      remoteJid,
				"senderJid":      evt.Info.Sender.String(),
				"messageId":      reactedMsgID,
				"reaction":       rm.GetText(),
				"timestamp":      evt.Info.Timestamp.Unix(),
				"fromMe":         evt.Info.IsFromMe,
			},
		})
		return
	}

	text := extractText(evt.Message)
	mediaType := extractMediaType(evt.Message)

	data := map[string]any{
		"organizationId": inst.OrgID,
		"messageId":      evt.Info.ID,
		"remoteJid":      evt.Info.Chat.String(),
		"senderJid":      evt.Info.Sender.String(),
		"pushName":       evt.Info.PushName,
		"fromMe":         evt.Info.IsFromMe,
		"timestamp":      evt.Info.Timestamp.Unix(),
		"text":           text,
		"mediaType":      mediaType,
		"isGroup":        evt.Info.IsGroup,
	}

	// Download media and include as base64 data URI
	if mediaType != "" {
		mediaData, mimetype := downloadMediaFromMessage(inst.Client, evt.Message)
		if mediaData != "" {
			data["mediaBase64"] = mediaData
			data["mediaMimetype"] = mimetype
		}
	}

	m.webhook.Send(webhook.Event{
		Type:       "message",
		InstanceID: inst.ID,
		Data:       data,
	})
}

func (m *Manager) handleReceipt(inst *Instance, evt *events.Receipt) {
	m.webhook.Send(webhook.Event{
		Type:       "receipt",
		InstanceID: inst.ID,
		Data: map[string]any{
			"messageIds": evt.MessageIDs,
			"type":       string(evt.Type),
			"timestamp":  evt.Timestamp.Unix(),
			"from":       evt.MessageSource.Chat.String(),
		},
	})
}

// SendText sends a text message.
func (m *Manager) SendText(instanceID string, jid types.JID, text string) (whatsmeow.SendResponse, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return whatsmeow.SendResponse{}, fmt.Errorf("instance %s not found", instanceID)
	}

	msg := &waProto.Message{
		Conversation: &text,
	}

	return inst.Client.SendMessage(context.Background(), jid, msg)
}

// SendMedia uploads and sends a media message.
func (m *Manager) SendMedia(instanceID string, jid types.JID, fileBytes []byte, mimetype string, caption string, fileName string) (whatsmeow.SendResponse, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return whatsmeow.SendResponse{}, fmt.Errorf("instance %s not found", instanceID)
	}

	// Determine media type from MIME
	var mediaType whatsmeow.MediaType
	switch {
	case strings.HasPrefix(mimetype, "image/"):
		mediaType = whatsmeow.MediaImage
	case strings.HasPrefix(mimetype, "video/"):
		mediaType = whatsmeow.MediaVideo
	case strings.HasPrefix(mimetype, "audio/"):
		mediaType = whatsmeow.MediaAudio
	default:
		mediaType = whatsmeow.MediaDocument
	}

	resp, err := inst.Client.Upload(context.Background(), fileBytes, mediaType)
	if err != nil {
		return whatsmeow.SendResponse{}, fmt.Errorf("upload failed: %w", err)
	}

	var msg *waProto.Message
	fileLen := uint64(len(fileBytes))

	switch mediaType {
	case whatsmeow.MediaImage:
		msg = &waProto.Message{
			ImageMessage: &waProto.ImageMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mimetype),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    proto.Uint64(fileLen),
			},
		}
	case whatsmeow.MediaVideo:
		msg = &waProto.Message{
			VideoMessage: &waProto.VideoMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mimetype),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    proto.Uint64(fileLen),
			},
		}
	case whatsmeow.MediaAudio:
		msg = &waProto.Message{
			AudioMessage: &waProto.AudioMessage{
				Mimetype:      proto.String(mimetype),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    proto.Uint64(fileLen),
			},
		}
	default:
		msg = &waProto.Message{
			DocumentMessage: &waProto.DocumentMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mimetype),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    proto.Uint64(fileLen),
				FileName:      proto.String(fileName),
			},
		}
	}

	return inst.Client.SendMessage(context.Background(), jid, msg)
}

// MarkRead marks messages as read.
func (m *Manager) MarkRead(instanceID string, chat types.JID, messageIDs []types.MessageID) error {
	inst, ok := m.Get(instanceID)
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}
	return inst.Client.MarkRead(context.Background(), messageIDs, time.Now(), chat, types.EmptyJID)
}

// SendReaction sends a reaction to a message.
func (m *Manager) SendReaction(instanceID string, chat types.JID, remoteJid string, messageId string, reaction string) (whatsmeow.SendResponse, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return whatsmeow.SendResponse{}, fmt.Errorf("instance %s not found", instanceID)
	}

	msg := &waProto.Message{
		ReactionMessage: &waProto.ReactionMessage{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(remoteJid),
				ID:        proto.String(messageId),
				FromMe:    proto.Bool(true),
			},
			Text:              proto.String(reaction),
			SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
		},
	}

	return inst.Client.SendMessage(context.Background(), chat, msg)
}

// GetContacts returns all contacts from the device store.
func (m *Manager) GetContacts(instanceID string) ([]map[string]any, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance %s not found", instanceID)
	}

	contacts, err := inst.Client.Store.Contacts.GetAllContacts(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get contacts: %w", err)
	}

	result := make([]map[string]any, 0, len(contacts))
	for jid, info := range contacts {
		result = append(result, map[string]any{
			"jid":          jid.String(),
			"phone":        jid.User,
			"pushName":     info.PushName,
			"businessName": info.BusinessName,
			"fullName":     info.FullName,
		})
	}
	return result, nil
}

// GetGroups returns all joined groups with metadata.
func (m *Manager) GetGroups(instanceID string) ([]map[string]any, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance %s not found", instanceID)
	}

	groups, err := inst.Client.GetJoinedGroups(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get groups: %w", err)
	}

	result := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		result = append(result, map[string]any{
			"jid":          g.JID.String(),
			"name":         g.Name,
			"topic":        g.Topic,
			"participants": len(g.Participants),
			"isLocked":     g.IsLocked,
			"isAnnounce":   g.IsAnnounce,
		})
	}
	return result, nil
}

// GetGroupInfo returns detailed info for a specific group.
func (m *Manager) GetGroupInfo(instanceID string, groupJID types.JID) (map[string]any, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance %s not found", instanceID)
	}

	info, err := inst.Client.GetGroupInfo(context.Background(), groupJID)
	if err != nil {
		return nil, fmt.Errorf("get group info: %w", err)
	}

	participants := make([]map[string]string, 0, len(info.Participants))
	for _, p := range info.Participants {
		role := "member"
		if p.IsAdmin {
			role = "admin"
		}
		if p.IsSuperAdmin {
			role = "superadmin"
		}
		participants = append(participants, map[string]string{
			"jid":  p.JID.String(),
			"role": role,
		})
	}

	return map[string]any{
		"jid":          info.JID.String(),
		"name":         info.Name,
		"topic":        info.Topic,
		"participants": participants,
		"isLocked":     info.IsLocked,
		"isAnnounce":   info.IsAnnounce,
		"owner":        info.OwnerJID.String(),
		"created":      info.GroupCreated.Unix(),
	}, nil
}

// GetProfilePic returns the profile picture URL for a JID.
func (m *Manager) GetProfilePic(instanceID string, jid types.JID) (string, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return "", fmt.Errorf("instance %s not found", instanceID)
	}

	pic, err := inst.Client.GetProfilePictureInfo(context.Background(), jid, &whatsmeow.GetProfilePictureParams{Preview: false})
	if err != nil {
		return "", fmt.Errorf("get profile pic: %w", err)
	}
	if pic == nil {
		return "", nil
	}
	return pic.URL, nil
}

// GetSyncStats returns the history sync stats for an instance.
func (m *Manager) GetSyncStats(instanceID string) (map[string]any, error) {
	inst, ok := m.Get(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance %s not found", instanceID)
	}

	inst.Sync.mu.RLock()
	defer inst.Sync.mu.RUnlock()

	result := map[string]any{
		"totalMessages":      inst.Sync.TotalMessages,
		"totalConversations": inst.Sync.TotalConversations,
		"lastSyncType":       inst.Sync.SyncType,
		"inProgress":         inst.Sync.InProgress,
	}
	if !inst.Sync.LastSyncAt.IsZero() {
		result["lastSyncAt"] = inst.Sync.LastSyncAt.Unix()
	}
	return result, nil
}

// RequestHistorySync asks WhatsApp for older messages (on-demand sync).
func (m *Manager) RequestHistorySync(instanceID string, count int) error {
	inst, ok := m.Get(instanceID)
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	if !inst.Client.IsConnected() {
		return fmt.Errorf("instance %s is not connected", instanceID)
	}

	if count <= 0 {
		count = 50
	}

	inst.Sync.mu.Lock()
	inst.Sync.InProgress = true
	inst.Sync.mu.Unlock()

	// Request full recent history re-sync from WhatsApp
	inst.Client.SendMessage(context.Background(), inst.Client.Store.ID.ToNonAD(), &waProto.Message{
		ProtocolMessage: &waProto.ProtocolMessage{
			Type: waProto.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		},
	})

	return nil
}

func (m *Manager) getOrCreateDevice(id string) (*store.Device, error) {
	// Always create a fresh device for new instances.
	// Existing sessions are restored via RestoreInstances at startup.
	return m.container.NewDevice(), nil
}

// RestoreInstances loads persisted instances from the gateway DB and
// reconnects them using existing whatsmeow sessions.
func (m *Manager) RestoreInstances() {
	instances, err := m.db.ListAllInstances()
	if err != nil {
		slog.Error("failed to list instances for restore", "error", err)
		return
	}

	devices, err := m.container.GetAllDevices(context.Background())
	if err != nil {
		slog.Error("failed to get devices for restore", "error", err)
		return
	}

	if len(devices) == 0 || len(instances) == 0 {
		return
	}

	// For single-device setups, map the first logged-in device to the first instance
	// For multi-device, we match by stored JID
	deviceMap := make(map[string]*store.Device)
	for _, d := range devices {
		if d.ID != nil {
			deviceMap[d.ID.String()] = d
		}
	}

	for _, dbInst := range instances {
		// Skip if already loaded in memory
		if _, ok := m.clients[dbInst.ID]; ok {
			continue
		}

		var device *store.Device

		// Try to match by JID
		if dbInst.JID != "" {
			if d, ok := deviceMap[dbInst.JID]; ok {
				device = d
				delete(deviceMap, dbInst.JID) // don't reuse
			}
		}

		// Fallback: use first available logged-in device
		if device == nil {
			for jid, d := range deviceMap {
				device = d
				delete(deviceMap, jid)
				break
			}
		}

		if device == nil {
			slog.Info("no device found for instance, skipping restore", "instance", dbInst.ID, "name", dbInst.Name)
			continue
		}

		client := whatsmeow.NewClient(device, nil)
		ctx, cancel := context.WithCancel(context.Background())

		inst := &Instance{
			Client:        client,
			ID:            dbInst.ID,
			Name:          dbInst.Name,
			OrgID:         dbInst.OrganizationID,
			Status:        "disconnected",
			cancelFunc:    cancel,
			qrSubscribers: make(map[*qrSubscriber]struct{}),
		}

		m.mu.Lock()
		m.clients[dbInst.ID] = inst
		m.mu.Unlock()

		client.AddEventHandler(func(evt interface{}) {
			m.handleEvent(inst, evt)
		})

		go m.connect(ctx, inst)
		slog.Info("restored instance", "id", dbInst.ID, "name", dbInst.Name)
	}
}

// --- Text/Media extraction helpers ---

func extractText(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Conversation != nil {
		return *msg.Conversation
	}
	if msg.ExtendedTextMessage != nil && msg.ExtendedTextMessage.Text != nil {
		return *msg.ExtendedTextMessage.Text
	}
	if msg.ImageMessage != nil {
		c := msg.ImageMessage.Caption
		if c != nil {
			return "[Imagem] " + *c
		}
		return "[Imagem]"
	}
	if msg.VideoMessage != nil {
		c := msg.VideoMessage.Caption
		if c != nil {
			return "[Vídeo] " + *c
		}
		return "[Vídeo]"
	}
	if msg.DocumentMessage != nil {
		f := msg.DocumentMessage.FileName
		if f != nil {
			return "[Documento] " + *f
		}
		return "[Documento]"
	}
	if msg.AudioMessage != nil {
		return "[Áudio]"
	}
	if msg.StickerMessage != nil {
		return "[Figurinha]"
	}
	if msg.LocationMessage != nil {
		return "[Localização]"
	}
	if msg.ContactMessage != nil {
		return "[Contato]"
	}
	return ""
}

func extractMediaType(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	if msg.ImageMessage != nil {
		return "image"
	}
	if msg.VideoMessage != nil {
		return "video"
	}
	if msg.DocumentMessage != nil {
		return "document"
	}
	if msg.AudioMessage != nil {
		return "audio"
	}
	if msg.StickerMessage != nil {
		return "sticker"
	}
	return ""
}

// downloadMediaFromMessage downloads media from a WhatsApp message and returns
// a base64 data URI string and the mimetype. Returns empty strings on failure.
func downloadMediaFromMessage(client *whatsmeow.Client, msg *waProto.Message) (string, string) {
	if msg == nil || client == nil {
		return "", ""
	}

	var downloadable whatsmeow.DownloadableMessage
	var mimetype string

	switch {
	case msg.ImageMessage != nil:
		downloadable = msg.ImageMessage
		mimetype = msg.ImageMessage.GetMimetype()
	case msg.VideoMessage != nil:
		downloadable = msg.VideoMessage
		mimetype = msg.VideoMessage.GetMimetype()
	case msg.AudioMessage != nil:
		downloadable = msg.AudioMessage
		mimetype = msg.AudioMessage.GetMimetype()
	case msg.DocumentMessage != nil:
		downloadable = msg.DocumentMessage
		mimetype = msg.DocumentMessage.GetMimetype()
	case msg.StickerMessage != nil:
		downloadable = msg.StickerMessage
		mimetype = msg.StickerMessage.GetMimetype()
	default:
		return "", ""
	}

	if mimetype == "" {
		mimetype = "application/octet-stream"
	}

	data, err := client.Download(context.Background(), downloadable)
	if err != nil {
		slog.Warn("failed to download media", "error", err)
		return "", ""
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimetype, b64)
	return dataURI, mimetype
}
