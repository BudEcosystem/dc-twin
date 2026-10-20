package bms

import (
	"encoding/json"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/scenario"
)

// PublishedValues gives the scenario runner the value messages on topics matching filter, as
// the publisher logged them (08: bms observations are made at the publisher's log).
func (a *Adapter) PublishedValues(filter string, since time.Time) []scenario.PublishedValue {
	var out []scenario.PublishedValue
	for _, p := range a.Published(filter, since) {
		if p.Kind != "value" && p.Kind != "mirror" {
			continue
		}
		var v map[string]any
		if json.Unmarshal(p.Payload, &v) != nil {
			continue
		}
		value, ok := v["value"]
		if !ok { // a mirror's metadata: no value to observe
			continue
		}
		out = append(out, scenario.PublishedValue{Topic: p.Topic, Value: value, At: p.Time})
	}
	return out
}
