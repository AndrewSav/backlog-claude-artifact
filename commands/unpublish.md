---
description: Remove this repo's published backlog and stop publishing it from this project
allowed-tools: Bash(node:*), Artifact
disable-model-invocation: true
---

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" request unpublish`

If no text appears above, run `node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" request unpublish` with Bash and follow its output.
