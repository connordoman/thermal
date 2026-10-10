package settings

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/connordoman/thermal/internal/auth"
	"github.com/connordoman/thermal/internal/render"
)

const (
	EnvThermalAddr     = "THERMAL_ADDR"      // listen address, default :8080
	EnvThermalDB       = "THERMAL_DB"        // SQLite file, default thermal.db
	EnvThermalMaxBody  = "THERMAL_MAX_BODY"  // request body limit in bytes, default 16 MiB
	EnvThermalRetain   = "THERMAL_RETENTION" // how long job bodies are kept, default 720h; 0 keeps them forever
	EnvThermalDebug    = "THERMAL_DEBUG"     // verbose logging and Gin debug mode
	EnvThermalCutFeed  = "THERMAL_CUT_FEED"  // dots fed past the cut position before cutting, default 96 (12 mm)
	EnvImageAllowLocal = "THERMAL_IMAGE_ALLOW_PRIVATE_HOSTS"
	EnvImageMaxBytes   = "THERMAL_IMAGE_MAX_BYTES"
	EnvImageTimeout    = "THERMAL_IMAGE_TIMEOUT"

	// User accounts and sessions.
	EnvSessionTTL       = "THERMAL_SESSION_TTL"        // how long a sign-in lasts, default 24h
	EnvSessionSecure    = "THERMAL_SESSION_SECURE"     // send the session cookie only over HTTPS, default false
	EnvTrustedOrigins   = "THERMAL_TRUSTED_ORIGINS"    // comma-separated origins allowed to make cookie-authenticated requests cross-origin
	EnvRootUser         = "THERMAL_ROOT_USER"          // the root user's name, default root; used when the root user is created
	EnvRootPassword     = "THERMAL_ROOT_PASSWORD"      // the root user's initial password; random and printed once if unset
	EnvRootPasswordFile = "THERMAL_ROOT_PASSWORD_FILE" // read the initial password from this file instead (e.g. a Docker secret)
)

const (
	// EnvUpsideDown prints jobs upside down unless a request says
	// otherwise. Default false.
	EnvUpsideDown = "THERMAL_UPSIDE_DOWN"
	// EnvUpsideDownPage is the tallest page, in dots, an upside-down job
	// prints at once. Default 1024.
	EnvUpsideDownPage = "THERMAL_UPSIDE_DOWN_PAGE_HEIGHT"
)

type ServerConfig struct {
	Addr         string
	DBPath       string
	MaxBodyBytes int64
	Retention    time.Duration
	Debug        bool
	// CutFeed is the paper, in dots, fed past the last line before a cut,
	// unless a request sets its own.
	CutFeed uint8
	// UpsideDown is the default for print requests' upside_down, and
	// UpsideDownPageHeight the tallest page mode area those jobs use.
	UpsideDown           bool
	UpsideDownPageHeight int

	// Images fetched by URL for markdown and JSON jobs.
	ImageAllowPrivateHosts bool
	ImageMaxBytes          int64
	ImageTimeout           time.Duration

	SessionTTL     time.Duration
	SessionSecure  bool
	TrustedOrigins []string
	// RootUser and RootPassword apply only when the root user is created,
	// on first start.
	RootUser     string
	RootPassword string
}

func NewServerConfig() *ServerConfig {
	return &ServerConfig{}
}

func (s *ServerConfig) Load() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	s.Addr = envString(EnvThermalAddr, ":8080")
	s.DBPath = envString(EnvThermalDB, "thermal.db")
	s.MaxBodyBytes, err = envInt(EnvThermalMaxBody, 16<<20)
	add(err)
	s.Retention, err = envDuration(EnvThermalRetain, 30*24*time.Hour)
	add(err)
	s.Debug, err = envBool(EnvThermalDebug, false)
	add(err)
	feed, err := envInt(EnvThermalCutFeed, 120)
	add(err)
	if feed < 0 || feed > 255 {
		add(configError(EnvThermalCutFeed, "must be 0\u2013255 dots"))
	}
	s.CutFeed = uint8(min(max(feed, 0), 255))
	s.UpsideDown, err = envBool(EnvUpsideDown, false)
	add(err)
	page, err := envInt(EnvUpsideDownPage, render.DefaultPageHeight)
	add(err)
	s.UpsideDownPageHeight = int(page)
	s.ImageAllowPrivateHosts, err = envBool(EnvImageAllowLocal, false)
	add(err)
	s.ImageMaxBytes, err = envInt(EnvImageMaxBytes, 10<<20)
	add(err)
	s.ImageTimeout, err = envDuration(EnvImageTimeout, 10*time.Second)
	add(err)
	s.SessionTTL, err = envDuration(EnvSessionTTL, 24*time.Hour)
	add(err)
	s.SessionSecure, err = envBool(EnvSessionSecure, false)
	add(err)
	s.TrustedOrigins = nil
	for o := range strings.SplitSeq(envString(EnvTrustedOrigins, ""), ",") {
		if o = strings.TrimSpace(o); o != "" {
			s.TrustedOrigins = append(s.TrustedOrigins, strings.TrimSuffix(o, "/"))
		}
	}
	s.RootUser = envString(EnvRootUser, "root")
	s.RootPassword = os.Getenv(EnvRootPassword)
	if f := envString(EnvRootPasswordFile, ""); f != "" {
		if s.RootPassword != "" {
			add(configError(EnvRootPasswordFile, "set "+EnvRootPassword+" or "+EnvRootPasswordFile+", not both"))
		}
		b, err := os.ReadFile(f)
		if err != nil {
			add(configError(EnvRootPasswordFile, err.Error()))
		}
		s.RootPassword = strings.TrimRight(string(b), "\r\n")
	}
	return errors.Join(errs...)
}

func (s *ServerConfig) Validate() error {
	var errs []error
	if s.MaxBodyBytes < 1024 {
		errs = append(errs, configError(EnvThermalMaxBody, "must be at least 1024"))
	}
	if s.ImageMaxBytes < 1024 {
		errs = append(errs, configError(EnvImageMaxBytes, "must be at least 1024"))
	}
	if s.ImageTimeout <= 0 {
		errs = append(errs, configError(EnvImageTimeout, "must be positive"))
	}
	if s.SessionTTL < time.Minute {
		errs = append(errs, configError(EnvSessionTTL, "must be at least 1m"))
	}
	if s.RootPassword != "" && auth.CheckPassword(s.RootPassword) != nil {
		errs = append(errs, configError(EnvRootPassword, auth.ErrWeakPassword.Error()))
	}
	if _, err := auth.NormalizeUsername(s.RootUser); err != nil {
		errs = append(errs, configError(EnvRootUser, err.Error()))
	}
	for _, o := range s.TrustedOrigins {
		if u, err := url.Parse(o); err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			errs = append(errs, configError(EnvTrustedOrigins, fmt.Sprintf("%q is not an origin such as https://thermal.example.com", o)))
		}
	}
	if s.UpsideDownPageHeight < 64 || s.UpsideDownPageHeight > 65535 {
		errs = append(errs, configError(EnvUpsideDownPage, "must be 64–65535 dots"))
	}
	if s.Retention < 0 {
		errs = append(errs, configError(EnvThermalRetain, "must not be negative"))
	}
	return errors.Join(errs...)
}
