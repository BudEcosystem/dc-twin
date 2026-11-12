package nos

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SONiC-VS (D44, D55). The twin renders, from the model, what a SONiC switch boots from: its HWSKU
// (the lane map SAI's virtual switch reads to bind each port to its container's eth<n>, and the
// port table), CONFIG_DB (config_db.json, which the image's own start.sh merges and loads) and
// /etc/sonic/constants.yml, which a switch has from its host image. Then it hands over to the
// image's own supervisord, which starts redis, syncd, orchagent and the *mgrd daemons as SONiC
// does, but not the routing daemons docker-sonic-vs also carries: on a switch they are the bgp
// container's. Beside it, in its network namespace and sharing its CONFIG_DB, run SONiC's own
// containers as a switch runs them: dhcp_relay, which renders its relay command lines from
// CONFIG_DB's VLAN table (`dhcrelay … -a %h:%p … -id Vlan1000 -iu …`); bgp (docker-fpm-frr) in
// SONiC's unified routing mode, where frrcfgd renders FRR's configuration from CONFIG_DB's BGP_*
// tables at start and applies every later change to them, VRFs and EVPN included; and gnmi,
// SONiC's gNMI server, at the switch's Loopback0 over TLS. The twin writes the fabric's routing
// into CONFIG_DB and nothing into FRR, the relay or the gNMI server.
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

// The gNMI server's port and where its TLS material is in the switch (the shared /etc/sonic).
const (
	SonicGNMIPort  = 8080
	sonicCredsDir  = "/etc/sonic/credentials"
	sonicGNMICert  = sonicCredsDir + "/gnmi.crt"
	sonicGNMIKey   = sonicCredsDir + "/gnmi.key"
	sonicGNMICA    = sonicCredsDir + "/ca.crt"
	sonicConstants = "/etc/sonic/constants.yml"
	// sonicFabricPolicy is the route-map every fabric session takes both ways
	sonicFabricPolicy = "DCTWIN_FABRIC"
)

//go:embed sonic-constants.yml
var sonicConstantsYML string

// sonicSidecars are SONiC's containers beside swss other than the relay: bgp and gnmi.
var sonicSidecars = []Sidecar{
	{Role: "bgp", Image: SonicBGPImage, CapAdd: []string{"NET_ADMIN", "NET_RAW", "SYS_ADMIN"},
		Check: []string{"vtysh", "-c", "show running-config"}, Pending: sonicBGPPending},
	{Role: "gnmi", Image: SonicGNMIImage,
		Check: []string{"sh", "-c", "supervisorctl status gnmi-native; ss -Hltn"}, Pending: sonicGNMIPending},
}

// sonicFactorySidecars: SONiC's lldp container (D78), which renders its supervisord from
// CONFIG_DB and runs lldpd, lldp_syncd and lldpmgrd; LLDP is how a day-0 tool validates cabling.
var sonicFactorySidecars = []Sidecar{
	{Role: "lldp", Image: SonicLLDPImage, CapAdd: []string{"NET_ADMIN", "NET_RAW"},
		Check: []string{"supervisorctl", "status", "lldpd"}, Pending: func(_ Switch, out string) (string, []string) {
			if strings.Contains(out, "RUNNING") {
				return "", nil
			}
			return "lldpd running", nil
		}},
}

// sonicBGPPending: FRR holds the switch's BGP instance. The bgp container loads the frr.conf it
// rendered from CONFIG_DB with `vtysh -b` as soon as bgpd runs, which can be before bgpd answers
// (under emulation it was, and the load left FRR without the instance); loading it again then is
// what an operator does, and frrcfgd applies only changes after start.
func sonicBGPPending(sw Switch, out string) (string, []string) {
	if sw.Factory != nil {
		// a factory switch has no BGP until day 0 configures one: FRR answering is enough
		if strings.Contains(out, "frr version") || strings.Contains(out, "Current configuration") {
			return "", nil
		}
		return "FRR answering vtysh", nil
	}
	if sw.ASN == 0 || strings.Contains(out, fmt.Sprintf("router bgp %d\n", sw.ASN)) {
		return "", nil
	}
	return fmt.Sprintf("FRR's router bgp %d", sw.ASN), []string{"vtysh", "-b"}
}

var gnmiRunning = regexp.MustCompile(`(?m)^gnmi-native\s+RUNNING`)

// sonicGNMIPending: the gNMI server runs and listens on its port.
func sonicGNMIPending(sw Switch, out string) (string, []string) {
	if sw.GNMI == nil {
		return "", nil // no TLS material: the server listens on loopback only, which nothing waits for
	}
	if !gnmiRunning.MatchString(out) || !strings.Contains(out, fmt.Sprintf(":%d ", SonicGNMIPort)) {
		return fmt.Sprintf("the gNMI server on port %d", SonicGNMIPort), nil
	}
	return "", nil
}

// ConfigDB renders a switch's config_db.json: DEVICE_METADATA (in SONiC's unified routing mode),
// PORT (the cabled ports), VLAN with its dhcp_servers, VLAN_MEMBER, VLAN_INTERFACE with the
// segment's gateway, LOOPBACK_INTERFACE, INTERFACE for routed and service ports, DEVICE_NEIGHBOR
// for the fabric links, the BGP instance with a neighbour per fabric link (BGP_GLOBALS,
// BGP_GLOBALS_AF, ROUTE_REDISTRIBUTE, BGP_NEIGHBOR, BGP_NEIGHBOR_AF, keyed by VRF as frrcfgd reads
// them), and GNMI.
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
	if sw.Factory != nil {
		// a factory switch (D72): what SONiC's factory default config holds, its hostname
		// sonic, its ports, unified routing; nothing of the design
		meta := map[string]any{"hostname": SonicFactoryHostname, "hwsku": SonicHWSKU(sw), "platform": sonicPlatform,
			"docker_routing_config_mode": "unified", "frr_mgmt_framework_config": "true", "type": "LeafRouter"}
		if sw.MAC != "" {
			meta["mac"] = strings.ToLower(sw.MAC)
		}
		put("DEVICE_METADATA", "localhost", meta)
		for _, p := range list {
			lanes := make([]string, len(p.Lanes))
			for i, l := range p.Lanes {
				lanes[i] = strconv.Itoa(l)
			}
			put("PORT", p.Name, map[string]any{"alias": p.Model, "lanes": strings.Join(lanes, ","), "index": strconv.Itoa(p.Index),
				"speed": strconv.Itoa(p.Speed), "admin_status": "up", "mtu": "9100"})
		}
		if sw.GNMI != nil {
			put("GNMI", "gnmi", map[string]any{"port": strconv.Itoa(SonicGNMIPort), "client_auth": "true", "user_auth": "none", "log_level": "2"})
			put("GNMI", "certs", map[string]any{"server_crt": sonicGNMICert, "server_key": sonicGNMIKey, "ca_crt": sonicGNMICA})
		}
		_ = ports
		return db, nil
	}
	// unified: FRR's configuration is CONFIG_DB's, rendered and kept by frrcfgd, which is what
	// takes a VRF, a VNI or an EVPN address family written there to FRR
	meta := map[string]any{"hostname": sw.Hostname, "hwsku": SonicHWSKU(sw), "platform": sonicPlatform,
		"docker_routing_config_mode": "unified", "frr_mgmt_framework_config": "true", "type": "LeafRouter"}
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
	if sw.Loopback.IsValid() {
		put("LOOPBACK_INTERFACE", "Loopback0", map[string]any{})
		put("LOOPBACK_INTERFACE", "Loopback0|"+sw.Loopback.String()+"/32", map[string]any{})
	}
	// the BGP instance: numbered eBGP to each fabric peer, connected routes (the segments, the
	// /31s, the service subnets, Loopback0) redistributed, a data centre's timers (3 s keepalives,
	// 10 s connect retry) so a replaced cable's session comes back in seconds, as on a switch
	if sw.ASN != 0 {
		put("BGP_GLOBALS", "default", map[string]any{"local_asn": strconv.FormatUint(uint64(sw.ASN), 10), "router_id": sw.RouterID.String(),
			"load_balance_mp_relax": "true", "keepalive": "3", "holdtime": "9"})
		put("BGP_GLOBALS_AF", "default|ipv4_unicast", map[string]any{"redistribute_connected": "true"})
		put("ROUTE_REDISTRIBUTE", "default|connected|bgp|ipv4", map[string]any{})
	}
	// FRR requires a policy on every eBGP session (ebgp-requires-policy, which frrcfgd has no
	// field for) and exchanges nothing without one: each fabric session takes a permit-all
	// route-map both ways, as a switch's configuration gives it one
	if len(sw.Routed) > 0 {
		put("ROUTE_MAP", sonicFabricPolicy+"|10", map[string]any{"route_operation": "permit"})
	}
	for _, r := range sw.Routed {
		name := ports[r.Port].Name
		put("INTERFACE", name, map[string]any{})
		put("INTERFACE", name+"|"+r.Addr.String(), map[string]any{})
		put("BGP_NEIGHBOR", "default|"+r.Peer.String(), map[string]any{"asn": strconv.FormatUint(uint64(r.PeerASN), 10), "name": r.PeerName,
			"local_addr": r.Addr.Addr().String(), "admin_status": "up", "keepalive": "3", "holdtime": "9", "conn_retry": "10"})
		// route_map_in and _out are leaf-lists, which config_db.json holds as JSON lists (a string
		// would be iterated by frrcfgd's template, character by character)
		put("BGP_NEIGHBOR_AF", "default|"+r.Peer.String()+"|ipv4_unicast", map[string]any{"admin_status": "true",
			"route_map_in": []string{sonicFabricPolicy}, "route_map_out": []string{sonicFabricPolicy}})
		if host, port, ok := strings.Cut(r.PeerName, ":"); ok {
			put("DEVICE_NEIGHBOR", name, map[string]any{"name": host, "port": port})
		}
	}
	for _, s := range sw.Service {
		name := ports[s.Port].Name
		put("INTERFACE", name, map[string]any{})
		put("INTERFACE", name+"|"+s.Gateway.String(), map[string]any{})
	}
	// mutual TLS: a client needs a certificate from the CA the server's chains to; user_auth none,
	// so a client's certificate is all it needs (no PAM users, no GNMI_CLIENT_CERT roles)
	if sw.GNMI != nil {
		put("GNMI", "gnmi", map[string]any{"port": strconv.Itoa(SonicGNMIPort), "client_auth": "true", "user_auth": "none", "log_level": "2"})
		put("GNMI", "certs", map[string]any{"server_crt": sonicGNMICert, "server_key": sonicGNMIKey, "ca_crt": sonicGNMICA})
	}
	return db, nil
}

// sonicReadyKeys are the STATE_DB keys intfmgrd sets to state ok once a routed interface's address
// is in place: the switch is ready when every one is, and SONiC's own relay container waits on
// the same keys (wait_for_intf.sh) before it starts dhcrelay.
func sonicReadyKeys(sw Switch, ports map[string]SonicPort) []string {
	var keys []string
	if sw.Factory != nil {
		return nil // nothing of the design is configured
	}
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

// sonicStartScript is the switch container's command: wait for the twin to move the ports in,
// render the HWSKU, config_db.json, constants.yml and the gNMI server's TLS files, take the routing
// daemons out of the image's start.sh (they are the bgp container's), then exec the image's
// supervisord (whose start.sh loads CONFIG_DB and starts SONiC). A background job reports
// `started` once SONiC has created its ports and every routed interface's address is in place;
// the substrate then starts the relay and the sidecars.
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
	w("cat > %s <<'EOF'", sonicConstants)
	b.WriteString(strings.TrimSuffix(sonicConstantsYML, "\n"))
	w("")
	w("EOF")
	if sw.GNMI != nil {
		w("mkdir -p %s && chmod 0755 %s", sonicCredsDir, sonicCredsDir)
		for _, f := range []struct{ path, pem string }{{sonicGNMICA, sw.GNMI.CA}, {sonicGNMICert, sw.GNMI.Cert}, {sonicGNMIKey, sw.GNMI.Key}} {
			w("cat > %s <<'EOF'", f.path)
			b.WriteString(strings.TrimSuffix(f.pem, "\n"))
			w("")
			w("EOF")
		}
		w("chmod 0600 %s", sonicGNMIKey)
	}
	if sw.Factory != nil {
		factoryPreamble(w, sw.Factory)
		w("# SONiC's factory login, admin/YourPaSsWoRd (sonic-buildimage rules/config); the VS image")
		w("# has no sudo, so admin is root-equivalent and sudo runs what it is given")
		w("getent passwd admin >/dev/null || useradd -o -u 0 -g 0 -M -d /root -s /bin/bash admin")
		w("echo 'admin:%s' | chpasswd", SonicFactoryPassword)
		w("%s", sonicFactoryShims)
	}
	w("# zebra, staticd, mgmtd and fpmsyncd are the bgp container's, as on a switch")
	w("sed -i -E '/supervisorctl start (zebra|staticd|mgmtd|fpmsyncd)$/d' /usr/bin/start.sh")
	w("(")
	w("  until [ \"$(redis-cli -n 0 EXISTS PORT_TABLE:PortInitDone 2>/dev/null)\" = 1 ]; do sleep 1; done")
	w("  log \"ports created\"")
	for _, k := range sonicReadyKeys(sw, ports) {
		w("  until [ \"$(redis-cli -n 6 HGET '%s' state 2>/dev/null)\" = ok ]; do sleep 1; done", k)
	}
	w("  log \"interfaces ready\"")
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

// sonicFactoryShims make a factory sonic-vs switch's CLI behave as a switch's for a day-0 tool:
// sudo (the VS image has none; admin is root-equivalent there), `config reload` without the
// systemd restarts a VS container cannot do (SonicReloadCommand), and sshd with password logins.
const sonicFactoryShims = `command -v sudo >/dev/null 2>&1 || { cat > /usr/local/bin/sudo <<'SUDO'
#!/bin/sh
exec "$@"
SUDO
chmod 755 /usr/local/bin/sudo; }
if [ ! -e /usr/local/bin/config.sonic ]; then
  mv /usr/local/bin/config /usr/local/bin/config.sonic
  cat > /usr/local/bin/config <<'CONFIG'
#!/bin/sh
# dc-twin: config reload without its systemd service restarts (a VS container runs no systemd)
if [ "$1" = reload ]; then shift; exec /usr/local/bin/config.sonic reload -n -f "$@"; fi
exec /usr/local/bin/config.sonic "$@"
CONFIG
  chmod 755 /usr/local/bin/config
fi
mkdir -p /run/sshd && ssh-keygen -A >/dev/null 2>&1
sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
# admin is uid 0 here (no sudo to elevate through), and sshd refuses a password login to any
# uid-0 account unless root logins with a password are allowed; on a switch admin is uid 1000
sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
grep -q '^PermitRootLogin' /etc/ssh/sshd_config || echo 'PermitRootLogin yes' >> /etc/ssh/sshd_config
/usr/sbin/sshd || log 'sshd did not start'`
