package main

import "testing"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	// What the unit sets plus what the host's env file
	// (/var/lib/homelab-api/env) adds.
	s, err := load(env(map[string]string{
		"MQTT_PASSWORD_FILE":    "/run/credentials/homelab-ha.service/mqtt-password",
		"MQTT_HOST":             "192.0.2.10",
		"HOMELAB_DASHBOARD_URL": "https://homelab.example.com",
		"HOMELAB_API_URL":       "http://10.99.7.1:7781/api",
		"HOMELAB_API_TOKEN":     "tok",
	}))
	if err != nil {
		t.Fatal(err)
	}
	b := s.bridge
	if b.BrokerURL != "mqtt://192.0.2.10:1883" || b.Username != "homelab" || b.ClientID != "homelab-ha" ||
		b.DiscoveryPrefix != "homeassistant" || b.DashboardURL != "https://homelab.example.com" ||
		b.PasswordFile != "/run/credentials/homelab-ha.service/mqtt-password" || b.HostRoot != "/" {
		t.Fatalf("bridge config = %+v", b)
	}
	if s.apiURL != "http://10.99.7.1:7781/api" || s.apiToken != "tok" {
		t.Fatalf("api = %q %q", s.apiURL, s.apiToken)
	}
}

func TestLoadOverridesAndMissingAPI(t *testing.T) {
	t.Parallel()
	s, err := load(env(map[string]string{
		"MQTT_PASSWORD_FILE": "/p", "MQTT_BROKER": "mqtt://broker:1884", "MQTT_CLIENT_ID": "x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.bridge.BrokerURL != "mqtt://broker:1884" || s.bridge.ClientID != "x" {
		t.Fatalf("overrides = %+v", s.bridge)
	}
	if s.apiURL != "" || s.apiToken != "" {
		t.Fatal("API settings invented")
	}
}

func TestLoadNeedsPasswordFile(t *testing.T) {
	t.Parallel()
	if _, err := load(env(map[string]string{"MQTT_HOST": "192.0.2.10"})); err == nil {
		t.Fatal("no MQTT_PASSWORD_FILE accepted")
	}
}

func TestLoadNeedsOneBroker(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"", "192.0.2.10,192.0.2.11"} {
		if _, err := load(env(map[string]string{"MQTT_PASSWORD_FILE": "/p", "MQTT_HOST": host})); err == nil {
			t.Errorf("MQTT_HOST %q accepted", host)
		}
	}
	s, err := load(env(map[string]string{"MQTT_PASSWORD_FILE": "/p", "MQTT_HOST": "192.0.2.10", "MQTT_PORT": "8883"}))
	if err != nil || s.bridge.BrokerURL != "mqtt://192.0.2.10:8883" || s.bridge.DashboardURL != "" {
		t.Errorf("settings = %+v, %v", s.bridge, err)
	}
}
