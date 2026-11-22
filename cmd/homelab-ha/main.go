// Command homelab-ha is the homelab's Home Assistant MQTT bridge. It runs
// in the `ha-bridge` stack and reads and controls the homelab through the daemon's
// stack API.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/homelab-ngx/stack-ha-bridge/internal/bridge"
	"github.com/homelab-ngx/stack-ha-bridge/internal/homelab"
)

// version is set by the Nix build to the repo's short commit.
var version = "dev"

// settings is everything the bridge is configured with, all from the
// environment. HOMELAB_API_URL and HOMELAB_API_TOKEN come from the env file
// the host writes into the stack (/var/lib/homelab-api/env) once the api
// grant is approved.
type settings struct {
	bridge   bridge.Config
	apiURL   string
	apiToken string
}

func load(getenv func(string) string) (settings, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	broker := getenv("MQTT_BROKER")
	if broker == "" {
		host := strings.TrimSpace(getenv("MQTT_HOST"))
		if host == "" {
			return settings{}, fmt.Errorf("MQTT_HOST (the broker's address) is not set")
		}
		if strings.ContainsAny(host, ", ") {
			return settings{}, fmt.Errorf("MQTT_HOST %q: give the broker's one address", host)
		}
		broker = "mqtt://" + host + ":" + or("MQTT_PORT", "1883")
	}
	s := settings{
		bridge: bridge.Config{
			BrokerURL:       broker,
			Username:        or("MQTT_USERNAME", "homelab"),
			PasswordFile:    getenv("MQTT_PASSWORD_FILE"),
			ClientID:        or("MQTT_CLIENT_ID", "homelab-ha"),
			DiscoveryPrefix: or("HA_DISCOVERY_PREFIX", "homeassistant"),
			// The host gives every stack its dashboard's address; without
			// it the device has no link.
			DashboardURL: getenv("HOMELAB_DASHBOARD_URL"),
			Rev:          version,
			HostRoot:     or("HOST_ROOT", "/"),
		},
		apiURL:   getenv("HOMELAB_API_URL"),
		apiToken: getenv("HOMELAB_API_TOKEN"),
	}
	if s.bridge.PasswordFile == "" {
		return s, fmt.Errorf("MQTT_PASSWORD_FILE is not set")
	}
	return s, nil
}

func main() {
	log.SetFlags(0) // journald stamps every line
	s, err := load(os.Getenv)
	if err != nil {
		slog.Error("homelab-ha", "error", err)
		os.Exit(2)
	}
	api, err := homelab.New(s.apiURL, s.apiToken, nil)
	if err != nil {
		slog.Error("homelab-ha", "error", err)
		os.Exit(2)
	}
	if !api.Configured() {
		// Keep running: MQTT and the host sensors still work, and the unit
		// restarts when the host writes the env file.
		slog.Warn("homelab API not configured; services and backups stay unavailable", "error", homelab.ErrNotConfigured)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("homelab-ha starting", "version", version, "broker", s.bridge.BrokerURL, "api", s.apiURL)
	if err := bridge.New(s.bridge, api).Run(ctx); err != nil {
		slog.Error("homelab-ha", "error", err)
		os.Exit(1)
	}
}
