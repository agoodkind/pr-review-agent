// Package templates embeds review output templates.
package templates

import _ "embed"

// RawResult defines the multiline result cell.
//
//go:embed raw.result.md.tmpl
var RawResult string
