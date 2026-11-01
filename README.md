# CPMAC-Plugins

Two [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugins (Go, built as `-buildmode=c-shared`). Each adds a page to the management UI under **Plugins**.

| Plugin | Version | What it does |
|---|---|---|
| [`quota-ledger`](quota-ledger/) | 0.3 | Polls Claude, Codex and OpenCode Go quotas, keeps history and alerts, and routes requests to the credential with the most quota left. |
| [`token-trim`](token-trim/) | 0.4 | Shortens tool outputs in requests before they go upstream, and shows token use, cost, prompt-cache behaviour and reply speed. |

Each folder has its own README with the routes, every config field and the on-disk data layout.

## Install from the plugin store

Linux amd64 builds are published as releases. Add this repo as an extra store source in `config.yaml`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/Osyna/CPMAC-Plugins/main/registry.json"
```

Then open **Plugins → Store** in the management UI and install `quota-ledger` and `token-trim`. The plugins land in the plugins folder, so they survive image updates as long as that folder is a mounted volume (see below). Each release zip is checked against the SHA-256 pinned in `registry.json`.

## Build from source

You need the CLIProxyAPI Docker setup (`eceasy/cli-proxy-api`), Docker, and Linux. The plugins are built in Debian bookworm so their glibc matches that image.

```sh
git clone https://github.com/Osyna/CPMAC-Plugins
cd CPMAC-Plugins
./quota-ledger/build.sh     # -> plugins/quota-ledger.so
./token-trim/build.sh       # -> plugins/token-trim.so
```

Then mount the plugin folder and a data folder per plugin into the container, and give it a management password that `quota-ledger` uses to read the loopback management API:

```yaml
# docker-compose.yml (excerpt)
services:
  cli-proxy-api:
    image: eceasy/cli-proxy-api:latest
    env_file: .env              # MANAGEMENT_PASSWORD=<your management key>
    volumes:
      - ./config.yaml:/CLIProxyAPI/config.yaml
      - ./plugins:/CLIProxyAPI/plugins
      - ./data/quota-ledger:/CLIProxyAPI/quota-ledger-data
      - ./data/token-trim:/CLIProxyAPI/token-trim-data
```

Enable them in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    quota-ledger:
      enabled: true
      priority: 1
      scheduler_mode: "most-remaining"   # "off" disables routing
    token-trim:
      enabled: true
      priority: 2
      mode: measure                      # off | measure | apply
```

Restart the container (`.so` changes are not hot-reloaded; config changes are). `token-trim` starts in `measure`, which records savings without changing requests; switch to `apply` once you have looked at the numbers.

## Test

```sh
cd quota-ledger && go test ./...
cd ../token-trim && go test ./...
```

## License

[MIT](LICENSE)
