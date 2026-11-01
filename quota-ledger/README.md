# quota-ledger

CLIProxyAPI plugin (v0.3) that adds a **Quota Ledger** page (sidebar → Plugins) to the management UI and quota-aware routing.

| Provider | Credentials | Quota source |
|---|---|---|
| Claude | OAuth auth files (`provider: claude`) | `api.anthropic.com/api/oauth/usage` + `/profile` |
| Codex | OAuth auth files (`provider: codex`) | `chatgpt.com/backend-api/wham/usage` |
| OpenCode Go | `api-keys.openai-compatibility` groups whose `base-url` contains `opencode.ai` | `opencode.ai/zen/go/v1/usage` |

## How it works

- **Server poller** (Go, inside the CPA process) polls every `poll_interval_minutes` through the loopback management API (`auth-files`, `config`, `api-call`), authenticating with the process env var `MANAGEMENT_PASSWORD` (set in `../.env`, passed by docker compose; CPA accepts it as a second management key).
- **Rate limits**: Anthropic's `/api/oauth/usage` answers 429 after a handful of calls, so upstream calls are kept to a minimum. Scheduled polls skip accounts checked within half an interval (a restart reuses `last.json` instead of refetching), a manual refresh skips accounts checked in the last minute, and the Claude plan (`/profile`) is re-read at most every 12 h. After a 429 the account is not called again for 15 min, doubling up to 2 h; the page shows the last known numbers with the next check time. Reset-grant reads from the page are cached for an hour and paused during a backoff.
- **Page** (`index.html`, embedded) reads the plugin's authenticated routes with your management key (reused from CPAMC when "Remember password" is checked, else prompted once per tab).
- **Scheduler** (`scheduler.pick`) and **usage observer** (`usage.handle`) hook into routing; anything the scheduler cannot decide is left to CPA's built-in scheduler (it never rejects a request).
- **Live traffic**: every proxied request's outcome (429 / 401-403 / 5xx, latency) is counted per account. A 429 makes the scheduler try the account's siblings first for 2 min; a 401/403 not followed by a success marks the login as expired (so does a 401/403 quota check, or a host auth error mentioning an expired/revoked token) and that account is tried last until it works again. With a single account per provider there is nothing to route around: the request still goes to it.
- **Wasted quota**: when a window's reset passes between two checks, the share still unused at the last check before it is recorded (`waste.json`, 180 days). Usage between that check and the reset is not seen, so the figure can be slightly high.
- **Run-out alerts**: after each check, a least-squares fit over the window's last 24 h projects when it hits 0%; one `runout` alert per window cycle when that is at least 30 min before the reset (needs ≥3 points over ≥2 h and ≥10% used).

Page features: **Use first** (accounts in spending order, soonest-expiring weekly/monthly budget first, with the % each would lose at reset at its average pace so far; limits blocked by an empty sibling until after they reset are left out, since that quota is lost anyway; model caps like 7-day Fable show as extra bars inside the all-models card), **Lost at reset** (unused share at each recorded weekly/monthly reset, last 8 weeks), provider tabs and summary cards, remaining-% per window, pace badges (used% vs elapsed% of the window), Claude overage spend and session-credit pool, 30 s auto-refresh with "updated N min ago", capacity-returning strip, 7-day sparklines and run-out projection, per-account live traffic (24 h requests, failures by kind, average latency, last failure, "skipped by routing"), login banner with **Sign in again** (CPAMC OAuth page) / **Turn off** (auth-file switch) / **Edit key** (OpenCode), per-account alert mute, alerts panel (dismiss one or all, browser notifications, tab-title count, webhook test, digest preview/send), routing rules with the ones active now, status-bar preview, usage attribution (credential → client key → model), scheduler status with `reserved` badges, and Claude reset-grant / Codex reset-credit actions when available.

## Routes (under `/v0/management/`, management key required)

`quota-ledger/state` · `quota-ledger/refresh` (POST, optional `{"auth_index"}`) · `quota-ledger/history?hours=` · `quota-ledger/usage?hours=` · `quota-ledger/alerts?since=` · `quota-ledger/alerts/test` (POST) · `quota-ledger/alerts/dismiss` (POST `{"all":true}` or an alert's `at`/`kind`/`auth_index`/`window`) · `quota-ledger/mute` (POST `{"auth_index","muted"}`) · `quota-ledger/waste?days=` · `quota-ledger/digest` (GET preview, POST send now) · `quota-ledger/bar` (Waybar JSON; `?format=text` plain) · `quota-ledger/metrics` (Prometheus)

### Status bar and metrics

Both routes need the management key like every management route.

```jsonc
// Waybar: "custom/quota" module
{ "exec": "curl -s -H 'Authorization: Bearer KEY' http://127.0.0.1:8317/v0/management/quota-ledger/bar", "return-type": "json", "interval": 60 }
```

Text is the best account's plan remaining per provider (`C 92% · X 81% · O 0%`, `!` = an account has a problem); `class` is `ok`, `warning` (below `alert_below_percent` or a problem) or `critical` (a provider at 0%).

```yaml
# Prometheus scrape job
- job_name: quota-ledger
  metrics_path: /v0/management/quota-ledger/metrics
  authorization: { credentials: KEY }
  static_configs: [{ targets: ["127.0.0.1:8317"] }]
```

Gauges per account: `quota_ledger_remaining_percent`, `_reset_timestamp_seconds`, `_projected_loss_percent`, `_wasted_percent` (last reset), `_up`, `_login_ok`, `_rate_limited`, `_disabled`, `_requests_24h{outcome}`, `_latency_avg_seconds_24h`; counters `_scheduler_picks_total`, `_scheduler_fallbacks_total`.

## Configuration (`plugins.configs.quota-ledger` in `config.yaml`, also editable on CPAMC's Plugins page; applied without restart)

| Field | Default | Meaning |
|---|---|---|
| `poll_interval_minutes` | 5 | server poll period |
| `history_days` | 14 | retention for history, usage, alerts |
| `management_url` | `http://127.0.0.1:8317` | loopback management API |
| `data_dir` | `quota-ledger-data` | relative to `/CLIProxyAPI`; mounted from `../data/quota-ledger` |
| `alert_below_percent` | 20 | `below` alert when a plan window's remaining crosses under it |
| `alert_run_out` | true | `runout` alert when a window would hit 0% before it resets |
| `webhook_url` | "" | alert endpoint; empty = none |
| `webhook_format` | `json` | `json` \| `discord` \| `slack` \| `ntfy` |
| `quiet_hours` | "" | server local time range, e.g. `22:00-07:00`: alerts are listed but not sent to the webhook |
| `digest_day` | `mon` | weekly digest to the webhook (`off` \| `mon` … `sun`): quota left, lost at reset, use-first order, 7-day traffic, problems |
| `digest_hour` | 9 | server local hour of the digest; a slot missed by more than a day is skipped |
| `scheduler_mode` | `off` | `off` (kill switch) \| `most-remaining` \| `soonest-reset` \| `use-first` (the quota that would be lost at the next weekly reset first, same order as the page) |
| `reserve_floor_percent` | 0 | >0: credentials below this plan remaining are skipped for non-exempt clients |
| `reserve_exempt_user_agents` | `claude-cli` | comma-separated User-Agent substrings that may use reserved credentials |
| `routing_rules` | [] | ordered rules, first match overrides `scheduler_mode` and/or the floor (see below) |

Scheduler "plan remaining" = lowest remaining over plan windows. The Claude session-credit dollar pool never binds; model-scoped windows (e.g. 7-day Fable) bind only requests for that model and also count for `use-first` on those requests.

```yaml
routing_rules:
  # weekdays 9-18, scripts may not take Claude below 40% (claude-cli still can)
  - { days: "mon-fri", hours: "09:00-18:00", provider: claude, client: "!claude-cli", reserve_floor_percent: 40 }
  # Fable requests go where the Fable cap would otherwise be lost
  - { model: fable, mode: use-first }
```

Rule fields (all optional, empty matches everything): `days` (`mon-fri`, `sat,sun`, wraps), `hours` (server local `HH:MM-HH:MM`, wraps midnight), `provider`, `model` (substring), `client` (User-Agent substring, `!x` = without x), `mode`, `reserve_floor_percent` (applies to every request the rule matches; the exempt list does not). A rule needs `mode` or `reserve_floor_percent`; invalid rules and quiet hours are ignored and shown at the top of the page. `scheduler_mode: off` stays a kill switch: rules are not applied. Rules never reject a request: when everything is filtered out, CPA's own scheduler decides.

## Data (`../data/quota-ledger/`)

`history.jsonl` (`[ts_ms, auth_index, window_key, used]` per poll), `usage.json` (hourly buckets with failures by kind and latency; client keys stored masked only), `alerts.json` (last 200 events), `last.json` (last successful quota per account, shown after a restart until the next fetch succeeds; a failed fetch keeps the old numbers and shows the error next to them), `waste.json` (unused share at each reset), `prefs.json` (muted accounts, last digest, once-per-cycle alert markers). Alerts, snapshots and mutes of accounts that no longer exist are dropped at the next poll.

## Build and install

```sh
./build.sh                 # builds ../plugins/quota-ledger.so in golang:1.26-bookworm
docker compose restart     # .so changes are not hot-reloaded (config changes are)
```

Tests: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false golang:1.26-bookworm go test ./...`

## OpenCode Go key

Add it as an OpenAI-compatible provider (AI Providers page, or `config.yaml`):

```yaml
api-keys:
  openai-compatibility:
    - name: "opencode-go"
      base-url: "https://opencode.ai/zen/go/v1"
      keys:
        - api-key: "<your OpenCode key>"
      models:
        - name: "<model id>"
```
