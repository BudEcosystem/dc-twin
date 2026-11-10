// Package policies embeds the critic's generic policies (docs/spec/17-critic.md — The generic
// policies): what no datacenter software should do, applied to every run unless a policy
// disables a rule by name with a reason (D67).
package policies

import "embed"

//go:embed generic/*.yaml
var FS embed.FS
