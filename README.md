# backlog-artifact

A Claude Code plugin that publishes a [Backlog.md](https://github.com/MrLesk/Backlog.md) backlog as a page that works well on a phone, and republishes it automatically after turns that change the backlog. It publishes to one of two places:

- **A claude.ai Artifact.** Nothing to host, but each publish costs one extra Claude step.
- **Your own backlog server** (`server/`, a small Go service). The hook uploads the page directly, so publishing costs no model tokens at all. The server lists every published project and serves each page behind Google sign-in or view-key links.

The page has no swimlanes. Each state in your `backlog/config.yml` gets a toggle chip with a task count, and you can turn on any combination. All states except the final one (usually Done) are on by default. Statuses that appear in tasks but not in the config get chips too, so no task is hidden. You can sort by last change, creation time, task number (subtasks sort numerically, so 1, 1.2, 1.10, 2), priority, board order or title, in either direction, and filter by ID, title or label. Tap a task to see its description, acceptance criteria, plan and notes.

## Install

Inside Claude Code:

```
/plugin marketplace add AndrewSav/backlog-claude-artifact
/plugin install backlog-artifact@backlog-artifact
```

Or from a terminal:

```
claude plugin marketplace add AndrewSav/backlog-claude-artifact
claude plugin install backlog-artifact@backlog-artifact
```

## Update

Inside Claude Code:

```
/plugin marketplace update backlog-artifact
```

This updates the plugin and reloads it, so no restart is needed.

Or from a terminal (the update loads in your next session):

```
claude plugin marketplace update backlog-artifact
claude plugin update backlog-artifact@backlog-artifact
```

## Requirements

Requires Node.js 18 or newer on `PATH`. Backlog.md is installed through npm or bun, so you most likely have it already.

## How it works (Artifact mode)

Only Claude's Artifact tool can publish an artifact, so a hook alone can't do it. At the end of every turn, a `Stop` hook:

1. Exits immediately if the project has no `backlog/config.yml`.
2. Renders the page to `.claude/backlog-artifact/backlog.html` in the project. It also adds that folder to `.git/info/exclude`, so the file never shows up in `git status` and nothing is committed.
3. Stops there if the backlog is unchanged since the last publish, or if the last publish was less than `publish_interval_minutes` ago.
4. Otherwise asks Claude to publish the file. Claude makes one Artifact call and ends the turn.

A `PostToolUse` hook on the Artifact tool saves the artifact's URL, so later publishes update the same page, including from new sessions.

Headless runs (`claude -p`, the Agent SDK) have no Artifact tool, so there the hook only renders the page.

### Cost

Each publish is one extra Claude step. It re-reads the session's context (mostly from the prompt cache) and adds a few hundred output tokens. In a long session with a large context that cost adds up, which is why the default limit is one publish every 15 minutes. Changes made in between are included in the next publish. Turns that don't change the backlog cost nothing. Edits made at the very end of a session are published on the next turn that ends after the interval, or straight away with `/backlog-artifact:publish`.

## Server mode

Set `publish_target` to `server` and `server_url` to your server. At the end of every turn that changed the backlog, the `Stop` hook uploads the page itself, in well under a second, without involving Claude. It works in headless runs too. The first publish and any new error show up as a one-line message in the session, and errors are also logged to `.claude/backlog-artifact/publish.log`. After a failure the hook waits a minute before retrying.

**Publishing auth**, in order:

1. `server_key`: a static publish token, sent as a Bearer token.
2. Otherwise an SSH signature. The hook runs `ssh-keygen -Y sign` with a public key, and ssh-agent signs with the matching private key, so the plugin never reads a private key file and never needs a passphrase. That is the same key setup `git push` to GitHub uses. The key is `ssh_key` if set, otherwise the first key `ssh-add -L` lists. On Windows the hook uses the Windows OpenSSH tools in `Program Files\OpenSSH` or `System32\OpenSSH`, because Git Bash's copies can't reach the Windows agent. Each signature covers the method, the project, a timestamp and a hash of the upload, and the server accepts it once, within five minutes.

The project's address is its `project_name` in lower case with hyphens, for example `/p/blueprinceandrew`. The same backlog published from two machines lands on one page.

### Running the server

The image is `andrewsav/backlog-server`, built by `.github/workflows/server-image.yml` on every push to `main` that touches the server's code (`server/`, `assets.go`, `go.mod`, or the shared `plugin/shared/` script). It builds from the repo root: `docker build -f server/Dockerfile .`. That workflow needs the `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` repository secrets. It listens on port 8080 and is meant to sit behind a TLS-terminating proxy such as Traefik. The container starts as root only to take ownership of `DATA_DIR`, since Docker creates missing bind-mount folders as root. It then drops to uid 65532 before serving, so the data folder needs no manual `chown`. The config files must be readable by that user, which mode 644 (the default) is.

| Variable | Default | Meaning |
|---|---|---|
| `BASE_URL` | `http://localhost:8080` | The public address. Cookies are `Secure` when it starts with https. |
| `SESSION_SECRET` | (required) | 32+ characters for signing cookies (`openssl rand -hex 32`). |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | empty | Google OAuth web client, redirect URI `<BASE_URL>/auth/callback`. Leave both empty to turn off Google sign-in. |
| `VIEW_ALLOWED_EMAILS` | empty | Google accounts allowed to view, comma-separated. |
| `DATA_DIR` | `/data` | Where pages are stored. |
| `PUBLISH_AUTHORIZED_KEYS_FILE` | `/config/publish_authorized_keys` | SSH public keys that may publish (authorized_keys format). |
| `PUBLISH_TOKENS_FILE` | `/config/publish_tokens` | Static publish tokens, one `name:token` per line. |
| `VIEW_TOKENS_FILE` | `/config/view_tokens` | Static view tokens, one `name:token` per line. |
| `RUN_AS_UID`, `RUN_AS_GID` | `65532` | The user to drop to after taking ownership of `DATA_DIR`. `0` keeps running as root. |

The three files are re-read when they change, so adding or removing a key needs no restart. Removing a view token or an allowed email also signs out browsers that used it. Publish and view tokens are separate lists: a view token can never publish, and a publish token can't view.

| Route | Who | What |
|---|---|---|
| `GET /` | viewer | List of published projects with per-state counts, sortable and filterable |
| `GET /p/<slug>` | viewer | The backlog page |
| `GET /api/projects`, `GET /api/projects/<slug>` | viewer | JSON: the list, or one project with its full task data |
| `PUT`/`DELETE /api/projects/<slug>` | publisher | Publish or remove a project |
| `?key=<view token>` on any page | anyone | Signs the browser in for 30 days and redirects without the key |
| `/auth/login`, `/auth/logout` | anyone | Google sign-in and sign-out |

Run the server's tests with `go test ./...` from the repo root.

## Commands

- `/backlog-artifact:publish` renders and publishes now, ignoring the interval. In server mode it uploads directly. In a project that was unpublished, it turns publishing back on.
- `/backlog-artifact:unpublish` removes this project's page from the server (or has Claude delete its artifact) and turns publishing off for this project only. Other projects keep publishing. Both commands do their work in the `Stop` hook at the end of the turn, because plugin options reach only hook processes, and they show the result there. The setting lives in `.claude/backlog-artifact/state.json`, which is kept out of git, so it never touches the repo. Running it before the first publish just opts the project out. To remove a page for a project that isn't on this machine, run `node plugin/scripts/backlog-artifact.mjs unpublish --slug <slug>` with the plugin options set as `CLAUDE_PLUGIN_OPTION_*` environment variables.

To keep the plugin out of a project completely, so it doesn't even render the page, disable it in that project's `.claude/settings.local.json`: `{ "enabledPlugins": { "backlog-artifact@backlog-artifact": false } }`.

## Options

Set these when you enable the plugin, or later in `/plugin` → backlog-artifact → configure.

| Option | Default | Effect |
|---|---|---|
| `publish_target` | `artifact` | `artifact` or `server`. |
| `auto_publish` | `true` | Publish automatically. When off, the page is still rendered locally and you publish with the command. |
| `publish_interval_minutes` | `15` | Artifact mode: minimum minutes between automatic publishes, one of 0, 5, 15, 30 or 60. `0` publishes after every turn that changed the backlog. Server mode publishes every change. |
| `server_url` | empty | Server mode: the server's address, for example `https://backlog.example.com`. |
| `server_key` | empty | Server mode: a static publish token, kept in secure storage. Leave empty to sign with SSH. |
| `ssh_key` | empty | Server mode without a token: the public key file to sign with. Empty means the first key in ssh-agent. |
| `include_drafts` | `false` | Show `backlog/drafts` as a Draft chip. |
| `include_archived` | `false` | Show `backlog/archive/tasks` as an Archived chip (off by default even when included). |

## Avoiding the permission prompt

If Claude Code asks for permission every time Claude publishes, add the Artifact tool to the allow list in `~/.claude/settings.json`:

```json
{ "permissions": { "allow": ["Artifact"] } }
```

## Development

The plugin is everything under `plugin/`; that folder is all an install copies. The server is `server/`, built from the Go module at the repo root. `plugin/shared/list-controls.js` (sort menu, direction button, filter box) is used by both: the plugin inlines it into the backlog page, and the server embeds it through `assets.go` for its project list.

```
node --test plugin/test/*.test.mjs
node plugin/scripts/backlog-artifact.mjs render path/to/repo   # prints a summary
claude --plugin-dir plugin                                      # try it in a session
```

Set `BACKLOG_ARTIFACT_OUT_DIR` to write renders somewhere other than the repo.

## License

MIT
