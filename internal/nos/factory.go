package nos

import (
	"fmt"
	"strings"
	"time"
)

// Factory is a switch that starts in its NOS's factory state (D72, 18): the twin applies none
// of the configuration in its Switch (its loopback, VLANs, fabric addresses, BGP, relay), which
// stays the design the day-0 oracle compares the software under test's result with. The switch
// has its management port, eth0, cabled to the OOB network (D73), and runs its NOS's ZTP (D74)
// and, before its NOS, ONIE's discovery (D75) when the fabric asks for them.
type Factory struct {
	// Install is preinstalled, or onie: the switch has no NOS until ONIE installs one.
	Install string
	// ZTP: the NOS's ZTP runs at its first boot.
	ZTP bool
	// Identity is what the switch reports as itself, in ONIE's EEPROM and ZTP's requests.
	Identity Identity
	// BootTime is how long the NOS takes from power-on to its management port's DHCP;
	// InstallTime how long ONIE's install takes. Both already scaled by spec.timeScale.
	BootTime, InstallTime time.Duration
	// Images are the installers ONIE's discovery recognises (D75).
	Images []Image
}

// Identity is a switch's identity as its ONIE EEPROM (onie-syseeprom) and its NOS carry it.
type Identity struct {
	Serial       string `json:"serial"`
	BaseMAC      string `json:"baseMAC"` // the switch's own MAC (its management port's, D73)
	MgmtMAC      string `json:"mgmtMAC"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"` // the product name
	PartNumber   string `json:"partNumber,omitempty"`
	// Platform is the ONIE platform string, <arch>-<vendor>_<machine>-r<revision>.
	Platform string `json:"platform,omitempty"`
	Arch     string `json:"arch,omitempty"` // x86_64, arm64
	// NOS and Version are what the NOS reports about itself once installed.
	NOS     string `json:"nos,omitempty"`
	Version string `json:"version,omitempty"`
}

// Image is one NOS installer ONIE's discovery recognises: by sha256 or by file name.
type Image struct {
	SHA256  string `json:"sha256,omitempty"`
	Name    string `json:"name,omitempty"`
	NOS     string `json:"nos"`
	Version string `json:"version,omitempty"`
}

// Day0Mount is where the twin's day-0 programs (cmd/dctwin-day0) are mounted in a factory
// switch, and Day0Dir where the twin writes what they read (the switch's identity, ONIE's
// images) and they write their status.
const (
	Day0Mount = "/opt/dctwin/bin"
	Day0Dir   = "/etc/dctwin"
	// Day0State is where the day-0 programs keep their state in the switch: ZTP's status and
	// log, ONIE's result.
	Day0State = "/var/lib/dctwin"
	// ExecSocket, in Day0Dir, is where the twin takes a factory SONiC switch's `docker exec` into
	// its service containers (clsys.Docker).
	ExecSocket = "exec.sock"
)

// MgmtIf is a switch's management port inside its container, on every NOS the twin runs
// (Cumulus's and SONiC's eth0).
const MgmtIf = "eth0"

// FactoryCapable reports why a NOS cannot start in factory state, or "" when it can: a NOS
// with no management API and no configuration store has nothing a day-0 tool could configure.
func (e Entry) FactoryCapable() string {
	switch e.Name {
	case NVUEFRR, SonicVS:
		return ""
	case HBN:
		return "an emulated BlueField's HBN is configured by NICo's agent, not by day 0"
	}
	return fmt.Sprintf("%s has no management API or configuration store a day-0 tool could configure", e.Name)
}

// ZTPFlavour is the ZTP contract a NOS's factory switch follows (D74): Cumulus's or SONiC's.
func (e Entry) ZTPFlavour() string {
	if e.Name == SonicVS {
		return "sonic"
	}
	return "cumulus"
}

// SonicReloadCommand is how SONiC's ZTP and a day-0 tool's `config reload` take a new
// config_db.json on sonic-vs: SONiC's own `config reload` without its service restarts (-n), which
// stop and start the switch's systemd units, which a VS container does not run; the managers and
// frrcfgd take the flushed and rewritten CONFIG_DB as it changes. -f skips its readiness checks,
// which read systemd too.
const SonicReloadCommand = "config reload -y -n -f"

// SonicFactoryMgmt starts a factory sonic-vs switch's management port's DHCP client: the twin's
// (dctwin-day0 dhcp), as docker-sonic-vs carries no dhclient (Q25) where a switch's
// interfaces-config runs one on eth0.
var SonicFactoryMgmt = []string{"sh", "-c", "mkdir -p /run/dctwin && exec " + Day0Mount + "/dctwin-day0 dhcp -i " + MgmtIf + " >>/var/log/dctwin-dhcp.log 2>&1"}

// factoryPreamble is what every factory switch's start script runs before its NOS: the day-0
// programs on the PATH as the switch has them, its management port present, and, when it has no
// NOS yet, ONIE's discovery until an installer the twin recognises has "installed" it (D75).
func factoryPreamble(w func(string, ...any), f *Factory, cumulus bool) {
	w("# a factory switch (D72): the twin's day-0 programs as the NOS has them")
	w("for p in ztp nv onie-discover; do [ -e /usr/sbin/$p ] || [ -e /usr/bin/$p ] || ln -sf %s/dctwin-day0 /usr/sbin/$p; done", Day0Mount)
	w("mkdir -p %s/ztp %s/onie /run/dctwin", Day0State, Day0State)
	if cumulus {
		// what Cumulus has beyond NVUE that a day-0 tool's script calls (internal/day0/clsys):
		// onie-install and reboot, which the twin carries out; `ip vrf exec` through the image's
		// socket-binding preload, first on a ZTP script's PATH (ScriptBin); dctwin-vrf-exec for
		// the relay's VRFs
		w("mkdir -p /usr/cumulus/bin %s", ScriptBin)
		w("ln -sf %s/dctwin-day0 /usr/cumulus/bin/onie-install", Day0Mount)
		w("ln -sf %s/dctwin-day0 /usr/sbin/reboot", Day0Mount)
		w("ln -sf %s/dctwin-day0 %s/ip", Day0Mount, ScriptBin)
		w("[ -e /usr/local/sbin/dctwin-vrf-exec ] || ln -sf %s/dctwin-day0 /usr/local/sbin/dctwin-vrf-exec", Day0Mount)
		w("ln -sf %[1]s/dctwin-day0 /usr/cumulus/bin/decode-syseeprom; ln -sf %[1]s/dctwin-day0 /usr/sbin/decode-syseeprom", Day0Mount)
		w("[ -e /usr/bin/timedatectl ] || ln -sf %s/dctwin-day0 /usr/bin/timedatectl", Day0Mount)
	} else {
		// SONiC's decode-syseeprom reads the unit's ONIE EEPROM; sonic-vs has none ("Platform
		// x86_64-kvm_x86_64-r0 does not support EEPROM"), so the twin's answers from the
		// switch's identity, as a SONiC switch's own EEPROM would
		w("ln -sf %s/dctwin-day0 /usr/local/bin/decode-syseeprom", Day0Mount)
		// SONiC's host CLI reaches its services with `docker exec <lldp|bgp|…>` (show lldp
		// neighbors, vtysh); the twin runs them in the switch's service containers
		w("command -v docker >/dev/null 2>&1 || ln -sf %s/dctwin-day0 /usr/local/bin/docker", Day0Mount)
		// timedatectl, which SONiC's image has from systemd and a VS container (no systemd) cannot
		// answer: the twin's, ahead of it on the PATH
		w("ln -sf %s/dctwin-day0 /usr/local/bin/timedatectl", Day0Mount)
	}
	w("n=0; while [ ! -e /sys/class/net/%s ]; do n=$((n+1)); [ $n -gt 3000 ] && { log \"%s never arrived\"; exit 1; }; sleep 0.1; done", MgmtIf, MgmtIf)
	w("ip link set %s up", MgmtIf)
	if f.Install == "onie" {
		w("log 'ONIE: discovering an installer'")
		w("until %s/dctwin-day0 onie-discover install; do log 'ONIE: discovery ended without an install; again'; sleep 5; done", Day0Mount)
		w("log 'ONIE: installed; booting the NOS'")
	}
	if cumulus {
		w("%s/dctwin-day0 os-release || log 'no /etc/os-release'", Day0Mount)
	}
}

// ScriptBin is where a factory Cumulus switch keeps the day-0 programs a ZTP script must find
// before the system's (its `ip`, whose `vrf exec` works in a container): first on ZTP's PATH.
const ScriptBin = "/usr/lib/dctwin/bin"

// SONiC's factory hostname and its admin user's factory password (sonic-buildimage rules/config,
// docs/reviews/2026-11-11-day0-vendor-facts.md E).
const (
	SonicFactoryHostname = "sonic"
	SonicFactoryPassword = "YourPaSsWoRd"
)

// FactoryVersion is what a preinstalled NOS reports as its version (ZTP's CUMULUS-VERSION and
// CUMULUS-BUILD, SONiC's SONiC-VERSION): the Cumulus release the emulated NVUE follows, the
// pinned SONiC build. An ONIE install reports its image's version instead.
func (e Entry) FactoryVersion() string {
	switch e.Name {
	case NVUEFRR:
		return "5.6.0"
	case SonicVS:
		return SonicRelayBuild
	}
	return ""
}

// SwitchVersion is what a preinstalled switch's NOS reports as its version: the Cumulus release
// its build names ("Cumulus Linux 5.18.3", spec.adapters.network.nos.cumulusVersion), else
// FactoryVersion.
func (e Entry) SwitchVersion(sw Switch) string {
	if v, ok := strings.CutPrefix(sw.Build, "Cumulus Linux "); ok && e.Name == NVUEFRR {
		return v
	}
	return e.FactoryVersion()
}
