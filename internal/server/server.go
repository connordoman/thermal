// Package server exposes the print server's HTTP API with Gin.
package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/connordoman/thermal/internal/auth"
	"github.com/connordoman/thermal/internal/console"
	"github.com/connordoman/thermal/internal/device"
	"github.com/connordoman/thermal/internal/queue"
	"github.com/connordoman/thermal/internal/render"
	"github.com/connordoman/thermal/internal/store"
	"github.com/gin-gonic/gin"
)

// Version is reported by /healthz.
var Version = "dev"

// Server holds the API's dependencies.
type Server struct {
	Store        *store.Store
	Keys         *auth.Manager
	Device       *device.Device
	Queue        *queue.Queue
	Images       *render.ImageLoader
	MaxBodyBytes int64
	Debug        bool
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	if !s.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	// Clients connect directly; never trust X-Forwarded-For.
	_ = r.SetTrustedProxies(nil)
	r.Use(gin.CustomRecovery(func(c *gin.Context, err any) {
		console.Error("panic serving %s %s: %v", c.Request.Method, c.Request.URL.Path, err)
		abort(c, http.StatusInternalServerError, "internal", "internal server error")
	}))
	r.Use(s.logRequests)
	r.Use(s.limitBody)
	r.HandleMethodNotAllowed = true
	r.NoRoute(func(c *gin.Context) { abort(c, http.StatusNotFound, "not_found", "no such endpoint") })
	r.NoMethod(func(c *gin.Context) {
		abort(c, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})

	r.GET("/", s.index)
	r.GET("/healthz", s.health)
	r.GET("/v1/schema/job.json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/schema+json", render.SchemaJSON)
	})

	v1 := r.Group("/v1", s.authenticate)
	v1.GET("/whoami", s.whoami)

	print := v1.Group("/print", require(auth.ScopePrint))
	print.POST("/raw", s.printRaw)
	print.POST("/text", s.printText)
	print.POST("/markdown", s.printMarkdown)
	print.POST("/utf8", s.printUnicode)
	print.POST("/json", s.printJSON)

	read := v1.Group("", require(auth.ScopeRead))
	read.GET("/printer", s.printerInfo)
	read.GET("/printer/status", s.printerStatus)
	read.GET("/queue", s.queueSummary)
	read.GET("/jobs", s.listJobs)
	read.GET("/jobs/:id", s.getJob)
	read.GET("/jobs/:id/payload", s.jobPayload)
	read.GET("/jobs/:id/source", s.jobSource)

	jobs := v1.Group("/jobs", require(auth.ScopePrint))
	jobs.POST("/:id/cancel", s.cancelJob)
	jobs.POST("/:id/retry", s.retryJob)

	admin := v1.Group("", require(auth.ScopeAdmin))
	admin.GET("/keys", s.listKeys)
	admin.POST("/keys", s.createKey)
	admin.GET("/keys/:id", s.getKey)
	admin.PATCH("/keys/:id", s.updateKey)
	admin.POST("/keys/:id/rotate", s.rotateKey)
	admin.DELETE("/keys/:id", s.revokeKey)
	admin.GET("/audit", s.listAudit)
	return r
}

func (s *Server) index(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name":    "thermal",
		"version": Version,
		"auth":    "send an API key as 'Authorization: Bearer thm_...' or 'X-API-Key: thm_...'",
		"schema":  "/v1/schema/job.json",
		"endpoints": []string{
			"POST /v1/print/raw", "POST /v1/print/text", "POST /v1/print/markdown",
			"POST /v1/print/utf8", "POST /v1/print/json",
			"GET /v1/printer", "GET /v1/printer/status", "GET /v1/queue",
			"GET /v1/jobs", "GET /v1/jobs/:id", "GET /v1/jobs/:id/payload", "GET /v1/jobs/:id/source",
			"POST /v1/jobs/:id/cancel", "POST /v1/jobs/:id/retry",
			"GET /v1/keys", "POST /v1/keys", "GET /v1/keys/:id", "PATCH /v1/keys/:id",
			"POST /v1/keys/:id/rotate", "DELETE /v1/keys/:id", "GET /v1/audit", "GET /v1/whoami",
		},
	})
}

func (s *Server) health(c *gin.Context) {
	if err := s.Store.DB.PingContext(c); err != nil {
		abort(c, http.StatusServiceUnavailable, "unhealthy", "database unavailable")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "version": Version, "queue": s.Queue.State().State})
}

func (s *Server) logRequests(c *gin.Context) {
	start := time.Now()
	c.Next()
	key := "-"
	if k := currentKey(c); k != nil {
		key = k.ID
	}
	msg := "%s %s %d %v key=%s ip=%s"
	args := []any{c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start).Round(time.Millisecond), key, c.ClientIP()}
	switch {
	case c.Writer.Status() >= 500:
		console.Error(msg, args...)
	case c.Request.URL.Path == "/healthz":
		console.Debug(msg, args...)
	default:
		console.Info(msg, args...)
	}
}

func (s *Server) limitBody(c *gin.Context) {
	if c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.MaxBodyBytes)
	}
	c.Next()
}

// apiError is the body of every error response.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details,omitempty"`
	} `json:"error"`
}

func abort(c *gin.Context, status int, code, message string) {
	abortWith(c, status, code, message, nil)
}

func abortWith(c *gin.Context, status int, code, message string, details any) {
	var e apiError
	e.Error.Code, e.Error.Message, e.Error.Details = code, message, details
	c.AbortWithStatusJSON(status, e)
}

const keyContext = "thermal.key"

func currentKey(c *gin.Context) *auth.Key {
	if v, ok := c.Get(keyContext); ok {
		return v.(*auth.Key)
	}
	return nil
}

func (s *Server) authenticate(c *gin.Context) {
	token := c.GetHeader("X-API-Key")
	if h := c.GetHeader("Authorization"); token == "" && h != "" {
		scheme, value, _ := strings.Cut(h, " ")
		if strings.EqualFold(scheme, "Bearer") {
			token = strings.TrimSpace(value)
		}
	}
	if token == "" {
		c.Header("WWW-Authenticate", `Bearer realm="thermal"`)
		abort(c, http.StatusUnauthorized, "unauthorized", "an API key is required")
		return
	}
	key, err := s.Keys.Verify(c, token)
	switch {
	case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, auth.ErrRevoked), errors.Is(err, auth.ErrExpired):
		console.Warn("rejected API key from %s: %v", c.ClientIP(), err)
		c.Header("WWW-Authenticate", `Bearer realm="thermal", error="invalid_token"`)
		abort(c, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	case err != nil:
		console.Error("verifying API key: %v", err)
		abort(c, http.StatusInternalServerError, "internal", "could not verify the API key")
		return
	}
	c.Set(keyContext, key)
	c.Next()
}

func require(scope auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		if k := currentKey(c); k == nil || !k.Allows(scope) {
			abort(c, http.StatusForbidden, "forbidden", "this API key lacks the "+string(scope)+" scope")
			return
		}
		c.Next()
	}
}

func (s *Server) whoami(c *gin.Context) {
	k := currentKey(c)
	c.JSON(http.StatusOK, gin.H{"id": k.ID, "name": k.Name, "scopes": k.Scopes})
}
