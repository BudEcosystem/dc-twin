package nos

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SONiC-VS (D44). The twin renders, from the model, what a SONiC switch boots from: its HWSKU (the
// lane map SAI's virtual switch reads to bind each port to its container's eth<n>, and the port
// table), CONFIG_DB (config_db.json, which the image's own start.sh merges and loads) and, in place
// of the bgp container's bgpcfgd that docker-sonic-vs does not carry, FRR's bgpd configuration
// for the neighbours CONFIG_DB lists. Then it hands over to the image's own supervisord, which
// starts redis, syncd, orchagent, the *mgrd daemons and FRR as SONiC does. SONiC's dhcp_relay
// container, started beside it in its network namespace once CONFIG_DB is loaded, renders its own
// relay command lines from CONFIG_DB's VLAN table (`dhcrelay … -a %h:%p … -id Vlan1000 -iu …`):
// the twin writes the VLAN's dhcp_servers and nothing of the relay.
//
// Port numbering follows SONiC's: a port is Ethernet<first lane>, its lanes counted along the front
// panel in the profile's order from the port's speed, so a 48×1G + 4×10G management switch is
// Ethernet0…Ethernet51 and a 128×400G spine Ethernet0, Ethernet8, …, Ethernet1016. A port's alias
// is the model's name for it (eth4, up1), which is what SONiC's relay puts after the hostname in
// option 82's circuit id, so a SONiC-relayed BMC's circuit id is the one the twin relay gives the
// same BMC (`<rack>-<switch>:<port>`). The container interface behind the port is eth<n>, n its
// position on the panel, which is how docker-sonic-vs is cabled everywhere.
//
// Only the cabled ports are configured: docker-sonic-vs's start.sh keeps only the ports whose
// eth<n> exists when it runs, and SAI's virtual switch fails to create a host interface for a
// port without one, so a 128-port spine with three cables is three ports. MTU is 1500 on every
// port and VLAN, as the twin's TAPs and veths are: SONiC's 9100 would let its own TCP segments
// (BGP) outgrow the cables.

// sonicPlatform is the platform docker-sonic-vs is built as.
const sonicPlatform = "x86_64-kvm_x86_64-r0"

// SonicPort is one front-panel port as SONiC-VS knows it.
type SonicPort struct {
	Model     string // the model's port name: SONiC's alias for it
	Interface string // the container's interface behind it, eth<index>
	Name      string // SONiC's port, Ethernet<first lane>
	Index     int    // its position on the front panel, from 1
	Lanes     []int
	Speed     int // Mb/s
}

// SonicPorts numbers a switch's whole front panel as SONiC does.
func SonicPorts(sw Switch) ([]SonicPort, error) {
	out := make([]SonicPort, 0, len(sw.Panel))
	lane := 0
	for i, p := range sw.Panel {
		mbps, err := speedMbps(p.Speed)
		if err != nil {
			return nil, fmt.Errorf("%s: port %s: %w", sw.Path, p.Name, err)
		}
		n := lanesFor(mbps)
		sp := SonicPort{Model: p.Name, Interface: fmt.Sprintf("eth%d", i+1), Name: fmt.Sprintf("Ethernet%d", lane), Index: i + 1, Speed: mbps}
		for l := 0; l < n; l++ {
			sp.Lanes = append(sp.Lanes, lane+l+1)
		}
		lane += n
		out = append(out, sp)
	}
	return out, nil
}

// sonicCabled is SonicPorts for the ports the twin cables, by model name.
func sonicCabled(sw Switch) (map[string]SonicPort, []SonicPort, error) {
	all, err := SonicPorts(sw)
	if err != nil {
		return nil, nil, err
	}
	byModel := map[string]SonicPort{}
	for _, p := range all {
		byModel[p.Model] = p
	}
	out := map[string]SonicPort{}
	var list []SonicPort
	for _, m := range sw.Ports() {
		p, ok := byModel[m]
		if !ok {
			return nil, nil, fmt.Errorf("%s: port %s is not on the switch's front panel", sw.Path, m)
		}
		out[m] = p
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	return out, list, nil
}

var speedRe = regexp.MustCompile(`^([0-9]+)\s*([MG])$`)

// speedMbps reads a profile's port speed ("1G", "400G", "100M"); a port without one is 1G.
func speedMbps(s string) (int, error) {
	if s == "" {
		return 1000, nil
	}
	m := speedRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("speed %q is not <n>M or <n>G", s)
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "G" {
		n *= 1000
	}
	return n, nil
}

// lanesFor is how many SerDes lanes a port of a speed takes, as SONiC's port tables count them.
func lanesFor(mbps int) int {
	switch {
	case mbps <= 25000:
		return 1
	case mbps <= 50000:
		return 2
	case mbps <= 200000:
		return 4
	}
	return 8
}

var unsafeSKU = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// SonicHWSKU is the HWSKU the twin renders for a switch: named after its profile.
func SonicHWSKU(sw Switch) string {
	if sw.Profile == "" {
		return "DCTwin"
	}
	return "DCTwin-" + strings.Trim(unsafeSKU.ReplaceAllString(sw.Profile, "-"), "-")
}

// SonicName is SONiC's name for a model port of a switch ("" when it is not on the panel).
func SonicName(sw Switch, port string) string {
	all, err := SonicPorts(sw)
	if err != nil {
		return ""
	}
	for _, p := range all {
		if p.Model == port {
			return p.Name
		}
	}
	return ""
}

// ConfigDB renders a switch's config_db.json: DEVICE_METADATA, PORT (the cabled ports), VLAN with
// its dhcp_servers, VLAN_MEMBER, VLAN_INTERFACE with the segment's gateway, INTERFACE for routed
// and service ports, BGP_NEIGHBOR and DEVICE_NEIGHBOR for the fabric links.
func ConfigDB(sw Switch) (map[string]map[string]any, error) {
	ports, list, err := sonicCabled(sw)
	if err != nil {
		return nil, err
	}
	db := map[string]map[string]any{}
	put := func(table, key string, v map[string]any) {
		if db[table] == nil {
			db[table] = map[string]any{}
		}
		db[table][key] = v
	}
	meta := map[string]any{"hostname": sw.Hostname, "hwsku": SonicHWSKU(sw), "platform": sonicPlatform,
		"docker_routing_config_mode": "separated", "type": "LeafRouter"}
	if len(sw.Vlans) > 0 {
		meta["type"] = "ToRRouter" // the switch a rack's hosts hang off
	}
	if sw.MAC != "" {
		meta["mac"] = strings.ToLower(sw.MAC)
	}
	if sw.ASN != 0 {
		meta["bgp_asn"] = strconv.FormatUint(uint64(sw.ASN), 10)
	}
	put("DEVICE_METADATA", "localhost", meta)
	for _, p := range list {
		lanes := make([]string, len(p.Lanes))
		for i, l := range p.Lanes {
			lanes[i] = strconv.Itoa(l)
		}
		put("PORT", p.Name, map[string]any{"alias": p.Model, "lanes": strings.Join(lanes, ","), "index": strconv.Itoa(p.Index),
			"speed": strconv.Itoa(p.Speed), "admin_status": "up", "mtu": "1500"})
	}
	for _, v := range sw.Vlans {
		id := strings.TrimPrefix(v.Name, "Vlan")
		if _, err := strconv.Atoi(id); err != nil || id == v.Name {
			return nil, fmt.Errorf("%s: VLAN %q is not named Vlan<id>", sw.Path, v.Name)
		}
		vlan := map[string]any{"vlanid": id, "mtu": "1500"}
		if len(v.Relay) > 0 {
			var servers []string
			for _, s := range v.Relay {
				servers = append(servers, s.String())
			}
			vlan["dhcp_servers"] = servers
		}
		put("VLAN", v.Name, vlan)
		for _, m := range v.Ports {
			put("VLAN_MEMBER", v.Name+"|"+ports[m].Name, map[string]any{"tagging_mode": "untagged"})
		}
		put("VLAN_INTERFACE", v.Name, map[string]any{})
		put("VLAN_INTERFACE", v.Name+"|"+v.Gateway.String(), map[string]any{})
	}
	for _, r := range sw.Routed {
		name := ports[r.Port].Name
		put("INTERFACE", name, map[string]any{})
		put("INTERFACE", name+"|"+r.Addr.String(), map[string]any{})
		put("BGP_NEIGHBOR", r.Peer.String(), map[string]any{"asn": strconv.FormatUint(uint64(r.PeerASN), 10), "name": r.PeerName,
			"local_addr": r.Addr.Addr().String(), "admin_status": "up", "keepalive": "3", "holdtime": "9"})
		if host, port, ok := strings.Cut(r.PeerName, ":"); ok {
			put("DEVICE_NEIGHBOR", name, map[string]any{"name": host, "port": port})
		}
	}
	for _, s := range sw.Service {
		name := ports[s.Port].Name
		put("INTERFACE", name, map[string]any{})
		put("INTERFACE", name+"|"+s.Gateway.String(), map[string]any{})
	}
	return db, nil
}

// sonicReadyKeys are the STATE_DB keys intfmgrd sets to state ok once a routed interface's address
// is in place: the switch is ready when every one is, and SONiC's own relay container waits on
// the same keys (wait_for_intf.sh) before it starts dhcrelay.
func sonicReadyKeys(sw Switch, ports map[string]SonicPort) []string {
	var keys []string
	for _, v := range sw.Vlans {
		keys = append(keys, "INTERFACE_TABLE|"+v.Name+"|"+v.Gateway.String())
	}
	for _, r := range sw.Routed {
		keys = append(keys, "INTERFACE_TABLE|"+ports[r.Port].Name+"|"+r.Addr.String())
	}
	for _, s := range sw.Service {
		keys = append(keys, "INTERFACE_TABLE|"+ports[s.Port].Name+"|"+s.Gateway.String())
	}
	return keys
}

// sonicBGPConf is the bgpd configuration bgpcfgd would write for CONFIG_DB's BGP_NEIGHBOR: numbered
// eBGP to each, connected routes redistributed (the segments, the /31s, the service subnets), with
// a data centre's timers (3 s keepalives, 10 s connect retry) so a replaced cable's session comes
// back in seconds, as it does on a switch.
func sonicBGPConf(sw Switch) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("! dc-twin: %s's BGP, rendered from the estate in place of SONiC's bgpcfgd (docker-sonic-vs has none), for CONFIG_DB's BGP_NEIGHBOR", sw.Hostname)
	w("log syslog informational")
	w("router bgp %d", sw.ASN)
	w(" bgp router-id %s", sw.RouterID)
	w(" bgp log-neighbor-changes")
	w(" no bgp ebgp-requires-policy")
	w(" bgp bestpath as-path multipath-relax")
	w(" timers bgp 3 9")
	for _, r := range sw.Routed {
		w(" neighbor %s remote-as %d", r.Peer, r.PeerASN)
		w(" neighbor %s description %s", r.Peer, r.PeerName)
		w(" neighbor %s timers connect 10", r.Peer)
	}
	w(" address-family ipv4 unicast")
	for _, r := range sw.Routed {
		w("  neighbor %s activate", r.Peer)
	}
	w("  redistribute connected")
	w(" exit-address-family")
	w("!")
	return b.String()
}

// sonicStartScript is the switch container's command: wait for the twin to move the ports in,
// render the HWSKU, config_db.json and bgpd.conf, then exec the image's supervisord (whose start.sh
// loads CONFIG_DB and starts SONiC). A background job reports `started` once SONiC has created its
// ports and every routed interface's address is in place, having started bgpd (start.sh does not:
// on a switch the bgp container does).
func sonicStartScript(sw Switch) (string, error) {
	ports, list, err := sonicCabled(sw)
	if err != nil {
		return "", err
	}
	db, err := ConfigDB(sw)
	if err != nil {
		return "", err
	}
	cfg, err := json.MarshalIndent(db, "", "    ")
	if err != nil {
		return "", err
	}
	sku := SonicHWSKU(sw)
	var want []string
	for _, p := range list {
		want = append(want, p.Interface)
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/sh")
	w("# dc-twin: %s for %s (%s), rendered from the estate", SonicVS, sw.Hostname, sw.Path)
	w("log() { echo \"dctwin-nos $(date +%%H:%%M:%%S): $*\"; }")
	w("want=%q", strings.Join(want, " "))
	w("n=0")
	w("for p in $want; do")
	w("  while [ ! -e /sys/class/net/$p ]; do n=$((n+1)); [ $n -gt 3000 ] && { log \"port $p never arrived\"; exit 1; }; sleep 0.1; done")
	w("done")
	w("log \"ports present: $want\"")
	w("ip link set lo up")
	w("sku=/usr/share/sonic/device/%s/%s", sonicPlatform, sku)
	w("mkdir -p $sku")
	w("cat > $sku/sai.profile <<'EOF'")
	w("SAI_VS_SWITCH_TYPE=SAI_VS_SWITCH_TYPE_BCM56850")
	w("SAI_VS_HOSTIF_USE_TAP_DEVICE=true")
	w("SAI_VS_INTERFACE_LANE_MAP_FILE=/usr/share/sonic/hwsku/lanemap.ini")
	w("SAI_VS_USE_CONFIGURED_SPEED_AS_OPER_SPEED=true")
	w("EOF")
	w("cat > $sku/lanemap.ini <<'EOF'")
	for _, p := range list {
		w("%s:%s", p.Interface, joinInts(p.Lanes))
	}
	w("EOF")
	// SONiC reads the columns by the header; index goes last because docker-sonic-vs's start.sh
	// filters this file by `grep -E "\s<lanes>\s"`, which a lane id could match in an index column
	// that a space follows
	w("cat > $sku/port_config.ini <<'EOF'")
	w("# name lanes alias speed index")
	for _, p := range list {
		w("%s %s %s %d %d", p.Name, joinInts(p.Lanes), p.Model, p.Speed, p.Index)
	}
	w("EOF")
	w("cat > /etc/sonic/config_db.json <<'EOF'")
	b.Write(cfg)
	w("")
	w("EOF")
	if len(sw.Routed) > 0 {
		w("cat > /etc/frr/bgpd.conf <<'EOF'")
		b.WriteString(sonicBGPConf(sw))
		w("EOF")
	}
	w("(")
	w("  until [ \"$(redis-cli -n 0 EXISTS PORT_TABLE:PortInitDone 2>/dev/null)\" = 1 ]; do sleep 1; done")
	w("  log \"ports created\"")
	for _, k := range sonicReadyKeys(sw, ports) {
		w("  until [ \"$(redis-cli -n 6 HGET '%s' state 2>/dev/null)\" = ok ]; do sleep 1; done", k)
	}
	w("  log \"interfaces ready\"")
	if len(sw.Routed) > 0 {
		w("  supervisorctl start bgpd >/dev/null")
	}
	w("  log started")
	w(") &")
	w("exec env HWSKU=%s /usr/local/bin/supervisord", sku)
	return b.String(), nil
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

// PortStateCommand is what the NOS observer runs in a switch's container to read its ports'
// operational state, or nil when it reads them from the switch's network namespace (FRR's ports
// are the namespace's links). SONiC's is APPL_DB's PORT_TABLE, which orchagent writes from the
// SAI's port state notifications and `show interfaces status` prints, read in one redis call.
func (e Entry) PortStateCommand() []string {
	if e.Name != SonicVS {
		return nil
	}
	return []string{"redis-cli", "-n", "0", "--raw", "EVAL",
		`local o={} for _,k in ipairs(redis.call('KEYS','PORT_TABLE:Ethernet*')) do o[#o+1]=k..' '..tostring(redis.call('HGET',k,'admin_status'))..' '..tostring(redis.call('HGET',k,'oper_status')) end return o`, "0"}
}

// ParsePortStates maps PortStateCommand's output to the model's port names: "up", "down" (no
// link) or "admin-down", as the observer reports every NOS's ports.
func (e Entry) ParsePortStates(sw Switch, out string) map[string]string {
	all, err := SonicPorts(sw)
	if err != nil {
		return nil
	}
	byName := map[string]string{}
	for _, p := range all {
		byName[p.Name] = p.Model
	}
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || !strings.HasPrefix(f[0], "PORT_TABLE:") {
			continue
		}
		model, ok := byName[strings.TrimPrefix(f[0], "PORT_TABLE:")]
		if !ok {
			continue
		}
		switch {
		case f[1] != "up":
			res[model] = "admin-down"
		case f[2] == "up":
			res[model] = "up"
		default:
			res[model] = "down"
		}
	}
	return res
}

// RelayStatusCommand is what the substrate runs in a relay container to see its relays running.
var RelayStatusCommand = []string{"supervisorctl", "status"}

// RelaysPending lists the relays a switch's VLANs need that RelayStatusCommand's output does not
// show running: SONiC's dhcp_relay runs one isc-dhcpv4-relay-<VLAN> per VLAN with dhcp_servers.
func RelaysPending(sw Switch, status string) []string {
	running := map[string]bool{}
	for _, line := range strings.Split(status, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "RUNNING" {
			running[f[0]] = true
		}
	}
	var out []string
	for _, v := range sw.Vlans {
		if len(v.Relay) == 0 {
			continue
		}
		if p := "isc-dhcpv4-relay-" + v.Name; !running[p] {
			out = append(out, p)
		}
	}
	return out
}

// Relaying reports whether any of a switch's VLANs relays DHCP.
func (sw Switch) Relaying() bool {
	for _, v := range sw.Vlans {
		if len(v.Relay) > 0 {
			return true
		}
	}
	return false
}
