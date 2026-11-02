# token-trim

CLIProxyAPI plugin (v0.5) that reduces tool output before it goes upstream, with optional private archives, MCP retrieval and cache-aware history layouts. Its **Token Trim** management page reports ordinary trim estimates, provider usage, cache behaviour and reply speed.

## What it changes

Ordinary trimming changes only tool outputs: Claude `tool_result` blocks, OpenAI Chat `role: "tool"` messages, and OpenAI Responses `function_call_output` / `custom_tool_call_output` items, whether plain strings or text parts. It leaves the system prompt, tool definitions, images, thinking blocks and provider replies alone. **Optional history archiving** also replaces complete older conversation groups with a searchable index; it is off by default and requires the access setup below.

| Rule | Does | Lossy |
|---|---|---|
| Colour codes (`rule_ansi`) | removes terminal escape sequences | no (for the model) |
| Progress bars (`rule_progress`) | a line redrawn with `\r` keeps its final state; CRLF endings stay | no |
| Overstrike (`rule_overstrike`) | renders backspace bold/underline (`man`, some build tools) as a terminal shows it | no |
| Control characters (`rule_control`) | drops characters a terminal does not print (NUL, BEL, form feed, DEL…) | no |
| Repeated lines (`rule_repeats`) | a run of 12 or more lines that differ only in their numbers (and contain a letter) keeps its first 2 and last 2 lines plus `[… N similar lines omitted by token-trim]`. Line-numbered file views (Claude Code `Read`, `cat -n`) are skipped because edits quote them exactly | yes, marked |
| JSON whitespace (`rule_json`) | an output that is one pretty-printed JSON value is minified (`json.Compact`: key order and values untouched) | no |
| JSON Lines (`rule_jsonl`) | output where every non-empty line is a JSON value is minified line by line | no |
| Duplicate outputs (`rule_dedupe`) | a later output identical to an earlier one in the same request (at least `dedupe_min_chars` long) becomes `[token-trim: identical to the output of tool call <id> above]` | no (the first copy stays) |
| Passing tests (`rule_tests`) | folds recognised adjacent Go verbose run/pass records, verbose pytest, Jest and TAP successes into counted markers; failures and their diagnostic tails stay exact | yes, marked |
| Build progress (`rule_build`) | folds recognised CMake and Cargo progress; unknown lines and diagnostics stay | yes, marked |
| Install progress (`rule_install`) | folds recognised npm fetch and pip download records; warnings, errors and totals stay | yes, marked |
| Search grouping (`rule_search`) | adjacent matches share a quoted file-path header; every match, line number and line ending stays recoverable by prepending that path | no |
| Diagnostic grouping (`rule_diagnostics`) | adjacent warning/note records share a path header; error records stay exact | no |

Outputs shorter than `min_output_chars`, and new outputs matching `skip_tools` or `skip_models`, are left alone. Existing apply-mode pins retain their first-sent form. New format-aware rules require recognised shell/search tools and command provenance. Unknown or compound commands and command arrays do not get those new folds. File reads, patches and explicit tool errors are protected; the generic repeats rule is suppressed in recognised structured/protected contexts for new modern-rule pins. Old pin masks keep their original algorithms.

## Prompt cache safety

Each turn, an agent resends the whole conversation, and providers cache the unchanged prefix. Two mechanisms keep that prefix byte-identical:

- **Deterministic rules.** Every rule is a pure function of one output, so an old output always trims to the same text.
- **Pinning.** Each tool output (content hash plus tool call id) remembers the form it was first sent in: raw, cleaned with a given rule set, or a duplicate reference. It goes out in exactly that form on every later turn, whatever the mode or rules are by then. Measuring pins outputs as raw, so switching to `apply` leaves open conversations as the provider cached them and trims only new outputs. Turning a rule on or off works the same way: only outputs sent afterwards change. Pins live in `pins.bin` (200,000 outputs, about 7 MB, oldest dropped first) and survive restarts. `off` mode does not pin, so outputs sent while off may be trimmed on their next resend.

The **Cache** tab checks the result on live traffic. It shows how much of each model's prompt was read from the cache, and it flags **cache breaks**: within one conversation (session id plus model), the prompt kept growing, yet within 5 minutes of the previous request most of the previous prompt was written to the cache again instead of read from it. A rebuild after a longer pause counts as **expired**, which is normal. Only providers that report cache writes (Anthropic) can show either. Session ids come from the host: the Claude Code header, Codex metadata, or OpenCode `session_id`. The page stores only a short hash of each id.

Archived output views and history layouts are also durable. History boundaries move in growth batches, not on each append. Forks retain separate matching prefix plans. A history cut intentionally rewrites the prefix once; the Cache tab may report that rebuild. Removing retrieval access, disabling archive integration, or changing mode away from apply can restore full history and rewrite the prefix too. Byte budgets are not exact model context limits, and this plugin does not promise to prevent client auto-compaction.

## Safety net

- Every patched body is rebuilt and re-parsed before it is returned. One that is not valid JSON goes out unchanged and is counted (`unsafe`). Edits that are out of range or overlapping are rejected the same way.
- A body that cannot be parsed, an invalid result or a panic sends the body unchanged. The host also fails open on plugin errors.
- `testdata/` holds request bodies shaped like what Claude Code, Codex, OpenAI Chat clients and OpenCode send (escape codes, `\u2028`, NUL, overstrike, JSON Lines, thinking blocks, images, `cache_control`). `TestCorpus` checks that the patched body is valid JSON and identical outside tool outputs, and that each output becomes its cleaned form or a duplicate reference. `FuzzTrim` uses the same bodies as seeds (`go test -fuzz=FuzzTrim`).

## How it hooks in

- `request.intercept_before` (plugin schema v8): the host supplies the request body, headers and an opaque authenticated `CallerScope`. Ordinary trimming returns byte-range edits; archive/history layouts return a replacement body. `request.intercept_after` is a no-op and does not receive the body. Internal workspace/token headers are always cleared before upstream delivery. No host credentials are stored by the archive. Hosts without `CallerScope` get ordinary trimming only.
- `usage.handle` (`usage_plugin`): the host reports every finished request with the provider's token counts and its own timing (latency, time to first token, session id, credential). Nothing extra is ever sent to a provider. Providers overlap their token fields differently: Claude's input excludes cache reads and its output includes thinking, OpenAI's input includes cached tokens, Gemini reports thoughts separately. So the plugin keeps the host's total and splits it into non-overlapping parts: uncached input = total − output − cache read − cache write.

## Page

- **Overview**: health strip (share of today's prompt read from cache, cache breaks today, pinned outputs, failed patch checks) and **Token consumption**:
  - live tokens/s and output speed over the last 5 minutes, with a bar per minute for the last hour
  - today's and this month's totals split into input, cache read, cache write and output, with their cost when prices are set
  - trim tiles, where tokens saved (bytes ÷ 4) are split into **new** (full input price) and **re-sent** (mostly cache reads), plus their value at your prices
  - a per-day chart
- **Trimming**:
  - rules with on/off switches; a disabled rule shows what it would have removed
  - **Savings by tool**: each output counted once, when first sent
  - **Biggest outputs** of the last 30 days, sizes only
  - savings by model and request format, and recent trims
  - a "Try it" box
- **Cache**: prompt cache by model (read from cache, written, breaks, expired), the latest cache breaks with the time of the last settings change, pinning (outputs pinned, outputs kept as first sent instead of trimmed), and the safety counters.
- **Speed**: per model and per credential, the average, median and slowest-10% output speed (tokens/s while generating) and time to first token. Medians and percentiles are interpolated from fixed histograms. A last table shows time to first token by prompt size (< 8k, 8k–32k, 32k–128k, ≥ 128k tokens). Only streamed replies are timed. Failed replies, replies under 10 tokens and streams delivered in one piece (< 100 ms) are skipped.
- **Archive**: opt-in controls, bounded caller/workspace discovery, management-issued MCP bearer tokens, revocation and byte reductions since the listener started. Archive figures are not added to ordinary token/cost estimates; they do not price retrieval overhead.
- **Settings**: opt-out lists, the price table (models seen without a price are listed for you), and thresholds. Changes go to `config.yaml` through `PATCH /v0/management/plugins/token-trim/config` and are applied without a restart.

## Routes (under `/v0/management/`, management key required)

`token-trim/stats?days=` · `token-trim/preview` (POST `{"text"}`) · `token-trim/reset` (POST: clears trim statistics; consumption, speed, cache data and pins are kept)

Archive administration: `token-trim/archive/namespaces` (GET), `token-trim/archive/token` (POST `{"namespace":"<handle>"}`, returns a bearer once), `token-trim/archive/revoke` (POST `{"token":"<bearer>"}`). These require the management key. Normal request traffic can discover at most 128 pending workspaces in memory; only administrative provisioning persists a new namespace.

## Configuration (`plugins.configs.token-trim` in `config.yaml`, also editable on CPAMC's Plugins page)

| Field | Default | Meaning |
|---|---|---|
| `mode` | `measure` | `off` · `measure` (record savings, send unchanged) · `apply` (send trimmed outputs) |
| `rule_ansi`, `rule_progress`, `rule_overstrike`, `rule_control`, `rule_repeats`, `rule_json`, `rule_jsonl`, `rule_dedupe` | true | rules above |
| `rule_tests`, `rule_build`, `rule_install`, `rule_search`, `rule_diagnostics` | true | format-aware rules above |
| `archive_enabled` | false | opt into the private archive and its MCP listener |
| `archive_listen` | `127.0.0.1:8319` | MCP listener; failed startup disables archive transformations, not ordinary trimming |
| `archive_max_bytes` | 1073741824 | global archive/view quota; new growth is refused when full, existing reads and token revocation remain available |
| `history_budget_bytes` | 600000 | threshold for considering an old-history cut; not an exact token/window limit |
| `history_recent_bytes` | 150000 | recent-history target; minimum newest eight entries and index overhead may exceed the budget, in which case another cut waits for a full growth batch |
| `output_limit_bytes` | 24576 | threshold for a recoverable beginning/end preview of eligible large tool output |
| `min_output_chars` | 200 | shorter tool outputs are left alone |
| `dedupe_min_chars` | 2000 | shortest duplicate replaced (minimum 200) |
| `skip_tools` | `[]` | tool names never trimmed, e.g. `["Read", "mcp__*"]` (case-insensitive, `*` at the end = prefix) |
| `skip_models` | `[]` | models whose requests are never trimmed, e.g. `["claude-opus-*"]` |
| `prices` | `{}` | $ per million tokens by model (exact name or `prefix*`, exact wins, then the longest prefix): `{"claude-sonnet-*": {"input": 3, "cache_read": 0.3, "cache_write": 3.75, "output": 15}}`. On subscriptions this shows what the same traffic would cost on the API |
| `data_dir` | `token-trim-data` | relative to `/CLIProxyAPI`; mounted from `../data/token-trim` |
| `history_days` | 90 | days of daily statistics kept (keep at least 31 for the month total) |

## Enable recoverable output and history

This stays in the existing proxy: no additional model gateway or provider account is needed. Native loading and HTTP traffic were exercised with CLIProxyAPI **v8.0.51**. Build/install the updated library first.

1. Enable the archive in the plugin's config. Start in `measure`; move to `apply` once the retrieval connection works:

   ```yaml
   plugins:
     configs:
       token-trim:
         mode: measure
         archive_enabled: true
         archive_listen: "0.0.0.0:8319" # inside Docker; publish only on host loopback
   ```

   For Docker, add `127.0.0.1:8319:8319` to the proxy service's `ports` and retain its existing data volume. Without Docker, keep the default `127.0.0.1:8319` listener. Use TLS or a trusted encrypted tunnel for remote clients; bearer tokens grant access to the workspace's raw archive.

2. Give each project a stable API request header, for example `X-Token-Trim-Workspace: billing`. Omission uses `default`. Send one normal request, then open **Token Trim → Archive → Refresh callers**. Rows are isolated by authenticated client key plus workspace. People sharing a proxy API key share its caller identity; use separate proxy keys for separate users.

3. Issue an MCP token for the intended row. The management key is **not** an MCP credential.

4. Configure a remote HTTP MCP server at `http://127.0.0.1:8319/mcp`, with `Authorization: Bearer <workspace-token>`. It advertises `token_trim_search` and `token_trim_read`. Configure the same client's **model API requests** with both custom headers:

   ```text
   X-Token-Trim-Workspace: billing
   X-Token-Trim-Archive-Token: <same-workspace-token>
   ```

   Keep the existing API base URL and API-key configuration. Merge these headers with any existing custom headers. The plugin removes both internal headers before forwarding upstream. A wrong caller/workspace token, missing retrieval tools, or missing host identity leaves archive/history transformations inactive.

5. Reconnect/restart the client so it advertises both tools, then switch Token Trim to `apply`. Already-pinned raw outputs remain raw; new eligible outputs can use recoverable previews.

### Retrieval and history behaviour

- A preview appears only after the exact original and its stable rendered view have been durably saved. It includes the document ID and a `token_trim_read` call. Errors, source-reading tools/commands, patches, numbered file views and retrieval results are excluded from preview creation.
- MCP uses stateless HTTP POST JSON-RPC. It supports `initialize`, notifications, `ping`, `tools/list` and `tools/call`; browser Origins are refused. Read arguments: `{"id":"<64-hex-id>","offset":0,"limit":16384}`. Follow `next_offset` until null. Limits are UTF-8-safe byte counts, up to 65536.
- Search arguments: `{"query":"words","limit":10}`. Search is deterministic keyword matching, bounded to 8 MiB / 1024 documents / 50 results; `truncated` reports incomplete scans. Namespace selection is never an MCP argument: it comes from the bearer token.
- History cuts require full replayed conversation arrays. The first user message, system/developer entries and newest eight entries are retained. Only complete tool-call/result groups leave; retained message elements, including signed thinking, are not rewritten. Removed entries are archived as their exact original JSON.
- The index retains older user text and tool-call pointers. Known later edits of the same file path or Git worktree-changing commands flag old reads as potentially stale when the index is built. Every retrieved item is labelled historical; always read current files before editing.
- Images, file/PDF inputs, audio/video and other recognised non-text attachments prevent cuts that would lose their native representation. Responses requests using `previous_response_id` do not get history cuts. Large user text, protected entries and the mandatory tail can leave a request above the byte budget.
- `measure` computes ordinary rule savings only; it does not estimate history/preview savings or store raw conversation content. The Archive page's runtime byte counters include successful transformed requests and replays, not failed attempts.

### Storage and privacy

Archives contain **raw, unredacted text**, including any secrets already present in tool output or older conversation entries. Files are private (`0600`, directory `0700`), not encrypted. Tokens are random; only their hashes are stored. Root/document symlinks and unsafe existing permissions are rejected. An exclusive lock prevents two writers sharing the archive directory.

No automatic retention or eviction is applied to referenced content. At quota, new writes fail open and existing retrieval keeps working. Disabling the feature stops the listener but does not delete data. To forget everything, stop the proxy, remove `archive/` and `pins.bin` from this plugin's data directory, then restart. This permanently invalidates old archive IDs/tokens and may rebuild cached prompts; `stats.json` can be kept. Restrict OS access and back up the private archive only where raw project data is allowed.

## Data (`../data/token-trim/`)

- `stats.json`: daily buckets (consumption and cache behaviour by model, speed by model and credential, trim counters per rule, model, tool and format, plus what disabled rules would have removed), the last 60 trims, the 20 biggest outputs and the last 30 cache breaks.
- `pins.bin`: append-only, 34 bytes per pinned output, compacted on load when it has grown past twice its cap or ends in a partial record.
- `archive/`: exact content-addressed documents, hashed workspace/token metadata, private rendered output views and history plans. Authentication does not persist client/provider credentials; raw archived content can still contain secrets. Its quota is separate from `history_days`, which controls statistics only.
- Kept in memory only: the per-minute live data and the last request of each session (for break detection). After a restart the live tiles start empty, and the first request of each open conversation is not compared.

## Build and install

```sh
./build.sh                 # builds ../plugins/token-trim.so in golang:1.26-bookworm
docker compose restart     # .so changes are not hot-reloaded (config changes are)
```

Tests: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false golang:1.26-bookworm go test -race ./...`
