# Superhuman CLI

**Superhuman email from your terminal or Claude Code, backed by a local SQLite store and agent-native JSON I/O.**

Read, draft, send, and snooze Superhuman email from your terminal or Claude Code. Pair a Firebase JWT lifted from your logged-in Chrome session with a local SQLite store for offline thread search, draft management, and Ask AI semantic queries.

Printed by [@mvanhorn](https://github.com/mvanhorn) (Matt Van Horn).

## Install

The recommended path installs both the `superhuman-pp-cli` binary and the `pp-superhuman` agent skill in one shot:

```bash
npx -y @mvanhorn/printing-press install superhuman
```

For CLI only (no skill):

```bash
npx -y @mvanhorn/printing-press install superhuman --cli-only
```


### Without Node

The generated install path is category-agnostic until this CLI is published. If `npx` is not available before publish, install Node or use the category-specific Go fallback from the public-library entry after publish.

### Pre-built binary

Download a pre-built binary for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/superhuman-current). On macOS, clear the Gatekeeper quarantine: `xattr -d com.apple.quarantine <binary>`. On Unix, mark it executable: `chmod +x <binary>`.

<!-- pp-hermes-install-anchor -->
## Install for Hermes

From the Hermes CLI:

```bash
hermes skills install mvanhorn/printing-press-library/cli-skills/pp-superhuman --force
```

Inside a Hermes chat session:

```bash
/skills install mvanhorn/printing-press-library/cli-skills/pp-superhuman --force
```

## Install for OpenClaw

Tell your OpenClaw agent (copy this):

```
Install the pp-superhuman skill from https://github.com/mvanhorn/printing-press-library/tree/main/cli-skills/pp-superhuman. The skill defines how its required CLI can be installed.
```

## Authentication

Superhuman has no public API key. Lift your Firebase JWT from a logged-in Chrome session, then persist it locally. Run `superhuman-pp-cli auth setup` for step-by-step instructions.

```bash
# 1. In Chrome DevTools, copy the JWT from Application > Local Storage > mail.superhuman.com
export SUPERHUMAN_JWT="eyJ..."
superhuman-pp-cli auth set-token "$SUPERHUMAN_JWT"
superhuman-pp-cli doctor
```

Firebase JWTs expire after ~1 hour; the CLI returns 401 when stale and you re-paste the current value.

## Quick Start

```bash
# One-time: get the JWT-from-Chrome instructions
superhuman-pp-cli auth setup


# Persist the JWT lifted from your Chrome session
superhuman-pp-cli auth set-token "$SUPERHUMAN_JWT"


# Confirm auth and connectivity are green
superhuman-pp-cli doctor


# Populate the local SQLite store for offline analysis
superhuman-pp-cli sync --full


# List recent threads as structured JSON
superhuman-pp-cli threads list --limit 20 --json


# See pending drafts before sending
superhuman-pp-cli drafts list --json

```

## Unique Features

These capabilities aren't available in any other tool for this API.
- **`workflow`** — Compound commands that combine multiple API operations into one verb (see `workflow --help`).

  ```bash
  superhuman-pp-cli workflow --help
  ```
- **`which`** — Resolve a natural-language capability query to the best matching command from this CLI's curated feature index.

  ```bash
  superhuman-pp-cli which 'snooze a thread for tomorrow'
  ```

## Usage

Run `superhuman-pp-cli --help` for the full command reference and flag list.

## Commands

### ai

Semantic search via Ask AI

- **`superhuman-pp-cli ai ask`** - Ask AI semantic search (SSE stream)

### attachments

Upload, list, download attachments

- **`superhuman-pp-cli attachments upload`** - Upload an attachment for a draft

### drafts

Drafts — create, update, send, delete

- **`superhuman-pp-cli drafts list`** - List drafts
- **`superhuman-pp-cli drafts write`** - Create or update a draft (write to draft path)

### messages

Individual messages within threads

- **`superhuman-pp-cli messages send`** - Send a draft (with optional undo delay)

### reminders

Snooze reminders for threads

- **`superhuman-pp-cli reminders cancel`** - Cancel a snooze (un-snooze a thread)
- **`superhuman-pp-cli reminders create`** - Create a snooze reminder for a thread

### teams

Team and account info

- **`superhuman-pp-cli teams suggest`** - List teams the user belongs to

### threads

Email threads — read, list, search, archive, label

- **`superhuman-pp-cli threads get`** - Get a thread by ID
- **`superhuman-pp-cli threads list`** - List recent threads

### users

User account state

- **`superhuman-pp-cli users achievements`** - User achievements / gamification state


## Output Formats

```bash
# Human-readable table (default in terminal, JSON when piped)
superhuman-pp-cli drafts list

# JSON for scripting and agents
superhuman-pp-cli drafts list --json

# Filter to specific fields
superhuman-pp-cli drafts list --json --select id,name,status

# Dry run — show the request without sending
superhuman-pp-cli drafts list --dry-run

# Agent mode — JSON + compact + no prompts in one flag
superhuman-pp-cli drafts list --agent
```

## Agent Usage

This CLI is designed for AI agent consumption:

- **Non-interactive** - never prompts, every input is a flag
- **Pipeable** - `--json` output to stdout, errors to stderr
- **Filterable** - `--select id,name` returns only fields you need
- **Previewable** - `--dry-run` shows the request without sending
- **Explicit retries** - add `--idempotent` to create retries when a no-op success is acceptable
- **Confirmable** - `--yes` for explicit confirmation of destructive actions
- **Piped input** - write commands can accept structured input when their help lists `--stdin`
- **Offline-friendly** - sync/search commands can use the local SQLite store when available
- **Agent-safe by default** - no colors or formatting unless `--human-friendly` is set

Exit codes: `0` success, `2` usage error, `3` not found, `4` auth error, `5` API error, `7` rate limited, `10` config error.

## Use with Claude Code

Install the focused skill — it auto-installs the CLI on first invocation:

```bash
npx skills add mvanhorn/printing-press-library/cli-skills/pp-superhuman -g
```

Then invoke `/pp-superhuman <query>` in Claude Code. The skill is the most efficient path — Claude Code drives the CLI directly without an MCP server in the middle.

<details>
<summary>Use as an MCP server in Claude Code (advanced)</summary>

If you'd rather register this CLI as an MCP server in Claude Code, install the MCP binary first:


Install the MCP binary from this CLI's published public-library entry or pre-built release.

Then register it:

```bash
claude mcp add superhuman superhuman-pp-mcp -e SUPERHUMAN_JWT=<your-token>
```

</details>

## Use with Claude Desktop

This CLI ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle — Claude Desktop's standard format for one-click MCP extension installs (no JSON config required).

To install:

1. Download the `.mcpb` for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/superhuman-current).
2. Double-click the `.mcpb` file. Claude Desktop opens and walks you through the install.
3. Fill in `SUPERHUMAN_JWT` when Claude Desktop prompts you.

Requires Claude Desktop 1.0.0 or later. Pre-built bundles ship for macOS Apple Silicon (`darwin-arm64`) and Windows (`amd64`, `arm64`); for other platforms, use the manual config below.

<details>
<summary>Manual JSON config (advanced)</summary>

If you can't use the MCPB bundle (older Claude Desktop, unsupported platform), install the MCP binary and configure it manually.


Install the MCP binary from this CLI's published public-library entry or pre-built release.

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "superhuman": {
      "command": "superhuman-pp-mcp",
      "env": {
        "SUPERHUMAN_JWT": "<your-key>"
      }
    }
  }
}
```

</details>

## Health Check

```bash
superhuman-pp-cli doctor
```

Verifies configuration, credentials, and connectivity to the API.

## Configuration

Config file: `~/.config/superhuman-pp-cli/config.toml`

Static request headers can be configured under `headers`; per-command header overrides take precedence.

Environment variables:

| Name | Kind | Required | Description |
| --- | --- | --- | --- |
| `SUPERHUMAN_JWT` | per_call | Yes | Set to your API credential. |

## Troubleshooting
**Authentication errors (exit code 4)**
- Run `superhuman-pp-cli doctor` to check credentials
- Verify the environment variable is set: `echo $SUPERHUMAN_JWT`
**Not found errors (exit code 3)**
- Check the resource ID is correct
- Run the `list` command to see available items

### API-specific

- **401 on every backend call** - Run `superhuman-pp-cli doctor` to confirm the JWT is set, then re-paste a fresh JWT from Chrome DevTools and re-run `auth set-token`. Firebase JWTs expire roughly hourly.
- **`auth setup` says no token configured** - Set `SUPERHUMAN_JWT` in your environment or persist it with `superhuman-pp-cli auth set-token <jwt>`.
- **`threads list` is empty after sync** - Confirm `doctor --json` shows a recent sync; if not, re-run `sync --full`.
- **`ai` returns 400 invalid-question-event-id** - The JWT may be from a different account or expired; re-paste a fresh JWT and retry.

---

## Sources & Inspiration

This CLI was built by studying these projects and resources:

- [**edwinhu/superhuman-cli**](https://github.com/edwinhu/superhuman-cli) — TypeScript (3 stars)
- [**superhuman/mcp-mail**](https://github.com/superhuman/mcp-mail) — JavaScript
- [**himalaya**](https://github.com/pimalaya/himalaya) — Rust
- [**aerc**](https://git.sr.ht/~rjarry/aerc) — Go

Generated by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press)
