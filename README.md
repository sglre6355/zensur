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

## Configuration

Env vars:

| Var              | Required | Default        | Notes                                        |
|------------------|----------|----------------|----------------------------------------------|
| `DISCORD_TOKEN`  | yes      | —              | Bot token from Discord Developer Portal      |
| `ZENSUR_CONFIG`  | no       | `./config.yaml`| Path to YAML config                          |
| `LOG_LEVEL`      | no       | `info`         | `debug`, `info`, `warn`, `error`             |

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
`internal/bot/bot.go::process`).

Message handling lives in `internal/bot/bot.go`; the guild/channel metadata guard lives in
`internal/bot/metadata.go`. To guard additional metadata fields, add them to the `metaField` checks
in the relevant `on*Update` handler.
