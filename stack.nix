{ lib, bridge, ... }:
let
  # Written by the host once the api grant is approved:
  # HOMELAB_API_URL=http://<host .1>:7781/api and HOMELAB_API_TOKEN=…
  apiEnv = "/var/lib/homelab-api/env";
in
{
  # The homelab daemon's base module makes this a container, sets the
  # hostname, time zone and stateVersion, and hands in the address. The
  # bridge only connects out (the broker by the egress grant, the daemon API
  # on the host's .1), so no port is open.

  # ── The bridge ──────────────────────────────────────────────────────────
  users.users.homelab-ha = {
    isSystemUser = true;
    group = "homelab-ha";
  };
  users.groups.homelab-ha = { };

  systemd.services.homelab-ha = {
    description = "Home Assistant MQTT bridge for the homelab";
    wantedBy = [ "multi-user.target" ];
    wants = [ "network-online.target" ];
    after = [ "network-online.target" ];
    # MQTT_HOST (the manifest's address variable) and HOMELAB_DASHBOARD_URL
    # come from the host in /run/secrets/env, which the base module gives
    # every service; so does any of these the operator sets on the host.
    environment = {
      MQTT_USERNAME = "homelab";
      # The credential below, in the unit's credentials directory (%d).
      MQTT_PASSWORD_FILE = "%d/mqtt-password";
      MQTT_CLIENT_ID = "homelab-ha";
      HA_DISCOVERY_PREFIX = "homeassistant";
    };
    serviceConfig = {
      ExecStart = lib.getExe bridge;
      # The host binds the stack's variables read-only at /run/secrets,
      # root-owned; systemd copies this one for the bridge's user. The copy
      # is made at start: restart the stack after a change.
      LoadCredential = "mqtt-password:/run/secrets/MQTT_PASSWORD";
      # Read by systemd as root before dropping to the bridge's user. A
      # missing file (the api grant not approved yet) is not an error: the
      # bridge runs without the API. A new token takes a restart of the
      # stack, which the homelab dashboard asks for.
      EnvironmentFile = "-${apiEnv}";
      User = "homelab-ha";
      Group = "homelab-ha";
      Restart = "always";
      RestartSec = "10s";

      # The bridge reads /proc/stat, /proc/meminfo and /sys/class/hwmon, so
      # no ProcSubset=pid.
      NoNewPrivileges = true;
      CapabilityBoundingSet = "";
      ProtectSystem = "strict";
      ProtectHome = true;
      PrivateTmp = true;
      RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
      RestrictNamespaces = true;
      LockPersonality = true;
      MemoryDenyWriteExecute = true;
      SystemCallArchitectures = "native";
    };
  };
}
