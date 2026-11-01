# token-trim

CLIProxyAPI plugin (v0.4) that shortens **tool outputs** in requests before they go upstream, plus a **Token Trim** page (sidebar → Plugins) that shows what it saved, how many tokens go through the proxy, what they cost, how the prompt cache behaves and how fast models reply.

## What it changes

It changes only tool outputs: Claude `tool_result` blocks, OpenAI Chat `role: "tool"` messages, and OpenAI Responses `function_call_output` / `custom_tool_call_output` items, whether plain strings or their text parts. It never touches prompts, model replies, the system prompt, tool definitions, images, thinking blocks or anything the provider sends back.

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

Outputs shorter than `min_output_chars` are left alone, and so are outputs from tools in `skip_tools` and requests for models in `skip_models`. Skipped outputs are still counted. Opting out per client key is not possible, because the body hook only sees the request body.

## Prompt cache safety

Each turn, an agent resends the whole conversation, and providers cache the unchanged prefix. Two mechanisms keep that prefix byte-identical:

- **Deterministic rules.** Every rule is a pure function of one output, so an old output always trims to the same text.
- **Pinning.** Each tool output (content hash plus tool call id) remembers the form it was first sent in: raw, cleaned with a given rule set, or a duplicate reference. It goes out in exactly that form on every later turn, whatever the mode or rules are by then. Measuring pins outputs as raw, so switching to `apply` leaves open conversations as the provider cached them and trims only new outputs. Turning a rule on or off works the same way: only outputs sent afterwards change. Pins live in `pins.bin` (200,000 outputs, about 7 MB, oldest dropped first) and survive restarts. `off` mode does not pin, so outputs sent while off may be trimmed on their next resend.

The **Cache** tab checks the result on live traffic. It shows how much of each model's prompt was read from the cache, and it flags **cache breaks**: within one conversation (session id plus model), the prompt kept growing, yet within 5 minutes of the previous request most of the previous prompt was written to the cache again instead of read from it. A rebuild after a longer pause counts as **expired**, which is normal. Only providers that report cache writes (Anthropic) can show either. Session ids come from the host: the Claude Code header, Codex metadata, or OpenCode `session_id`. The page stores only a short hash of each id.

## Safety net

- Every patched body is rebuilt and re-parsed before it is returned. One that is not valid JSON goes out unchanged and is counted (`unsafe`). Edits that are out of range or overlapping are rejected the same way.
- A body that cannot be parsed, an invalid result or a panic sends the body unchanged. The host also fails open on plugin errors.
- `testdata/` holds request bodies shaped like what Claude Code, Codex, OpenAI Chat clients and OpenCode send (escape codes, `\u2028`, NUL, overstrike, JSON Lines, thinking blocks, images, `cache_control`). `TestCorpus` checks that the patched body is valid JSON and identical outside tool outputs, and that each output becomes its cleaned form or a duplicate reference. `FuzzTrim` uses the same bodies as seeds (`go test -fuzz=FuzzTrim`).

## How it hooks in

- `request.patch_body_before` (plugin schema v8): the host passes the raw client request body, and the plugin returns byte-range edits that replace only the changed tool-output strings. Bodies without a tool output are skipped before parsing. Tool names come from the matching `tool_use` / `tool_calls` / `function_call` entries in the same body.
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
- **Settings**: opt-out lists, the price table (models seen without a price are listed for you), and thresholds. Changes go to `config.yaml` through `PATCH /v0/management/plugins/token-trim/config` and are applied without a restart.

## Routes (under `/v0/management/`, management key required)

`token-trim/stats?days=` · `token-trim/preview` (POST `{"text"}`) · `token-trim/reset` (POST: clears trim statistics; consumption, speed, cache data and pins are kept)

## Configuration (`plugins.configs.token-trim` in `config.yaml`, also editable on CPAMC's Plugins page)

| Field | Default | Meaning |
|---|---|---|
| `mode` | `measure` | `off` · `measure` (record savings, send unchanged) · `apply` (send trimmed outputs) |
| `rule_ansi`, `rule_progress`, `rule_overstrike`, `rule_control`, `rule_repeats`, `rule_json`, `rule_jsonl`, `rule_dedupe` | true | rules above |
| `min_output_chars` | 200 | shorter tool outputs are left alone |
| `dedupe_min_chars` | 2000 | shortest duplicate replaced (minimum 200) |
| `skip_tools` | `[]` | tool names never trimmed, e.g. `["Read", "mcp__*"]` (case-insensitive, `*` at the end = prefix) |
| `skip_models` | `[]` | models whose requests are never trimmed, e.g. `["claude-opus-*"]` |
| `prices` | `{}` | $ per million tokens by model (exact name or `prefix*`, exact wins, then the longest prefix): `{"claude-sonnet-*": {"input": 3, "cache_read": 0.3, "cache_write": 3.75, "output": 15}}`. On subscriptions this shows what the same traffic would cost on the API |
| `data_dir` | `token-trim-data` | relative to `/CLIProxyAPI`; mounted from `../data/token-trim` |
| `history_days` | 90 | days of daily statistics kept (keep at least 31 for the month total) |

## Data (`../data/token-trim/`)

- `stats.json`: daily buckets (consumption and cache behaviour by model, speed by model and credential, trim counters per rule, model, tool and format, plus what disabled rules would have removed), the last 60 trims, the 20 biggest outputs and the last 30 cache breaks.
- `pins.bin`: append-only, 34 bytes per pinned output, compacted on load when it has grown past twice its cap or ends in a partial record.
- Kept in memory only: the per-minute live data and the last request of each session (for break detection). After a restart the live tiles start empty, and the first request of each open conversation is not compared.

## Build and install

```sh
./build.sh                 # builds ../plugins/token-trim.so in golang:1.26-bookworm
docker compose restart     # .so changes are not hot-reloaded (config changes are)
```

Tests: `docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false golang:1.26-bookworm go test ./...`
