package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"HA-Cupbearer/internal/discovery"

	"github.com/gin-gonic/gin"
)

func (h *Handler) GetDeployDiscovery(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	report, warnings := h.orch.Discover(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "deployment discovery fetched",
		"data":    gin.H{"report": report, "warnings": warnings},
	})
}

func (h *Handler) PostDeployDiscoveryImport(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	var input discovery.ImportRequest
	if err := decoder.Decode(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid discovery import payload"})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid discovery import payload"})
		return
	}
	result, err := h.orch.ImportDiscovered(c.Request.Context(), input)
	if err != nil {
		var validationErr *discovery.ValidationError
		if errors.As(err, &validationErr) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false, "message": validationErr.Message,
				"data": gin.H{"field": validationErr.Field},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "failed to import discovered projects: " + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"success": true, "message": "discovered projects imported", "data": result,
	})
}
