---
name: pp-superhuman
description: "Superhuman email from your terminal or Claude Code, backed by a local SQLite store and agent-native JSON I/O. Trigger phrases: `check my email`, `what's in my inbox`, `draft a reply to <thread>`, `send <draft> with undo`, `snooze this thread`, `use superhuman`, `run superhuman`."
author: "Matt Van Horn"
license: "Apache-2.0"
argument-hint: "<command> [args] | install cli|mcp"
allowed-tools: "Read Bash"
metadata:
  openclaw:
    requires:
      bins:
        - superhuman-pp-cli
---

# Superhuman — Printing Press CLI

## Prerequisites: Install the CLI

This skill drives the `superhuman-pp-cli` binary. **You must verify the CLI is installed before invoking any command from this skill.** If it is missing, install it first:

1. Install via the Printing Press installer:
   ```bash
   npx -y @mvanhorn/printing-press install superhuman --cli-only
   ```
2. Verify: `superhuman-pp-cli --version`
3. Ensure `$GOPATH/bin` (or `$HOME/go/bin`) is on `$PATH`.

If the `npx` install fails before this CLI has a public-library category, install Node or use the category-specific Go fallback after publish.

If `--version` reports "command not found" after install, the install step did not put the binary on `$PATH`. Do not proceed with skill commands until verification succeeds.

Read, draft, send, and snooze Superhuman email from your terminal or Claude Code. Pair a JWT lifted from your logged-in Chrome session with a local SQLite store for offline thread search, draft management, and Ask AI semantic queries.

## When to Use This CLI

Pick the Superhuman CLI when you want email read/draft/respond access from a terminal or Claude Code, scriptable thread search backed by a local store, capability discovery via `which`, or Ask AI semantic search piped into other tools.

## Unique Capabilities

These capabilities aren't available in any other tool for this API.
- **`workflow`** — Compound commands that combine multiple API operations into one verb (see `workflow --help`).

  ```bash
  superhuman-pp-cli workflow --help
  ```
- **`which`** — Resolve a natural-language capability query to the best matching command from this CLI's curated feature index.

  ```bash
  superhuman-pp-cli which 'snooze a thread for tomorrow'
  ```

## Command Reference

**ai** — Semantic search via Ask AI

- `superhuman-pp-cli ai` — Ask AI semantic search (SSE stream)

**attachments** — Upload, list, download attachments

- `superhuman-pp-cli attachments` — Upload an attachment for a draft

**drafts** — Drafts — create, update, send, delete

- `superhuman-pp-cli drafts list` — List drafts
- `superhuman-pp-cli drafts write` — Create or update a draft (write to draft path)

**messages** — Individual messages within threads

- `superhuman-pp-cli messages` — Send a draft (with optional undo delay)

**reminders** — Snooze reminders for threads

- `superhuman-pp-cli reminders cancel` — Cancel a snooze (un-snooze a thread)
- `superhuman-pp-cli reminders create` — Create a snooze reminder for a thread

**teams** — Team and account info

- `superhuman-pp-cli teams` — List teams the user belongs to

**threads** — Email threads — read, list, search, archive, label

- `superhuman-pp-cli threads get` — Get a thread by ID
- `superhuman-pp-cli threads list` — List recent threads

**users** — User account state

- `superhuman-pp-cli users` — User achievements / gamification state


### Finding the right command

When you know what you want to do but not which command does it, ask the CLI directly:

```bash
superhuman-pp-cli which "<capability in your own words>"
```

`which` resolves a natural-language capability query to the best matching command from this CLI's curated feature index. Exit code `0` means at least one match; exit code `2` means no confident match — fall back to `--help` or use a narrower query.

## Recipes


### Fast triage view across recent threads

```bash
superhuman-pp-cli threads list --limit 100 --json --select id,subject,snippet,participants
```

Pull a wide swath of recent threads with only the fields you need; pipe to jq or an LLM for further triage.

### Draft from stdin and send with an undo window

```bash
echo 'Body text here' | superhuman-pp-cli drafts write --to teammate@example.com --subject 'Update' --stdin && superhuman-pp-cli messages --draft-message-id <draft-id> --delay 30
```

Write a draft from a piped body, then send it with a 30s undo window: agents can abort before the send fires.

### Snooze a thread for later

```bash
superhuman-pp-cli reminders create --thread-id <thread-id> --trigger-at 2026-05-13T09:00:00Z
```

Schedule a thread to resurface at a specific time; pair with `reminders cancel` to un-snooze.

### Discover a capability by description

```bash
superhuman-pp-cli which 'send an email with an undo window'
```

Resolve a natural-language capability query to the best matching command from this CLI's curated feature index.

### Ask AI semantic search across mail

```bash
superhuman-pp-cli ai --query 'what did Alice say about pricing last week' --agent
```

Run a semantic query through Superhuman's Ask AI surface; `--agent` gives you JSON streams pipeable to other tools.

## Auth Setup

Superhuman has no public API key. Lift your Firebase JWT from a logged-in Chrome session, then persist it locally. Run `superhuman-pp-cli auth setup` for step-by-step instructions; the short version:

```bash
# 1. In Chrome DevTools, copy the JWT from Application > Local Storage > mail.superhuman.com
export SUPERHUMAN_JWT="eyJ..."
superhuman-pp-cli auth set-token "$SUPERHUMAN_JWT"
superhuman-pp-cli doctor
```

Firebase JWTs expire after ~1 hour; the CLI returns 401 when stale and you re-paste the current value.

## Agent Mode

Add `--agent` to any command. Expands to: `--json --compact --no-input --no-color --yes`.

- **Pipeable** — JSON on stdout, errors on stderr
- **Filterable** — `--select` keeps a subset of fields. Dotted paths descend into nested structures; arrays traverse element-wise. Critical for keeping context small on verbose APIs:

  ```bash
  superhuman-pp-cli drafts list --agent --select id,name,status
  ```
- **Previewable** — `--dry-run` shows the request without sending
- **Offline-friendly** — sync/search commands can use the local SQLite store when available
- **Non-interactive** — never prompts, every input is a flag
- **Explicit retries** — use `--idempotent` only when an already-existing create should count as success

### Response envelope

Commands that read from the local store or the API wrap output in a provenance envelope:

```json
{
  "meta": {"source": "live" | "local", "synced_at": "...", "reason": "..."},
  "results": <data>
}
```

Parse `.results` for data and `.meta.source` to know whether it's live or local. A human-readable `N results (live)` summary is printed to stderr only when stdout is a terminal — piped/agent consumers get pure JSON on stdout.

## Agent Feedback

When you (or the agent) notice something off about this CLI, record it:

```
superhuman-pp-cli feedback "the --since flag is inclusive but docs say exclusive"
superhuman-pp-cli feedback --stdin < notes.txt
superhuman-pp-cli feedback list --json --limit 10
```

Entries are stored locally at `~/.superhuman-pp-cli/feedback.jsonl`. They are never POSTed unless `SUPERHUMAN_FEEDBACK_ENDPOINT` is set AND either `--send` is passed or `SUPERHUMAN_FEEDBACK_AUTO_SEND=true`. Default behavior is local-only.

Write what *surprised* you, not a bug report. Short, specific, one line: that is the part that compounds.

## Output Delivery

Every command accepts `--deliver <sink>`. The output goes to the named sink in addition to (or instead of) stdout, so agents can route command results without hand-piping. Three sinks are supported:

| Sink | Effect |
|------|--------|
| `stdout` | Default; write to stdout only |
| `file:<path>` | Atomically write output to `<path>` (tmp + rename) |
| `webhook:<url>` | POST the output body to the URL (`application/json` or `application/x-ndjson` when `--compact`) |

Unknown schemes are refused with a structured error naming the supported set. Webhook failures return non-zero and log the URL + HTTP status on stderr.

## Named Profiles

A profile is a saved set of flag values, reused across invocations. Use it when a scheduled agent calls the same command every run with the same configuration - HeyGen's "Beacon" pattern.

```
superhuman-pp-cli profile save briefing --json
superhuman-pp-cli --profile briefing drafts list
superhuman-pp-cli profile list --json
superhuman-pp-cli profile show briefing
superhuman-pp-cli profile delete briefing --yes
```

Explicit flags always win over profile values; profile values win over defaults. `agent-context` lists all available profiles under `available_profiles` so introspecting agents discover them at runtime.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 2 | Usage error (wrong arguments) |
| 3 | Resource not found |
| 4 | Authentication required |
| 5 | API error (upstream issue) |
| 7 | Rate limited (wait and retry) |
| 10 | Config error |

## Argument Parsing

Parse `$ARGUMENTS`:

1. **Empty, `help`, or `--help`** → show `superhuman-pp-cli --help` output
2. **Starts with `install`** → ends with `mcp` → MCP installation; otherwise → see Prerequisites above
3. **Anything else** → Direct Use (execute as CLI command with `--agent`)

## MCP Server Installation

Install the MCP binary from this CLI's published public-library entry or pre-built release, then register it:

```bash
claude mcp add superhuman-pp-mcp -- superhuman-pp-mcp
```

Verify: `claude mcp list`

## Direct Use

1. Check if installed: `which superhuman-pp-cli`
   If not found, offer to install (see Prerequisites at the top of this skill).
2. Match the user query to the best command from the Unique Capabilities and Command Reference above.
3. Execute with the `--agent` flag:
   ```bash
   superhuman-pp-cli <command> [subcommand] [args] --agent
   ```
4. If ambiguous, drill into subcommand help: `superhuman-pp-cli <command> --help`.
