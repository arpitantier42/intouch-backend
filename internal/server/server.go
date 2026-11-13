// Package server builds the HTTP router for the InTouch API.
package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// New returns the Gin engine with every route registered.
func New() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return r
}
