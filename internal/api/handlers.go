package api

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/types"

	"github.com/JeanZorzetti/whatsmeow-gateway/internal/store"
	"github.com/JeanZorzetti/whatsmeow-gateway/internal/whatsapp"
)

type Handler struct {
	manager            *whatsapp.Manager
	db                 *store.DB
	maxInstancesPerOrg int
}

func NewHandler(manager *whatsapp.Manager, db *store.DB, maxInstancesPerOrg int) *Handler {
	return &Handler{manager: manager, db: db, maxInstancesPerOrg: maxInstancesPerOrg}
}

func (h *Handler) RegisterRoutes(r *gin.Engine, apiKey string) {
	// Rate limiter: 100 requests per second per IP
	rl := NewRateLimiter(100, time.Second)

	r.GET("/metrics", AuthMiddleware(apiKey), h.Metrics)

	api := r.Group("/api", AuthMiddleware(apiKey), RateLimitMiddleware(rl))
	{
		api.POST("/instances", h.CreateInstance)
		api.GET("/instances", h.ListInstances)
		api.GET("/instances/:id", h.GetInstance)
		api.GET("/instances/:id/qr", h.GetQR)
		api.GET("/instances/:id/status", h.GetStatus)
		api.PUT("/instances/:id/restart", h.RestartInstance)
		api.DELETE("/instances/:id", h.DeleteInstance)

		api.POST("/instances/:id/messages/text", h.SendText)
		api.POST("/instances/:id/messages/media", h.SendMedia)
		api.POST("/instances/:id/messages/read", h.MarkRead)
		api.POST("/instances/:id/messages/reaction", h.SendReaction)

		api.POST("/instances/:id/sync/request", h.RequestSync)
		api.GET("/instances/:id/sync/status", h.GetSyncStatus)

		api.GET("/instances/:id/contacts", h.GetContacts)
		api.GET("/instances/:id/groups", h.GetGroups)
		api.GET("/instances/:id/groups/:jid", h.GetGroupInfo)
		api.GET("/instances/:id/profile-pic/:jid", h.GetProfilePic)
	}
}

func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

type CreateInstanceRequest struct {
	Name           string `json:"name" binding:"required"`
	OrganizationID string `json:"organizationId" binding:"required"`
}

func (h *Handler) CreateInstance(c *gin.Context) {
	var req CreateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check instance limit per organization
	count, err := h.db.CountInstancesByOrg(req.OrganizationID)
	if err == nil && count >= h.maxInstancesPerOrg {
		c.JSON(http.StatusForbidden, gin.H{
			"error": fmt.Sprintf("organization already has %d instances (limit: %d)", count, h.maxInstancesPerOrg),
		})
		return
	}

	id := uuid.New().String()

	// Save to DB
	inst, err := h.db.CreateInstance(id, req.Name, req.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create instance: " + err.Error()})
		return
	}

	// Start whatsmeow client
	_, err = h.manager.Create(id, req.Name, req.OrganizationID)
	if err != nil {
		h.db.DeleteInstance(id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start instance: " + err.Error()})
		return
	}

	c.JSON(http.StatusCreated, inst)
}

func (h *Handler) ListInstances(c *gin.Context) {
	orgID := c.Query("organizationId")
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organizationId query param required"})
		return
	}

	instances, err := h.db.ListInstances(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if instances == nil {
		instances = []store.Instance{}
	}

	c.JSON(http.StatusOK, instances)
}

func (h *Handler) GetInstance(c *gin.Context) {
	id := c.Param("id")
	inst, err := h.db.GetInstance(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
		return
	}

	c.JSON(http.StatusOK, inst)
}

func (h *Handler) GetQR(c *gin.Context) {
	id := c.Param("id")
	inst, ok := h.manager.Get(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
		return
	}

	// Subscribe to QR broadcast (gets cached QR immediately + future updates)
	qrCh, unsub := inst.SubscribeQR()
	defer unsub()

	// SSE: stream QR codes as they arrive
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, _ := c.Writer.(http.Flusher)

	for {
		select {
		case code, ok := <-qrCh:
			if !ok {
				return
			}
			c.SSEvent("qr", code)
			if flusher != nil {
				flusher.Flush()
			}
		case <-c.Request.Context().Done():
			return
		}
	}
}

func (h *Handler) GetStatus(c *gin.Context) {
	id := c.Param("id")

	inst, ok := h.manager.Get(id)
	if !ok {
		// Try DB
		dbInst, err := h.db.GetInstance(id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "instance not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": dbInst.Status, "connected": false})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":    inst.Status,
		"connected": inst.Client.IsConnected(),
	})
}

func (h *Handler) RestartInstance(c *gin.Context) {
	id := c.Param("id")
	if err := h.manager.Restart(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "restarting"})
}

func (h *Handler) DeleteInstance(c *gin.Context) {
	id := c.Param("id")

	h.manager.Remove(id)
	h.db.DeleteInstance(id)

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

type SendTextRequest struct {
	Number string `json:"number" binding:"required"`
	Text   string `json:"text" binding:"required"`
}

func (h *Handler) SendText(c *gin.Context) {
	id := c.Param("id")
	var req SendTextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	jid, _ := types.ParseJID(req.Number + "@s.whatsapp.net")

	resp, err := h.manager.SendText(id, jid, req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"messageId": resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	})
}

func (h *Handler) SendMedia(c *gin.Context) {
	id := c.Param("id")

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}
	defer file.Close()

	number := c.Request.FormValue("number")
	if number == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "number is required"})
		return
	}

	caption := c.Request.FormValue("caption")
	fileName := c.Request.FormValue("fileName")
	if fileName == "" {
		fileName = header.Filename
	}

	fileBytes, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read file"})
		return
	}

	mimetype := header.Header.Get("Content-Type")
	if mimetype == "" {
		mimetype = "application/octet-stream"
	}

	jid, _ := types.ParseJID(number + "@s.whatsapp.net")

	resp, err := h.manager.SendMedia(id, jid, fileBytes, mimetype, caption, fileName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"messageId": resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	})
}

type MarkReadRequest struct {
	RemoteJid  string   `json:"remoteJid" binding:"required"`
	MessageIDs []string `json:"messageIds" binding:"required"`
}

func (h *Handler) MarkRead(c *gin.Context) {
	id := c.Param("id")
	var req MarkReadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	chat, _ := types.ParseJID(req.RemoteJid)

	msgIDs := make([]types.MessageID, len(req.MessageIDs))
	for i, mid := range req.MessageIDs {
		msgIDs[i] = types.MessageID(mid)
	}

	if err := h.manager.MarkRead(id, chat, msgIDs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

type SendReactionRequest struct {
	RemoteJid string `json:"remoteJid" binding:"required"`
	MessageId string `json:"messageId" binding:"required"`
	Reaction  string `json:"reaction" binding:"required"`
}

func (h *Handler) SendReaction(c *gin.Context) {
	id := c.Param("id")
	var req SendReactionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	chat, _ := types.ParseJID(req.RemoteJid)

	resp, err := h.manager.SendReaction(id, chat, req.RemoteJid, req.MessageId, req.Reaction)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"messageId": resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	})
}

func (h *Handler) RequestSync(c *gin.Context) {
	id := c.Param("id")

	type syncReq struct {
		Count int `json:"count"`
	}
	var req syncReq
	c.ShouldBindJSON(&req)

	if err := h.manager.RequestHistorySync(id, req.Count); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "sync_requested"})
}

func (h *Handler) GetSyncStatus(c *gin.Context) {
	id := c.Param("id")

	stats, err := h.manager.GetSyncStats(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, stats)
}

func (h *Handler) GetContacts(c *gin.Context) {
	id := c.Param("id")
	contacts, err := h.manager.GetContacts(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, contacts)
}

func (h *Handler) GetGroups(c *gin.Context) {
	id := c.Param("id")
	groups, err := h.manager.GetGroups(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, groups)
}

func (h *Handler) GetGroupInfo(c *gin.Context) {
	id := c.Param("id")
	jidStr := c.Param("jid")

	jid, err := types.ParseJID(jidStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JID"})
		return
	}

	info, err := h.manager.GetGroupInfo(id, jid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, info)
}

func (h *Handler) GetProfilePic(c *gin.Context) {
	id := c.Param("id")
	jidStr := c.Param("jid")

	jid, err := types.ParseJID(jidStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JID"})
		return
	}

	url, err := h.manager.GetProfilePic(id, jid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}
