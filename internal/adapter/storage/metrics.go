package storage

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// serveMetrics is the system's Prometheus exporter: capacity, health, nodes, drives, objects and
// alerts, from the model, in the text exposition format. A generic system has no vendor exporter
// to imitate; the families are named storage_system_* and their labels carry the system's name.
func (s *server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	v := s.view()
	var b strings.Builder
	sys := `system="` + s.name + `"`
	family := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	family("storage_system_info", "gauge", "The storage system: its template, management API and fidelity.")
	fmt.Fprintf(&b, "storage_system_info{%s,template=%q,manager=%q,fidelity=%q,protocols=%q} 1\n", sys, v.Template, v.Manager, v.Fidelity, strings.Join(v.Protocols, ","))
	family("storage_system_health", "gauge", "Health: 0 OK, 1 Warning, 2 Critical.")
	fmt.Fprintf(&b, "storage_system_health{%s} %d\n", sys, healthCode(v.Health))
	family("storage_system_state", "gauge", "1 for the system's state (Online, Degraded, Offline).")
	for _, st := range []string{"Online", "Degraded", "Offline"} {
		fmt.Fprintf(&b, "storage_system_state{%s,state=%q} %d\n", sys, st, b2i(v.State == st))
	}
	family("storage_system_raw_bytes", "gauge", "Raw capacity of the pool's drives.")
	fmt.Fprintf(&b, "storage_system_raw_bytes{%s} %d\n", sys, v.RawBytes)
	family("storage_system_capacity_bytes", "gauge", "Usable capacity of the pool after protection.")
	fmt.Fprintf(&b, "storage_system_capacity_bytes{%s} %d\n", sys, v.UsableBytes)
	family("storage_system_used_bytes", "gauge", "Bytes the pool holds.")
	fmt.Fprintf(&b, "storage_system_used_bytes{%s} %d\n", sys, v.UsedBytes)
	family("storage_system_provisioned_bytes", "gauge", "Bytes the system's quotas and volumes reserve.")
	fmt.Fprintf(&b, "storage_system_provisioned_bytes{%s} %d\n", sys, v.ProvisionedBytes)
	family("storage_system_full_threshold_ratio", "gauge", "Use at which the pool-full alert is raised.")
	fmt.Fprintf(&b, "storage_system_full_threshold_ratio{%s} %g\n", sys, float64(v.FullPercent)/100)
	family("storage_system_drives", "gauge", "Drives in the pool, by state.")
	fmt.Fprintf(&b, "storage_system_drives{%s,state=\"ok\"} %d\nstorage_system_drives{%s,state=\"failed\"} %d\n", sys, v.Drives-v.DrivesFailed, sys, v.DrivesFailed)
	family("storage_system_node_up", "gauge", "Whether a storage node or enclosure serves.")
	for _, n := range v.Nodes {
		fmt.Fprintf(&b, "storage_system_node_up{%s,node=%q,role=\"node\"} %d\n", sys, chassisID(n.Path), b2i(n.Up))
	}
	for _, n := range v.Enclosures {
		fmt.Fprintf(&b, "storage_system_node_up{%s,node=%q,role=\"enclosure\"} %d\n", sys, chassisID(n.Path), b2i(n.Up))
	}
	objects := func(kind string, os []engine.StorageObjectView) {
		family("storage_system_"+kind+"_used_bytes", "gauge", "Bytes a "+kind+" holds.")
		for _, o := range os {
			fmt.Fprintf(&b, "storage_system_%s_used_bytes{%s,%s=%q} %d\n", kind, sys, kind, o.Name, o.UsedBytes)
		}
		family("storage_system_"+kind+"_capacity_bytes", "gauge", "A "+kind+"'s size or quota (0: unlimited).")
		for _, o := range os {
			fmt.Fprintf(&b, "storage_system_%s_capacity_bytes{%s,%s=%q} %d\n", kind, sys, kind, o.Name, o.CapacityBytes)
		}
		family("storage_system_"+kind+"_online", "gauge", "Whether a "+kind+" is served.")
		for _, o := range os {
			fmt.Fprintf(&b, "storage_system_%s_online{%s,%s=%q,node=%q} %d\n", kind, sys, kind, o.Name, nodeLabel(o.Node), b2i(o.Online))
		}
	}
	objects("filesystem", v.FileSystems)
	objects("volume", v.Volumes)
	objects("bucket", v.Buckets)
	family("storage_system_volume_hosts", "gauge", "Hosts a volume is mapped to.")
	for _, o := range v.Volumes {
		fmt.Fprintf(&b, "storage_system_volume_hosts{%s,volume=%q} %d\n", sys, o.Name, len(o.Hosts))
	}
	family("storage_system_snapshots", "gauge", "Snapshots the system holds.")
	fmt.Fprintf(&b, "storage_system_snapshots{%s} %d\n", sys, len(v.Snapshots))
	family("storage_system_alerts", "gauge", "Active alerts, by id and severity.")
	counts := map[string]int{}
	for _, a := range v.Alerts {
		counts[a.ID+"\x00"+a.Severity]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		id, sev, _ := strings.Cut(k, "\x00")
		fmt.Fprintf(&b, "storage_system_alerts{%s,alert=%q,severity=%q} %d\n", sys, id, sev, counts[k])
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// nodeLabel names a node by its rack and slot (two racks both have a node-01).
func nodeLabel(p string) string {
	if p == "" {
		return ""
	}
	return chassisID(p)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func healthCode(h string) int {
	switch h {
	case model.Warning:
		return 1
	case model.Critical:
		return 2
	}
	return 0
}
