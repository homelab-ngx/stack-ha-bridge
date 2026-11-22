# stack-ha-bridge

A bridge between [Home Assistant](https://www.home-assistant.io) and a
[homelab-ngx](https://github.com/homelab-ngx/homelab-ngx) host, as a stack.
It shows the host's stack services and backups in Home Assistant through
MQTT discovery, and lets Home Assistant start, stop and restart those
services.

It does not run Home Assistant. It connects to the MQTT broker of a Home
Assistant you have, and reads and controls the host only through the
daemon's stack API.

In Home Assistant you get one device, `homelab`, with:

| Entity | What it is |
| --- | --- |
| `switch.homelab_<unit>` | one per stack service: on is running |
| `button.homelab_<unit>_restart` | one per stack service |
| `sensor.homelab_backup_<id>_result`, `_last_run`, `_next_run` | one set per backup, read-only. `<id>` is the backup's number, so renaming a backup keeps its entities. The result is `success`, `partial`, `failure`, `cancelled`, `running`, `unknown` or `never` |
| `binary_sensor.homelab_backup_<id>_overdue` | one per backup |
| `sensor.homelab_cpu_usage`, `sensor.homelab_memory_usage` | the host's, from `/proc` |
| `sensor.homelab_cpu_temperature`, `sensor.homelab_nvme_temperature` | from `/sys/class/hwmon`, when the host has those sensors |
| `sensor.homelab_boot_time` | when the host booted |
| `sensor.homelab_last_command` | how the latest switch or button press went |

A host sensor whose source cannot be read when the bridge starts is not
published.

## Getting started

1. In Home Assistant, install the Mosquitto broker add-on and the MQTT
   integration if you have not, and create a user for the bridge.
2. Add the stack: `homelab stacks add github:homelab-ngx/stack-ha-bridge`,
   or Add stack on the dashboard's Stacks page.
3. On the stack's page, fill in `MQTT_HOST` and `MQTT_PASSWORD`, and
   `MQTT_USERNAME` if the user is not called `homelab`.
4. Approve the grants and start the stack. The approval issues the stack's
   API token.
5. In Home Assistant the `homelab` device appears under the MQTT
   integration. Toggle a switch: `sensor.homelab_last_command` reads `…: ok`.

From a shell, step 3 is
`homelab stacks var set ha-bridge MQTT_PASSWORD --value-stdin`.
`journalctl -M ha-bridge -u homelab-ha` on the host says "connected to the
Home Assistant broker" when the broker accepts it.

The daemon refuses every API call outside the two grants below, and core
services are never controllable. The stack has no route, port or storage:
nothing connects to it.

The bridge keeps no state, so there is nothing to back up. Home Assistant
rebuilds the device from the retained discovery message.

## More settings

The host gives the bridge its dashboard's address (`HOMELAB_DASHBOARD_URL`,
the device's link in Home Assistant) and its API token.

The bridge also reads `MQTT_PORT` (1883), `MQTT_CLIENT_ID` (`homelab-ha`)
and `HA_DISCOVERY_PREFIX` (`homeassistant`). Add them as your own variables
on the stack's page. A port other than 1883 also needs a stack whose egress
names it.

<!-- manifest:start -->
<!-- Made from homelab-stack.json. Change the manifest, not these lines. -->

## Grants

You approve each of these before the stack starts.

- Network: out to the addresses you give in `MQTT_HOST` on port 1883/tcp.
- API: the stack reads the state of the host from the daemon.
- API: the stack starts, stops and restarts stacks and their units.

## Variables

| Name | Kind | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `MQTT_HOST` | address | yes |  | The IPv4 address of Home Assistant's MQTT broker (the Mosquitto add-on runs on the Home Assistant machine). The bridge connects to it on port 1883. |
| `MQTT_PASSWORD` | secret | yes |  | Password of the bridge's user on the MQTT broker. Create the user in Home Assistant (Settings, People, Users) or in the Mosquitto add-on. |
| `MQTT_USERNAME` | env | no |  | The bridge's user on the MQTT broker. Unset: homelab. |

## Health

An update is kept when all of this holds, and rolled back when it does not.

- `homelab-ha.service` is active.

Manifest version 9, built with nixpkgs `nixos-unstable`. Runs on [homelab-ngx](https://github.com/homelab-ngx/homelab-ngx).

<!-- manifest:end -->

## How it works

- One Home Assistant device, `homelab`, is published as a single
  device-discovery payload at `homeassistant/device/homelab/config`
  (retained). On every connect the bridge reads that payload back and
  deletes every component it no longer publishes, so entities that went
  away are removed, not left unavailable.
- `homelab/status` is the bridge's availability (`online` or `offline`,
  retained, also the MQTT will). `homelab/api/status` says whether the
  daemon API answered the last read. Entities read from the daemon
  (switches, restart buttons, backup sensors) need both. Host sensors and
  the last-command sensor need only the first.
- State is published every 30 s, after every command and after Home
  Assistant restarts (birth on `homeassistant/status`). Commands run one at
  a time per unit with a 2 minute timeout. Retained commands are ignored.
- If the daemon API does not answer, MQTT stays up, host sensors keep
  updating, the daemon's entities go unavailable with their last state, and
  the API is retried with backoff (5 s doubling to 2 min). Until the API has
  answered once the bridge leaves the retained discovery alone, because
  publishing it without the services would delete them in Home Assistant.
- A stack added or removed while the bridge runs reaches Home Assistant at
  the next read, within 30 s: a changed service or backup set republishes
  discovery.

### Daemon API calls

All on `$HOMELAB_API_URL` with `Authorization: Bearer $HOMELAB_API_TOKEN`:

| Call | Scope | Used for |
| --- | --- | --- |
| `GET /services` | read | switches and restart buttons (rows with a `stack`) |
| `GET /backups` | read | backup sensors |
| `GET /version` | read | the device's `sw_version` |
| `POST /services/{unit}/start`, `/stop`, `/restart` | control | switch and button presses |

The host writes the two variables to `/var/lib/homelab-api/env` in the
stack when the `api` grant is approved. The unit reads it as an
`EnvironmentFile`.

## Development

```
nix develop -c go test -race -count=1 ./...
homelab stacks build .   # as the daemon builds it
```

After changing `go.mod`, refresh `vendorHash` in `package.nix` from the
hash `nix build .#homelab-ha` reports. The bridge tests run in `synctest`
bubbles (virtual time), with a fake publisher and a fake API. Nothing talks
to a real broker.

## License

MIT, see [LICENSE](LICENSE).
