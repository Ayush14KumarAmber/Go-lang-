package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"eth-explorer/internal/models"
)

// HealthHandler exposes liveness/readiness information about the service.
// It intentionally has no dependencies in this milestone; once the
// Ethereum client and Redis client exist, Health will be extended to
// report their connectivity too (see internal/service in a later
// milestone).
type HealthHandler struct {
	version string
}

// NewHealthHandler constructs a HealthHandler.
func NewHealthHandler(version string) *HealthHandler {
	return &HealthHandler{version: version}
}

// Health godoc
//
//	@Summary		Health check
//	@Description	Returns service status. Intended for load balancer / k8s liveness probes.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	models.APIResponse
//	@Router			/health [get]
func (h *HealthHandler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, models.Success(gin.H{
		"status":  "ok",
		"version": h.version,
	}))
}
