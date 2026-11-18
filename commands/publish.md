---
description: Render this repo's Backlog.md backlog and publish it as an Artifact now
allowed-tools: Bash(node:*), Artifact
disable-model-invocation: true
---

Publish instruction from the backlog-artifact plugin:

!`node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" instructions`

If no instruction appears above, run `node "${CLAUDE_PLUGIN_ROOT}/scripts/backlog-artifact.mjs" instructions` with Bash and use its output. Follow the instruction. If it says no backlog was found, tell the user that in one line and stop.
