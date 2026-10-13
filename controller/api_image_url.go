package controller

import (
	"errors"
	"mime"
	"net/http"
	"path/filepath"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// GetAPIImageFile serves an image through its unguessable, expiring bearer URL.
func GetAPIImageFile(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	file, err := service.OpenImageURLFile(c.Param("name"), time.Now().Unix())
	if err != nil {
		if errors.Is(err, service.ErrImageURLExpired) {
			c.AbortWithStatus(http.StatusGone)
		} else {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	extension := filepath.Ext(c.Param("name"))
	c.Header("Content-Type", mime.TypeByExtension(extension))
	http.ServeContent(c.Writer, c.Request, "image"+extension, info.ModTime(), file)
}
