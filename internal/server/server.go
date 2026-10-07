// Package server exposes the print server's HTTP API with Gin.
package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/connordoman/escpos/css"
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
	Users        *auth.Users
	Device       *device.Device
	Queue        *queue.Queue
	Images       *render.ImageLoader
	MaxBodyBytes int64
	Debug        bool
	// CutFeed is the paper, in dots, fed past the last line before a cut.
	CutFeed uint8
	// UpsideDown is the default for print requests' upside_down, and
	// PageHeight the tallest page, in dots, upside-down jobs print at once.
	UpsideDown bool
	PageHeight int

	// SessionSecure marks the session cookie Secure (HTTPS only).
	SessionSecure bool
	// TrustedOrigins may make cookie-authenticated requests cross-origin,
	// e.g. a web UI served from another host.
	TrustedOrigins []string

	logins   loginThrottle
	csrfOnce sync.Once
	csrf     *http.CrossOriginProtection
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
	// Renders HTML like the printer, for previews of styled jobs.
	r.GET("/escpos.css", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/css; charset=utf-8", css.Stylesheet)
	})

	r.POST("/v1/session", s.createSession)
	v1 := r.Group("/v1", s.authenticate)
	v1.GET("/whoami", s.whoami)
	v1.GET("/session", s.getSession)
	v1.DELETE("/session", s.deleteSession)
	// Users change their own password; admins anyone's.
	v1.PUT("/users/:username/password", s.setPassword)

	print := v1.Group("/print", require(auth.ScopePrint))
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
	// Raw bytes can do anything the printer can, including reconfiguring
	// it, so they need an admin key.
	admin.POST("/print/raw", s.printRaw)
	admin.GET("/keys", s.listKeys)
	admin.POST("/keys", s.createKey)
	admin.GET("/keys/:id", s.getKey)
	admin.PATCH("/keys/:id", s.updateKey)
	admin.POST("/keys/:id/rotate", s.rotateKey)
	admin.DELETE("/keys/:id", s.revokeKey)
	admin.GET("/users", s.listUsers)
	admin.POST("/users", s.createUser)
	admin.GET("/users/:username", s.getUser)
	admin.PATCH("/users/:username", s.updateUser)
	admin.DELETE("/users/:username", s.disableUser)
	admin.GET("/audit", s.listAudit)
	return r
}

func (s *Server) index(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name":    "thermal",
		"version": Version,
		"auth":    "send an API key as 'Authorization: Bearer thm_...' or 'X-API-Key: thm_...', or sign in with POST /v1/session for a session cookie",
		"schema":  "/v1/schema/job.json",
		"css":     "/escpos.css",
		"endpoints": []string{
			"POST /v1/print/raw", "POST /v1/print/text", "POST /v1/print/markdown",
			"POST /v1/print/utf8", "POST /v1/print/json",
			"GET /v1/printer", "GET /v1/printer/status", "GET /v1/queue",
			"GET /v1/jobs", "GET /v1/jobs/:id", "GET /v1/jobs/:id/payload", "GET /v1/jobs/:id/source",
			"POST /v1/jobs/:id/cancel", "POST /v1/jobs/:id/retry",
			"GET /v1/keys", "POST /v1/keys", "GET /v1/keys/:id", "PATCH /v1/keys/:id",
			"POST /v1/keys/:id/rotate", "DELETE /v1/keys/:id", "GET /v1/audit", "GET /v1/whoami",
			"POST /v1/session", "GET /v1/session", "DELETE /v1/session",
			"GET /v1/users", "POST /v1/users", "GET /v1/users/:username", "PATCH /v1/users/:username",
			"DELETE /v1/users/:username", "PUT /v1/users/:username/password",
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
	who := "key=-"
	if p := currentPrincipal(c); p != nil && p.user != nil {
		who = "user=" + p.user.Username
	} else if p != nil {
		who = "key=" + p.keyID
	}
	msg := "%s %s %d %v %s ip=%s"
	args := []any{c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start).Round(time.Millisecond), who, c.ClientIP()}
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

func require(scope auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		if p := currentPrincipal(c); p == nil || !p.allows(scope) {
			who := "API key"
			if p != nil && p.user != nil {
				who = "user"
			}
			abort(c, http.StatusForbidden, "forbidden", "this "+who+" lacks the "+string(scope)+" scope")
			return
		}
		c.Next()
	}
}
