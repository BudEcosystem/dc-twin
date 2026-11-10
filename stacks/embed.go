// Package stacks embeds the stack definitions (docs/spec/13-schemas.md — Stack).
package stacks

import "embed"

//go:embed */stack.yaml */policy.yaml
var FS embed.FS
