// Package packs embeds the in-tree packs (docs/spec/06-packs-and-stacks.md). Each directory is
// one pack; captured mockups are not committed and are obtained by `dctwin assets fetch`.
package packs

import "embed"

//go:embed all:generic all:nvidia all:liteon all:oem-dell all:oem-hpe all:oem-lenovo all:oem-supermicro all:amd all:intel all:delta
var FS embed.FS
