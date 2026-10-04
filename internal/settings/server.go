package settings

import (
	"errors"
	"time"
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

	// Images fetched by URL for markdown and JSON jobs.
	ImageAllowPrivateHosts bool
	ImageMaxBytes          int64
	ImageTimeout           time.Duration
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
	s.ImageAllowPrivateHosts, err = envBool(EnvImageAllowLocal, false)
	add(err)
	s.ImageMaxBytes, err = envInt(EnvImageMaxBytes, 10<<20)
	add(err)
	s.ImageTimeout, err = envDuration(EnvImageTimeout, 10*time.Second)
	add(err)
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
	if s.Retention < 0 {
		errs = append(errs, configError(EnvThermalRetain, "must not be negative"))
	}
	return errors.Join(errs...)
}
