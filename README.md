# ebeco-spot

Control an **Ebeco EB-Therm** electric floor thermostat from a
[spot-hinta.fi](https://spot-hinta.fi) heating schedule.

During the cheapest electricity periods the floor target is raised to a
**baseline** temperature; the rest of the time it drops to a **low** setpoint.
It's a Go port of the popular Shelly "Minimal-Heating" script, with the relay
replaced by the Ebeco Connect API's target-temperature control.

## How it works

- Authenticates to the [Ebeco Connect API](https://ebecoconnect.com/swagger)
  with email/password and keeps the bearer token fresh (auto re-auth on expiry
  and on a `401`). Repeated auth failures back off exponentially (15 s → 10 min)
  so a credential or outage problem can't hammer the token endpoint.
- Fetches a `PlanAhead` schedule from spot-hinta.fi (region, night hours, price
  ranks, max price … all configurable, mirroring the Shelly settings).
- Each cycle it determines whether heating should be **on**, and **only on a
  state change** writes the new target to the configured device(s):
  - **on** → the device's baseline temperature
  - **off** → `off_temperature` (default 15 °C)
- On every write it also sends `powerOn=true` and forces a constant-setpoint
  program (`program_name`, default `Manual`) so the spot-price target is
  honoured even if a built-in weekly schedule would otherwise override it.
- If spot-hinta is unreachable (or returns an empty/non-covering plan) it falls
  back to **backup hours**: heat on during `backup_hours`, otherwise off. While
  degraded it keeps using a still-valid older plan if it has one, and retries the
  fetch with exponential backoff (poll interval → 1 h) instead of every tick.
  Entering and leaving backup mode are logged as distinct transitions.

### Baseline persistence (manual override friendly)

The "on" temperature is a *baseline* that you can adjust by hand:

- On startup, and again right before every control write, the app reads the
  device's current target. If it lies within `[baseline_min, baseline_max]`
  (default 20–30 °C) it is saved to `state_file` as that device's baseline.
- If the current target is outside that window (e.g. it's currently at the 15 °C
  "off" value), the previously saved baseline — or `on_temperature` if none —
  is used instead.

So if you bump the thermostat to 27 °C in the Ebeco app, that becomes the new
baseline and survives restarts and on/off cycling. Baselines are tracked
per-device.

## Configuration

Settings live in a TOML file; **credentials come from the environment** and are
never written to the file:

```sh
export EBECO_EMAIL=you@example.com
export EBECO_PASSWORD=yourpassword
```

Copy `config.example.toml` to `config.toml` and edit it. The only required
field is `device_ids`. To find your device ids, run once — the app logs every
device's id and name on startup.

## Build & run

With the included `Makefile` (run `make help` to see all targets):

```sh
export EBECO_EMAIL=you@example.com EBECO_PASSWORD=secret

make build          # compiles to bin/ebeco-spot
make list           # authenticate and print your devices
make run            # run the controller (CONFIG=... to override config.toml)
```

Or directly with the Go toolchain:

```sh
go build -o bin/ebeco-spot ./cmd/ebeco-spot

EBECO_EMAIL=you@example.com EBECO_PASSWORD=secret \
  ./bin/ebeco-spot -config config.toml
```

Flags: `-config <path>` (or `EBECO_CONFIG` env, default `config.toml`) and
`-debug` for verbose logging. Logs are structured (slog) on stderr.

## Running as a service (systemd)

`/etc/systemd/system/ebeco-spot.service`:

```ini
[Unit]
Description=ebeco-spot floor heating controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/ebeco-spot
ExecStart=/opt/ebeco-spot/ebeco-spot -config /opt/ebeco-spot/config.toml
Environment=EBECO_EMAIL=you@example.com
Environment=EBECO_PASSWORD=secret
Restart=on-failure
RestartSec=30
# Keep credentials out of `systemctl show`; prefer an EnvironmentFile (chmod 600):
# EnvironmentFile=/opt/ebeco-spot/ebeco-spot.env

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now ebeco-spot
journalctl -u ebeco-spot -f
```

## Notes & limits

- The Ebeco API allows 10 requests / 10 s and 30 / 60 s per IP. This app stays
  far under that: it writes only on actual on/off transitions (a few times a
  day) and polls spot-hinta on the same cadence.
- Two-factor-enabled Ebeco accounts are not supported (the token endpoint would
  require a verification code).
- `program_name` defaults to `"Manual"`. If your device expects a different
  label for a constant setpoint (e.g. `"Home"`), change it — the API does not
  enumerate the allowed values.

## Tests

```sh
go test ./...
```
