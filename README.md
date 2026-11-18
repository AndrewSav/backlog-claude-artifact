# backlog-artifact

A Claude Code plugin that publishes a [Backlog.md](https://github.com/MrLesk/Backlog.md) backlog as a claude.ai Artifact. The page works well on a phone, and it is republished automatically after turns that change the backlog.

The page has no swimlanes. Each state in your `backlog/config.yml` gets a toggle chip with a task count, and you can turn on any combination. All states except the final one (usually Done) are on by default. Statuses that appear in tasks but not in the config get chips too, so no task is hidden. You can sort by last change, creation time, task number (subtasks sort numerically, so 1, 1.2, 1.10, 2), priority, board order or title, in either direction, and filter by ID, title or label. Tap a task to see its description, acceptance criteria, plan and notes.

## Install

```
/plugin marketplace add AndrewSav/backlog-claude-artifact
/plugin install backlog-artifact@backlog-artifact
```

Requires Node.js 18 or newer on `PATH`. Backlog.md is installed through npm or bun, so you most likely have it already.

## How it works

Only Claude's Artifact tool can publish an artifact, so a hook alone can't do it. At the end of every turn, a `Stop` hook:

1. Exits immediately if the project has no `backlog/config.yml`.
2. Renders the page to `.claude/backlog-artifact/backlog.html` in the project. It also adds that folder to `.git/info/exclude`, so the file never shows up in `git status` and nothing is committed.
3. Stops there if the backlog is unchanged since the last publish, or if the last publish was less than `publish_interval_minutes` ago.
4. Otherwise asks Claude to publish the file. Claude makes one Artifact call and ends the turn.

A `PostToolUse` hook on the Artifact tool saves the artifact's URL, so later publishes update the same page, including from new sessions.

Headless runs (`claude -p`, the Agent SDK) have no Artifact tool, so there the hook only renders the page.

### Cost

Each publish is one extra Claude step. It re-reads the session's context (mostly from the prompt cache) and adds a few hundred output tokens. In a long session with a large context that cost adds up, which is why the default limit is one publish every 15 minutes. Changes made in between are included in the next publish. Turns that don't change the backlog cost nothing. Edits made at the very end of a session are published on the next turn that ends after the interval, or straight away with `/backlog-artifact:publish`.

## Commands

- `/backlog-artifact:publish` renders and publishes now, ignoring the interval.

## Options

Set these when you enable the plugin, or later in `/plugin` → backlog-artifact → configure.

| Option | Default | Effect |
|---|---|---|
| `auto_publish` | `true` | Publish automatically. When off, the page is still rendered locally and you publish with the command. |
| `publish_interval_minutes` | `15` | Minimum minutes between automatic publishes: 0, 5, 15, 30 or 60. `0` publishes after every turn that changed the backlog. |
| `include_drafts` | `false` | Show `backlog/drafts` as a Draft chip. |
| `include_archived` | `false` | Show `backlog/archive/tasks` as an Archived chip (off by default even when included). |

## Avoiding the permission prompt

If Claude Code asks for permission every time Claude publishes, add the Artifact tool to the allow list in `~/.claude/settings.json`:

```json
{ "permissions": { "allow": ["Artifact"] } }
```

## Development

```
node --test test/*.test.mjs
node scripts/backlog-artifact.mjs render path/to/repo   # prints a summary
claude --plugin-dir .                                    # try it in a session
```

Set `BACKLOG_ARTIFACT_OUT_DIR` to write renders somewhere other than the repo.

## License

MIT
