package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type RouterConfig struct {
	Port        string
	UpstreamURL string
	Timeout     time.Duration
}

func NewRouterConfig() RouterConfig {
	cfg := RouterConfig{
		Port: "8080",
	}

	if portVal, exists := os.LookupEnv("port"); exists && len(portVal) > 0 {
		cfg.Port = portVal
	}

	if up, exists := os.LookupEnv("upstream_url"); exists && len(up) > 0 {
		if strings.HasSuffix(up, "/") == false {
			up = up + "/"
		}

		cfg.UpstreamURL = up
	}

	cfg.Timeout = parseIntOrDurationValue(os.Getenv("timeout"), time.Second*60)

	return cfg
}

func parseIntOrDurationValue(val string, fallback time.Duration) time.Duration {
	if len(val) > 0 {
		parsedVal, parseErr := strconv.Atoi(val)
		if parseErr == nil && parsedVal >= 0 {
			return time.Duration(parsedVal) * time.Second
		}
	}

	duration, durationErr := time.ParseDuration(val)
	if durationErr != nil {
		return fallback
	}
	return duration
}
