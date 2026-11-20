---
description: Remove this repo's published backlog and stop publishing it from this project
allowed-tools: Bash(node:*), Artifact
disable-model-invocation: true
---

Unpublish result from the backlog-artifact plugin:

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" unpublish`

If no result appears above, run `node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" unpublish` with Bash and use its output. If it asks you to delete an artifact, do that with the Artifact tool. Then tell the user the outcome in one or two lines.
