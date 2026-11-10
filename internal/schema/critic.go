package schema

import (
	"path"
	"strings"
)

// The critic's files (docs/spec/17-critic.md): a Policy says what is right, a Campaign is a
// matrix of judged runs, and a stack's `critic` section says how the critic reads it.

// ---------------------------------------------------------------- request classes

// destructive canonical requests: they take a machine, its data or its management controller
// out of service, or cut a rack's supply (17 — Evidence).
var destructive = []string{"power.off.graceful", "power.off.force", "power.restart", "power.cycle", "ac.cycle",
	"erase.start", "tpm.clear", "bmc.defaults", "bmc.reset", "bios.reset", "firmware.update.*",
	"facility.isolate", "account.delete"}

// RequestClass is a canonical request's class: read, write or destructive (14, 17).
func RequestClass(canonical string) string {
	for _, d := range destructive {
		if ok, _ := path.Match(d, canonical); ok {
			return "destructive"
		}
	}
	switch {
	case canonical == "", canonical == "other":
		return "other"
	case strings.HasSuffix(canonical, ".read"), canonical == "event.sse", canonical == "task.read":
		return "read"
	}
	return "write"
}

// ---------------------------------------------------------------- the stack's critic section

// StackCritic is how the critic reads a stack: its standard aliases, its state vocabulary, who
// its components are on the wire, and its policy.
type StackCritic struct {
	// Aliases maps the standard aliases (machine, alerts, allocation, allocatable, node,
	// workload) to <component>.<accessor>.
	Aliases map[string]string `json:"aliases,omitempty"`
	// StateMap maps the stack's machine states onto the standard vocabulary.
	StateMap map[string]string `json:"stateMap,omitempty"`
	// Principals names components by the BMC user or source address they act as.
	Principals map[string]StackPrincipal `json:"principals,omitempty"`
	// Policy is the stack's policy file, beside its stack.yaml.
	Policy string `json:"policy,omitempty"`
}

type StackPrincipal struct {
	Users     []string `json:"users,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

// ---------------------------------------------------------------- Policy

type Policy struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   Metadata   `json:"metadata"`
	Spec       PolicySpec `json:"spec"`
}

type PolicySpec struct {
	Title      string             `json:"title,omitempty"`
	Extends    []string           `json:"extends,omitempty"`
	Disable    []PolicyDisable    `json:"disable,omitempty"`
	Playbook   []PlaybookEntry    `json:"playbook,omitempty"`
	Invariants []Invariant        `json:"invariants,omitempty"`
	Objectives []Objective        `json:"objectives,omitempty"`
	Weights    map[string]float64 `json:"weights,omitempty"`
	Gate       *PolicyGate        `json:"gate,omitempty"`
	Sampling   *PolicySampling    `json:"sampling,omitempty"`
	IsNot      []string           `json:"isNot,omitempty"`
}

// PolicyDisable turns a rule of an extended policy off, with the reason printed in every
// judgement (D67).
type PolicyDisable struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

type PolicyGate struct {
	Score string `json:"score,omitempty"` // e.g. ">=0.9"
}

type PolicySampling struct {
	Accessors string `json:"accessors,omitempty"` // period, default 2s
}

// PlaybookEntry is what a stack promises for one fault class.
type PlaybookEntry struct {
	On           string          `json:"on"` // fault id, glob
	Where        string          `json:"where,omitempty"`
	Acknowledged string          `json:"acknowledged,omitempty"`
	Mitigated    string          `json:"mitigated,omitempty"`
	Within       string          `json:"within,omitempty"`
	Then         *PlaybookThen   `json:"then,omitempty"`
	Permits      []ActionPattern `json:"permits,omitempty"`
	Forbids      []ActionPattern `json:"forbids,omitempty"`
	// Key: a mitigation missed fails the run (default true).
	Key *bool `json:"key,omitempty"`
}

type PlaybookThen struct {
	Recovered string `json:"recovered"`
	Within    string `json:"within"`
}

// ActionPattern matches the stack's requests while a fault is open. On and Except are CEL
// expressions over `fault` that give a selector.
type ActionPattern struct {
	Canonical string `json:"canonical,omitempty"` // glob, alternatives with |
	Class     string `json:"class,omitempty"`
	On        string `json:"on,omitempty"`
	Except    string `json:"except,omitempty"`
	When      string `json:"when,omitempty"`
}

// Invariant is one rule in one of the five temporal forms.
type Invariant struct {
	Name      string   `json:"name"`
	Dimension string   `json:"dimension,omitempty"` // correctness, safety, timeliness, restraint, hygiene, coverage
	Severity  string   `json:"severity,omitempty"`  // error (default) or warning
	Key       bool     `json:"key,omitempty"`
	Message   string   `json:"message,omitempty"`
	Requires  []string `json:"requires,omitempty"` // standard aliases, or "stack"

	Never     *NeverRule     `json:"never,omitempty"`
	Always    *AlwaysRule    `json:"always,omitempty"`
	LeadsTo   *LeadsToRule   `json:"leadsTo,omitempty"`
	AtMost    *AtMostRule    `json:"atMost,omitempty"`
	Sometimes *SometimesRule `json:"sometimes,omitempty"`
}

// EventFilter matches committed events. Canonical takes globs and | alternatives; On is a selector.
type EventFilter struct {
	Kind      string `json:"kind,omitempty"`
	Canonical string `json:"canonical,omitempty"`
	Class     string `json:"class,omitempty"`
	From      string `json:"from,omitempty"`
	Principal string `json:"principal,omitempty"`
	On        string `json:"on,omitempty"`
	Field     string `json:"field,omitempty"`
	To        any    `json:"to,omitempty"`
}

type NeverRule struct {
	Event EventFilter `json:"event"`
	Where string      `json:"where,omitempty"`
}

type AlwaysRule struct {
	Each  string `json:"each,omitempty"`
	Holds string `json:"holds"`
}

// Trigger is an event, a fault being injected, a condition's rising edge, or one of a fault's
// moments.
type Trigger struct {
	Event  *EventFilter `json:"event,omitempty"`
	Fault  string       `json:"fault,omitempty"`
	Where  string       `json:"where,omitempty"`
	Holds  string       `json:"holds,omitempty"`
	Moment string       `json:"moment,omitempty"` // exposed | acknowledged | firstAction | mitigated | recovered, alternatives with |
}

type LeadsToRule struct {
	When   Trigger `json:"when"`
	Then   Trigger `json:"then"`
	Within string  `json:"within"`
	Unless string  `json:"unless,omitempty"`
}

type AtMostRule struct {
	Event  EventFilter `json:"event"`
	Per    string      `json:"per,omitempty"` // device, device+canonical, principal, run (default run)
	Max    int         `json:"max"`
	Window string      `json:"window,omitempty"`
	Where  string      `json:"where,omitempty"`
}

type SometimesRule struct {
	Event *EventFilter `json:"event,omitempty"`
	Holds string       `json:"holds,omitempty"`
}

// Objective is a measure with pass and warn criteria.
type Objective struct {
	Name      string `json:"name,omitempty"`
	Measure   string `json:"measure"`
	Where     string `json:"where,omitempty"`     // CEL over fault, for per-fault measures
	Aggregate string `json:"aggregate,omitempty"` // max (default), p50, p95, min, sum
	Pass      string `json:"pass"`
	Warn      string `json:"warn,omitempty"`
	Key       bool   `json:"key,omitempty"`
	Dimension string `json:"dimension,omitempty"`
}

// ---------------------------------------------------------------- Campaign

type Campaign struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata"`
	Spec       CampaignSpec `json:"spec"`
}

type CampaignSpec struct {
	Estates   []string          `json:"estates"`
	Adapters  []string          `json:"adapters,omitempty"`
	Scenarios CampaignScenarios `json:"scenarios,omitempty"`
	Policy    string            `json:"policy,omitempty"`
	Subject   string            `json:"subject,omitempty"`
	Seeds     *CampaignSeeds    `json:"seeds,omitempty"`
	Stack     *CampaignStack    `json:"stack,omitempty"`
	Generate  *CampaignGenerate `json:"generate,omitempty"`
	Holdout   *CampaignHoldout  `json:"holdout,omitempty"`
	Baseline  *CampaignBaseline `json:"baseline,omitempty"`
	Budget    *CampaignBudget   `json:"budget,omitempty"`
}

type CampaignScenarios struct {
	Files []string `json:"files,omitempty"`
}

type CampaignSeeds struct {
	Count int   `json:"count,omitempty"`
	From  int64 `json:"from,omitempty"`
}

// CampaignStack starts the software under test for each cell. Placeholders {tcp:name} and
// {udp:name} are free loopback addresses chosen per cell, {state} the cell's state directory;
// they may appear in the command, in Outputs (the estate's stack outputs), in Server (every
// network that names a DHCP server) and in Broker (the BMS broker).
type CampaignStack struct {
	Command string            `json:"command"`
	Outputs map[string]string `json:"outputs,omitempty"`
	Server  string            `json:"server,omitempty"`
	Broker  string            `json:"broker,omitempty"`
	Ready   string            `json:"ready,omitempty"` // a URL that answers once the stack is up
}

// CampaignGenerate draws fault sequences from the catalogues; the invariants and the playbook
// are the oracle, and a violation is shrunk to a minimal scenario.
type CampaignGenerate struct {
	Runs          int           `json:"runs"`
	Select        string        `json:"select,omitempty"`
	Faults        []string      `json:"faults"` // fault ids or catalogue prefixes
	Count         int           `json:"count,omitempty"`
	Within        string        `json:"within,omitempty"`
	Settle        string        `json:"settle,omitempty"`
	MaxConcurrent int           `json:"maxConcurrent,omitempty"`
	Setup         []Step        `json:"setup,omitempty"`
	Shrink        bool          `json:"shrink,omitempty"`
	Stack         ScenarioStack `json:"stack,omitempty"`
}

type CampaignHoldout struct {
	Files []string `json:"files"`
}

type CampaignBaseline struct {
	File string `json:"file"`
}

type CampaignBudget struct {
	Wall string `json:"wall,omitempty"`
}
