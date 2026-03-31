package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/types"

	"github.com/JeanZorzetti/whatsmeow-gateway/internal/store"
	"github.com/JeanZorzetti/whatsmeow-gateway/internal/whatsapp"
)

type Handler struct {
	manager *whatsapp.Manager
	db      *store.DB
}

func NewHandler(manager *whatsapp.Manager, db *store.DB) *Handler {
	return &Handler{manager: manager, db: db}
}

func (h *Handler) RegisterRoutes(r *gin.Engine, apiKey string) {
	r.GET("/health", h.Health)

	api := r.Group("/api", AuthMiddleware(apiKey))
	{
		api.POST("/instances", h.CreateInstance)
		api.GET("/instances", h.ListInstances)
		api.GET("/instances/:id", h.GetInstance)
		api.GET("/instances/:id/qr", h.GetQR)
		api.GET("/instances/:id/status", h.GetStatus)
		api.PUT("/instances/:id/restart", h.RestartInstance)
		api.DELETE("/instances/:id", h.DeleteInstance)

		api.POST("/instances/:id/messages/text", h.SendText)
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

	// SSE: stream QR codes as they arrive
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, _ := c.Writer.(http.Flusher)

	for {
		select {
		case code, ok := <-inst.QRChan:
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
