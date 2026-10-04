package settings

import (
	"os"
	"strconv"
	"strings"
	"time"
)

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// parseHexId reads a 16-bit hex ID from the environment, with or without a
// 0x prefix. An unset variable is zero.
func parseHexId(key string) (uint16, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, nil
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X")
	n, err := strconv.ParseUint(v, 16, 16)
	if err != nil {
		return 0, configError(key, "not a 16-bit hex ID: "+err.Error())
	}
	return uint16(n), nil
}

func envInt(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, configError(key, "not an integer: "+err.Error())
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, configError(key, "not a boolean: "+err.Error())
	}
	return b, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, configError(key, "not a duration: "+err.Error())
	}
	return d, nil
}
