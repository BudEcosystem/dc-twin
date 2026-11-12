// Package policies embeds the critic's policies (docs/spec/17-critic.md — The generic
// policies). generic/*.yaml is what no datacenter software should do, applied to every run
// unless a policy disables a rule by name with a reason (D67). day0/*.yaml judges a fabric
// brought up from the factory (18 — Judging); it is found by name like any embedded policy but
// is not applied by default (critic.GenericPolicies does not list it): a stack that does day 0
// extends it, or a run names it.
package policies

import "embed"

//go:embed generic/*.yaml day0/*.yaml
var FS embed.FS
