// Package nos is the registry of the switch operating systems a fabric's `nos` can name (D4,
// D21), and renders the start-up configuration the twin gives a real NOS it runs.
//
// A real NOS is what makes a fabric's routing and DHCP relay real: BGP sessions form between
// real routing daemons, and a BMC's DHCP reaches the stack's server through the relay the NOS
// runs, by giaddr. Which NOS an estate can run depends on the host. SONiC-VS (the spec's default,
// D4) is SONiC's own virtual switch image, published for x86_64 only: it runs natively on an
// x86_64 Docker host, and on an aarch64 one under the host's binary translation for amd64
// (binfmt_misc: Rosetta in an Apple-silicon Mac's Linux VM, or QEMU user mode), unmodified and
// slower, which the fidelity report says. `generic/frr-dhcrelay` runs natively anywhere: SONiC's
// own routing suite (FRR) and the relay SONiC's dhcp_relay container runs (ISC dhcrelay), from
// Debian, with nothing of SONiC around them. The registry says which is which, and the fidelity
// report prints it.
package nos

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Entry is one NOS a fabric can name.
type Entry struct {
	Name string
	// Real is false for `none`: model switches (D21).
	Real bool
	// Image is the container image a runtime starts for each switch.
	Image string
	// Obtain says how an operator gets the images onto the Docker host when one is missing.
	Obtain string
	// Arch lists the host architectures the image runs on natively (aarch64, x86_64).
	Arch []string
	// Platform is the image's OCI platform (linux/amd64); the runtime is asked for it, so an
	// image that runs emulated is started as what it is.
	Platform string
	// Emulated lists the host architectures the image runs on under the Docker host's binary
	// translation for its platform (binfmt_misc: Rosetta or QEMU user mode): unmodified, slower,
	// and labelled so in the fidelity report.
	Emulated []string
	// Privileged: the switch container runs privileged, as the image is run everywhere it is
	// run (SONiC-VS creates TAP devices for its host interfaces and writes its own sysctls).
	Privileged bool
	// Sysctls are the switch container's network sysctls.
	Sysctls map[string]string
	// Shared are directories the switch container shares with its relay container, one volume
	// each, as a SONiC switch's containers share the host's /etc/sonic and redis socket.
	Shared []Mount
	// RelayImage is the container the NOS runs its DHCP relay in, beside the switch container,
	// in its network namespace (SONiC's dhcp_relay container beside swss); empty when the relay
	// runs in the switch container.
	RelayImage string
	// StartTimeout bounds a switch's start-up: how long `up` waits for it to finish.
	StartTimeout time.Duration
	// SoftwareChecksum: a host cabled to one of its ports must checksum its own frames. SONiC-VS
	// copies frames between a port and its host interface with a packet socket, which delivers a
	// frame whose checksum the sender's kernel left to offload as it is, and the switch's kernel
	// then drops it; SONiC's own test harness turns transmit checksum offload off on every host
	// it cables to a SONiC-VS port, and the twin does the same on the veths it cables.
	SoftwareChecksum bool
	// Runnable is true when the twin renders this NOS's configuration and can start it.
	Runnable bool
	// Why is the reason a registered NOS is not runnable here.
	Why string
	// Relay is what the NOS's DHCP relay inserts and how it selects a segment.
	Relay Relay
	// Provides are the capabilities a fabric running it provides (06).
	Provides []string
	// IsNot is what the fidelity report prints for it.
	IsNot []string
}

// Mount is a directory a switch container shares with its relay container.
type Mount struct {
	Path string
	// RelayReadOnly mounts it read-only in the relay container.
	RelayReadOnly bool
}

// Relay describes a NOS's DHCP relay as a stack sees it.
type Relay struct {
	Implementation string
	// Circuit is option 82's circuit id: "interface" (the interface the request arrived on, ISC
	// dhcrelay unpatched), or "hostname:port" (SONiC's patched dhcrelay, `-a %h:%p`).
	Circuit string
	// LinkSelection: whether the relay sends RFC 3527 link selection (none of these do).
	LinkSelection bool
}

var registry = map[string]Entry{
	"none": {Name: "none", Relay: Relay{Implementation: "the twin relay (model switch)", Circuit: "hostname:port", LinkSelection: true}},
	SonicVS: {Name: SonicVS, Real: true, Runnable: true,
		Image: SonicVSImage, RelayImage: SonicRelayImage,
		Obtain: "docker pull --platform linux/amd64 " + SonicVSImage + "; the relay image is SONiC's docker-dhcp-relay.gz from build " + SonicRelayBuild + " (" + SonicRelayArtifact + ", sha256 " + SonicRelaySHA256 + "), docker load-ed and tagged " + SonicRelayImage + " (scripts/linux-substrate-e2e.sh does both)",
		Arch:   []string{"x86_64"}, Platform: "linux/amd64", Emulated: []string{"aarch64"},
		Privileged: true, Sysctls: map[string]string{"net.ipv4.ip_forward": "1"},
		Shared: []Mount{{Path: "/etc/sonic", RelayReadOnly: true}, {Path: "/var/run/redis"}},
		// emulated, a switch boots in under 2 minutes on a quiet host and took over 10 with the
		// Docker host's load average near 90: every `supervisorctl start` in SONiC's start.sh is an
		// emulated Python process
		StartTimeout: 30 * time.Minute, SoftwareChecksum: true,
		Relay:    Relay{Implementation: "SONiC's dhcp_relay container: ISC dhcrelay 4.4.3-P1 with SONiC's patches, its command line rendered by SONiC from CONFIG_DB (`-a %h:%p`)", Circuit: "hostname:port"},
		Provides: []string{"dhcp/relay-giaddr", "dhcp/relay-option-82", "switch/bgp-numbered"},
		IsNot: []string{
			"sonic-vs is SONiC's virtual switch (docker-sonic-vs " + SonicVSVersion + "): swss, syncd and FRR as SONiC runs them, over SAI's virtual switch (vslib), whose forwarding is the Linux kernel's; no buffers, queues, ACL hardware or ASIC counters behave as a switch's do.",
			"docker-sonic-vs carries no bgp container: the twin writes FRR's bgpd configuration from the same neighbours it puts in CONFIG_DB's BGP_NEIGHBOR, which is bgpcfgd's job on a switch; BGP is numbered eBGP over /31s, with no unnumbered sessions, no EVPN and no LLDP.",
			"Its DHCP relay is SONiC's dhcp_relay container from a later SONiC build (" + SonicRelayBuild + ") than the switch image; the two share only CONFIG_DB and the network namespace, as on a switch. Option 82's circuit id is SONiC's `hostname:alias`, the alias being the model's port name, and its remote id the switch's MAC.",
			"Only a switch's cabled ports exist in its config_db (SONiC-VS needs an interface behind every port it creates): an uncabled port is absent, not down. Ports are named EthernetN by their first lane, lanes by speed (1 up to 25G, 2 at 50G, 4 up to 200G, 8 above), with MTU 1500, the twin's cables'.",
			"Hosts cabled to a SONiC-VS port (attached hosts, a peer switch's fabric port) checksum in software: the twin turns transmit checksum offload off on their veths, as SONiC's own test harness does.",
		}},
	"cumulus-vx": {Name: "cumulus-vx", Real: true, Arch: []string{"x86_64"},
		Why:   "Cumulus VX is not freely redistributable; an operator's image would plug in as another entry here",
		Relay: Relay{Implementation: "ISC dhcrelay with Cumulus's patches", Circuit: "interface"}},
	FRRDHCRelay: {Name: FRRDHCRelay, Real: true, Image: "dctwin-nos-frr-dhcrelay:bookworm", Arch: []string{"aarch64", "x86_64"}, Runnable: true,
		Obtain:       "build it (test/linuxnet/nos/Containerfile) or set spec.adapters.network.nos.image",
		Sysctls:      map[string]string{"net.ipv4.ip_forward": "1", "net.ipv6.conf.all.disable_ipv6": "1", "net.ipv6.conf.default.disable_ipv6": "1"},
		StartTimeout: 90 * time.Second,
		Relay:        Relay{Implementation: "ISC dhcrelay 4.4.3-P1 (Debian bookworm, unpatched)", Circuit: "interface"},
		Provides:     []string{"dhcp/relay-giaddr", "switch/bgp-numbered"},
		IsNot: []string{
			"generic/frr-dhcrelay is not SONiC: it is FRR (SONiC's routing suite) and ISC dhcrelay (the relay SONiC's dhcp_relay container runs), from Debian, configured by the twin with ip(8) and an frr.conf; there is no config_db, no SAI, no SONiC CLI or management API.",
			"Its dhcrelay is unpatched: option 82's circuit id is the interface the request arrived on (the segment's VLAN), the same for every port of a segment, where SONiC's patched build sends hostname:port; no remote id and no link selection.",
			"BGP is numbered eBGP over /31s on each fabric link; no BGP unnumbered, no EVPN and no LLDP daemon.",
		}},
}

// FRRDHCRelay is the generic NOS the twin can run on any architecture.
const FRRDHCRelay = "generic/frr-dhcrelay"

// SonicVS is SONiC's virtual switch.
const SonicVS = "sonic-vs"

// The SONiC images, pinned. The switch is docker-sonic-vs as the SONiC project publishes it in its
// Azure container registry (master build 1112479, commit 83775acca, 2026-05-13), by digest. The
// project publishes no dhcp_relay image to a registry; its official VS build publishes one as a
// build artifact (docker-dhcp-relay.gz, a `docker save`), pinned here by build and by the
// tarball's sha256, loaded under a local tag. The two builds share CONFIG_DB's schema, which is
// all a relay reads.
const (
	SonicVSImage       = "sonicdev-microsoft.azurecr.io/docker-sonic-vs@sha256:0de1c335c5ce02b1f1102068eeb44c8272147535647acd6a3153e0cb857b44ed"
	SonicVSVersion     = "master.1112479-83775acca"
	SonicRelayImage    = "dctwin-sonic-dhcp-relay:master.1250767-408ffc98c"
	SonicRelayBuild    = "master.1250767-408ffc98c"
	SonicRelayArtifact = "https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-dhcp-relay.gz"
	SonicRelaySHA256   = "939cbdb454b76849284d3e8f51e26da41f63a87a3e763047bf61ecad091fc980"
)

// Lookup returns a registered NOS.
func Lookup(name string) (Entry, bool) {
	e, ok := registry[name]
	return e, ok
}

// Names lists the registry.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RunsOn reports whether the entry's image runs natively on a host architecture.
func (e Entry) RunsOn(arch string) bool {
	for _, a := range e.Arch {
		if a == arch {
			return true
		}
	}
	return false
}

// RunsEmulated reports whether the image runs on a host architecture only under emulation.
func (e Entry) RunsEmulated(arch string) bool {
	if e.RunsOn(arch) {
		return false
	}
	for _, a := range e.Emulated {
		if a == arch {
			return true
		}
	}
	return false
}

// Switch is one real switch's configuration, as the substrate derives it from the model. Every
// port in it is named as the model names it; a NOS maps those names to its own (Interface).
type Switch struct {
	Path     string // model device path
	Hostname string // "<rack>-<name>", the hostname the twin relay uses for the same switch
	Profile  string // the device profile, which names the SONiC HWSKU the twin renders for it
	// MAC is the switch's own MAC (its management port's): a NOS's system and router MAC.
	MAC      string
	ASN      uint32
	RouterID netip.Addr
	// Panel is every front-panel port of the switch in the profile's order (its Ethernet ports
	// that are not management ports), cabled or not: what a NOS that numbers its ports numbers
	// them from, so a port's number does not depend on what is cabled.
	Panel   []PanelPort
	Vlans   []Vlan
	Routed  []Routed
	Service []Service
}

// PanelPort is a front-panel port: the model's name and speed ("1G", "400G").
type PanelPort struct {
	Name  string
	Speed string
}

// Vlan is an access segment: the bridge a rack segment's ports are members of, with the
// segment's gateway on it and the DHCP servers it relays to.
type Vlan struct {
	Name    string // Vlan<id>, as SONiC names its VLAN interfaces
	Network string
	Rack    string
	Gateway netip.Prefix // the segment's .1 with its prefix length
	Ports   []string
	Relay   []netip.Addr
}

// Routed is a fabric port to another real switch: a /31, an eBGP session.
type Routed struct {
	Port     string
	Addr     netip.Prefix
	Peer     netip.Addr
	PeerASN  uint32
	PeerName string // "<peer hostname>:<peer port>", for the session's description
}

// Service is a port a stack's service host is attached to: the gateway of its subnet.
type Service struct {
	Port    string
	Gateway netip.Prefix
}

// Ports is every interface the twin moves into the switch, sorted.
func (s Switch) Ports() []string {
	seen := map[string]bool{}
	for _, v := range s.Vlans {
		for _, p := range v.Ports {
			seen[p] = true
		}
	}
	for _, r := range s.Routed {
		seen[r.Port] = true
	}
	for _, sv := range s.Service {
		seen[sv.Port] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Interface is the name a model port has inside the switch's container: the model's own name for
// generic/frr-dhcrelay, and eth<n> for SONiC-VS, whose front-panel ports are its container's
// eth1, eth2, … in panel order (eth0 is its management port). Empty when the NOS has no such
// port.
func (e Entry) Interface(sw Switch, port string) string {
	if e.Name == SonicVS {
		for i, p := range sw.Panel {
			if p.Name == port {
				return fmt.Sprintf("eth%d", i+1)
			}
		}
		return ""
	}
	return port
}

// Entrypoint is the switch container's entrypoint: a shell running the start-up script, which is
// its command.
var Entrypoint = []string{"/bin/sh", "-c"}

// StartScript renders the shell script a switch's container runs as its command: wait for the
// twin to move the ports in, configure the NOS from the model, start it and stay up. It says it
// is up with a line of its own that Started recognises.
func (e Entry) StartScript(sw Switch) (string, error) {
	for _, p := range sw.Ports() {
		ifname := e.Interface(sw, p)
		if ifname == "" {
			return "", fmt.Errorf("%s: port %s is not a front-panel port %s can carry", sw.Path, p, e.Name)
		}
		if len(ifname) > 15 || strings.ContainsAny(ifname, " /:") {
			return "", fmt.Errorf("%s: port %q is not a usable interface name", sw.Path, ifname)
		}
	}
	switch e.Name {
	case FRRDHCRelay:
		return e.frrStartScript(sw), nil
	case SonicVS:
		return sonicStartScript(sw)
	}
	return "", fmt.Errorf("nos %s: the twin renders no configuration for it", e.Name)
}

// startedLine is how a start-up script says its switch is up: `dctwin-nos <time>: started`, on a
// line of its own (SONiC's supervisord also logs "supervisord started").
var startedLine = regexp.MustCompile(`(?m)^dctwin-nos [0-9:]+: started\s*$`)

// Started reports whether a switch container's output says its start-up finished.
func Started(logs string) bool { return startedLine.MatchString(logs) }

func (e Entry) frrStartScript(sw Switch) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/sh")
	w("# dc-twin: %s for %s (%s), rendered from the estate", e.Name, sw.Hostname, sw.Path)
	w("log() { echo \"dctwin-nos $(date +%%H:%%M:%%S): $*\"; }")
	w("want=%q", strings.Join(sw.Ports(), " "))
	w("n=0")
	w("for p in $want; do")
	w("  while [ ! -e /sys/class/net/$p ]; do n=$((n+1)); [ $n -gt 3000 ] && { log \"port $p never arrived\"; exit 1; }; sleep 0.1; done")
	w("done")
	w("log \"ports present: $want\"")
	w("ip link set lo up")
	for _, v := range sw.Vlans {
		w("# %s: network %s, rack %s", v.Name, v.Network, v.Rack)
		w("ip link add %s type bridge", v.Name)
		for _, p := range v.Ports {
			w("ip link set %s master %s && ip link set %s up", p, v.Name, p)
		}
		w("ip addr add %s dev %s", v.Gateway, v.Name)
		w("ip link set %s up", v.Name)
	}
	for _, r := range sw.Routed {
		w("ip addr add %s dev %s && ip link set %s up", r.Addr, r.Port, r.Port)
	}
	for _, s := range sw.Service {
		w("ip addr add %s dev %s && ip link set %s up", s.Gateway, s.Port, s.Port)
	}
	w("mkdir -p /var/log/frr /var/run/frr && chown frr:frr /var/log/frr /var/run/frr")
	w("sed -i 's/^bgpd=no/bgpd=yes/' /etc/frr/daemons")
	w("echo 'service integrated-vtysh-config' > /etc/frr/vtysh.conf")
	w("cat > /etc/frr/frr.conf <<'FRR'")
	b.WriteString(e.frrConf(sw))
	w("FRR")
	w("chown frr:frr /etc/frr/frr.conf /etc/frr/vtysh.conf")
	w("/usr/lib/frr/frrinit.sh start")
	if args := e.relayArgs(sw); args != "" {
		w("log 'dhcrelay %s'", args)
		w("dhcrelay %s >/var/log/dhcrelay.log 2>&1 &", args)
	}
	w("log started")
	w("exec sleep infinity")
	return b.String()
}

func (e Entry) frrConf(sw Switch) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("frr defaults datacenter")
	w("hostname %s", sw.Hostname)
	w("log file /var/log/frr/frr.log informational")
	w("service integrated-vtysh-config")
	w("!")
	if len(sw.Routed) > 0 {
		w("router bgp %d", sw.ASN)
		w(" bgp router-id %s", sw.RouterID)
		w(" no bgp ebgp-requires-policy")
		w(" bgp bestpath as-path multipath-relax")
		for _, r := range sw.Routed {
			w(" neighbor %s remote-as %d", r.Peer, r.PeerASN)
			w(" neighbor %s description %s", r.Peer, r.PeerName)
		}
		w(" address-family ipv4 unicast")
		w("  redistribute connected")
		w(" exit-address-family")
		w("!")
	}
	return b.String()
}

// relayArgs is dhcrelay's command line: every access VLAN downstream, every routed port
// upstream, the servers last. `-a` appends option 82, `-m discard` drops a request that already
// carries one, as SONiC runs it.
func (e Entry) relayArgs(sw Switch) string {
	servers := map[netip.Addr]bool{}
	var args []string
	for _, v := range sw.Vlans {
		if len(v.Relay) == 0 {
			continue
		}
		args = append(args, "-id", v.Name)
		for _, s := range v.Relay {
			servers[s] = true
		}
	}
	if len(servers) == 0 {
		return ""
	}
	for _, r := range sw.Routed {
		args = append(args, "-iu", r.Port)
	}
	for _, s := range sw.Service {
		args = append(args, "-iu", s.Port)
	}
	var ss []string
	for s := range servers {
		ss = append(ss, s.String())
	}
	sort.Strings(ss)
	return strings.Join(append(append([]string{"-4", "-d", "-a", "-m", "discard"}, args...), ss...), " ")
}
