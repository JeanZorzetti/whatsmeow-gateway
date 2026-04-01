package api

import (
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

var startTime = time.Now()

func (h *Handler) Metrics(c *gin.Context) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Count connected instances
	instances := h.manager.ListAll()
	connected := 0
	for _, inst := range instances {
		if inst.Status == "connected" {
			connected++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"uptime":              time.Since(startTime).String(),
		"uptimeSeconds":       int(time.Since(startTime).Seconds()),
		"totalInstances":      len(instances),
		"connectedInstances":  connected,
		"goroutines":          runtime.NumGoroutine(),
		"memoryAllocMB":       float64(mem.Alloc) / 1024 / 1024,
		"memorySysMB":         float64(mem.Sys) / 1024 / 1024,
		"gcCycles":            mem.NumGC,
	})
}
