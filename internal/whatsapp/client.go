package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
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

// Instance wraps a whatsmeow client with gateway metadata.
type Instance struct {
	Client     *whatsmeow.Client
	ID         string
	Name       string
	OrgID      string
	QRChan     chan string // sends QR codes to API consumers
	Status     string
	cancelFunc context.CancelFunc
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
		Client:     client,
		ID:         id,
		Name:       name,
		OrgID:      orgID,
		QRChan:     make(chan string, 10),
		Status:     "disconnected",
		cancelFunc: cancel,
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

// Restart reconnects an existing instance without needing QR code.
func (m *Manager) Restart(id string) error {
	m.mu.Lock()
	inst, ok := m.clients[id]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}

	inst.Client.Disconnect()
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
			select {
			case inst.QRChan <- evt.Code:
			default:
			}
		case "success":
			slog.Info("QR code scanned successfully", "instance", inst.ID)
			return
		case "timeout":
			slog.Warn("QR code timeout", "instance", inst.ID)
			m.setStatus(inst, "qr_timeout")
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

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if inst.Client.IsConnected() {
			return
		}

		slog.Info("attempting reconnect", "instance", inst.ID, "backoff", backoff)
		err := inst.Client.Connect()
		if err == nil {
			return
		}

		slog.Warn("reconnect failed", "instance", inst.ID, "error", err)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
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
	slog.Info("history sync received",
		"instance", inst.ID,
		"syncType", data.GetSyncType(),
		"conversations", len(conversations),
	)

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
}

func (m *Manager) handleMessage(inst *Instance, evt *events.Message) {
	text := extractText(evt.Message)
	mediaType := extractMediaType(evt.Message)

	m.webhook.Send(webhook.Event{
		Type:       "message",
		InstanceID: inst.ID,
		Data: map[string]any{
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
		},
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

func (m *Manager) getOrCreateDevice(id string) (*store.Device, error) {
	devices, err := m.container.GetAllDevices(context.Background())
	if err != nil {
		return nil, err
	}

	// Try to find existing device for this instance
	for _, d := range devices {
		if d.ID != nil {
			return d, nil
		}
	}

	return m.container.NewDevice(), nil
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
