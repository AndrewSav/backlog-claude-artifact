---
description: Publish this repo's Backlog.md backlog now, and turn publishing back on if it was off
allowed-tools: Bash(node:*), Artifact
disable-model-invocation: true
---

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" request publish`

If no text appears above, run `node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" request publish` with Bash and follow its output.
