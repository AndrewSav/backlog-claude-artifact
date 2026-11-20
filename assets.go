// Package assets holds files the backlog server shares with the plugin.
// go:embed can't reach outside the directory of the Go file that embeds, so
// this lives at the repo root, above both plugin/ and server/.
package assets

import _ "embed"

// ListControls is the sort menu, direction button and filter box used by
// both the plugin's backlog page and the server's project list.
//
//go:embed plugin/shared/list-controls.js
var ListControls string
