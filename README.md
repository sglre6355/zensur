# zensur

A configurable, multilingual word-censor Discord bot in Go. Built for English and Japanese, but the
matching engine is language-agnostic.

## How it works

Each incoming message (and edit) is run through a normalization pipeline and then matched against a
YAML-defined ruleset. A rule decides both *what* to match and *what to do* about it (log, delete,
warn, or replace via webhook).

The **same ruleset** also guards server metadata: when a guild's name/description or a channel's
name/topic is changed to something a rule matches, the bot reverts the offending field to its
last-known-good value (the value before the update). The rule's action is reused — `delete`/`warn`
revert the field, `replace` rewrites the offending spans in place, and `log` only records the hit.
The baseline is seeded when the bot connects, so only *changes* the bot witnesses are enforced;
pre-existing names are left untouched.

The normalization pipeline is per-rule and inheritable from global defaults:

| Stage              | Default | Catches                                              |
|--------------------|---------|------------------------------------------------------|
| `normalize_unicode`| on      | full-width letters, compatibility codepoints (NFKC)  |
| `strip_marks`      | on      | zalgo, combining-mark obfuscation                    |
| `case_insensitive` | on      | `BadWord`, `bAdWoRd`                                 |
| `leet`             | off     | `5h1t`, `f@ck`, `$ex`                                |
| `collapse_repeats` | off     | `fuuuuck` (only collapses runs of 3+; keeps `book`)  |
| `fold_kana`        | off     | `ばか` ↔ `バカ`                                      |

Three match modes:

- **substring** — pattern anywhere in normalized text. Best for CJK.
- **word** — pattern bounded by non-letter/digit. Best for English.
- **regex** — RE2 against normalized text.

Plus a per-rule `allow` list of phrases that suppress the rule when present (the
*Scunthorpe problem*).

## Semantic (LLM) filter

Pattern rules catch known strings; an optional **LLM filter** catches things you can only describe
in words ("flag targeted harassment", "flag scam links"). When enabled, every message is also judged
by a large language model against a natural-language `directive`. A message is acted on if
**either** the pattern rules **or** the model flag it; when both fire, the more disruptive action
wins (`log` < `warn` < `delete`).

Each provider is its official Go SDK wrapped behind a small in-house adapter (the `chatProvider`
interface), so switching vendors is just a `provider`/`model` change in the config. Supported
providers:

- `openai` ([openai-go](https://github.com/openai/openai-go)) — also drives any
  **OpenAI-compatible** endpoint (xAI, Groq, OpenRouter, vLLM, ollama's `/v1`, …) by setting
  `endpoint` to its base URL.
- `anthropic` ([anthropic-sdk-go](https://github.com/anthropics/anthropic-sdk-go))
- `google` (Gemini, [google.golang.org/genai](https://pkg.go.dev/google.golang.org/genai))

```yaml
llm:
  enabled: true
  provider: openai
  model: gpt-4o-mini
  api_key_env: OPENAI_API_KEY   # the key is read from this env var, never stored in the file
  action: warn                  # log | delete | warn  (replace is N/A — the model reports no spans)
  directive: |
    Flag targeted harassment, scam/phishing links, or threats of violence.
    Do not flag ordinary profanity or jokes.
```

The API key is read from the environment variable named by `api_key_env`, so secrets stay out of the
config file. See [`config.example.yaml`](config.example.yaml) for every option (`endpoint`,
`timeout_seconds`, `max_tokens`, `temperature`, `max_message_chars`, `notice`).

### Image attachments

The same filter can inspect **image attachments** with the provider's vision model — useful for
content that pattern rules and text analysis can't see (words baked into an image, explicit
pictures). Enable it under `llm.images`; it reuses the provider, endpoint, and API key, and only the
differing fields live there:

```yaml
llm:
  # …text config above…
  images:
    enabled: true
    model: gpt-4o          # optional — defaults to the text model (which must then support vision)
    action: delete         # optional — defaults to the text action
    max_bytes: 5242880     # skip images larger than this (default 5 MiB)
    max_count: 4           # max images checked per message (default 4)
    directive: |           # optional — defaults to the text directive
      Flag the image if it depicts explicit sexual content, gore, or hateful symbols.
```

Only true image attachments (content-type `image/*`) are checked — not embeds or linked URLs. Each
image is a separate vision call, so it adds cost and latency on busy channels. The filter (text and
images) runs only on messages, not on guild/channel metadata.

## Configuration

Env vars:

| Var              | Required | Default        | Notes                                        |
|------------------|----------|----------------|----------------------------------------------|
| `DISCORD_TOKEN`  | yes      | —              | Bot token from Discord Developer Portal      |
| `ZENSUR_CONFIG`  | no       | `./config.yaml`| Path to YAML config                          |
| `LOG_LEVEL`      | no       | `info`         | `debug`, `info`, `warn`, `error`             |
| *(LLM key)*      | if LLM   | —              | Provider API key, read from the var named by `llm.api_key_env` |

YAML config: see [`config.example.yaml`](config.example.yaml).

## Discord setup

In the [Discord Developer Portal](https://discord.com/developers/applications):

1. Enable **Message Content Intent** (privileged) for the bot application.
2. Invite the bot with both the `bot` and `applications.commands` scopes, and
   at minimum the **View Channels**, **Read Message History**, **Manage
   Messages**, and **Send Messages** permissions.
3. For the `replace` action, also grant **Manage Webhooks** on any channel
   where you want messages re-posted under the user's identity.
4. For the metadata guard, grant **Manage Server** (to revert guild
   name/description) and **Manage Channels** (to revert channel name/topic).
   The required guild/channel gateway events are non-privileged and need no
   portal toggle.

## Commands

| Command       | Description                                              |
|---------------|----------------------------------------------------------|
| `/purge count`| Bulk-delete the last `count` (1–100) messages in the channel. The reply is *ephemeral* (visible only to the invoker). |

`/purge` requires the **Manage Messages** permission and is hidden from members without it. Messages
older than 14 days are deleted one at a time, since Discord's bulk-delete endpoint rejects them.

## Run

```sh
cp config.example.yaml config.yaml
# edit config.yaml with your rules
DISCORD_TOKEN=... go run ./cmd/zensur
```

## Extending

The matching engine in `internal/censor` is independent of Discord. Add a new normalization stage in
`normalize.go`, a new match `Mode` in `matcher.go`, or a new `Action` (with handling in
`internal/bot/bot.go::process`). The semantic filter lives in `internal/censor/llm.go` (filter
logic, prompts, verdict parsing) and `internal/censor/llm_providers.go` (one adapter per vendor);
adding a provider means implementing the `chatProvider` interface and registering it in
`newProvider`.

Message handling lives in `internal/bot/bot.go`; the guild/channel metadata guard lives in
`internal/bot/metadata.go`. To guard additional metadata fields, add them to the `metaField` checks
in the relevant `on*Update` handler.
