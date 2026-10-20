package dhcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

// NVOS's DHCP client (D46). An NVLink switch tray runs two systems behind two management ports:
// its BMC, on standby power, behind bmc0, and NVOS, the switch OS, on the tray's CPU behind mgmt0
// (twin.NVOSPorts). A stack finds a switch's NVOS by MAC alone. NICo's switch state machine
// waits in WaitForOsMachineInterface until a machine interface carries one of the expected
// switch's nvos_mac_addresses (crates/switch-controller/src/initializing.rs); NICo creates a
// machine interface for whatever MAC its DHCP discovery sees (crates/api-core/src/dhcp/
// discover.rs), and resolves a switch's NVOS endpoint from that interface's address, refusing
// power control without it, "NVOS MAC or IP not available" (crates/api-core/src/handlers/
// component_manager.rs). So NVOS is a DHCP client as a BMC is, relayed by the same twin relay
// with its own port's circuit id, but only while NVOS runs: from its tray reaching On until it
// leaves On. NICo's own switch simulator does the same (crates/machine-a-tron: NVOS DHCPs once
// the switch is powered on, a power-off abandons its DHCP and stops its NVUE).
//
// What NVOS's client sends is not published. NICo keys on nothing but the MAC (its dhcp-server
// passes option 60 on and its API stores it, for any interface), and its switch simulator sends
// no option 60, saying no value has been verified for NVOS; the twin's NVOS sends none either.
// It sends option 12, NVOS's host name (nvos-<serial>, the NVUE adapter's system hostname), and
// the parameter request list of Debian's default dhclient.conf (NICo's NVUE mock reports NVOS's
// base OS as Debian 12), and no client identifier. The fidelity report says all of this is the
// twin's assumption.
//
// The model holds one lease per device, and on a switch tray it is the BMC's, so NVOS's lease
// is committed as a state event on its port's path: kind state, field lease, from the address
// NVOS held to the new one, whenever it changes (as SetLease commits a BMC's). The NVUE adapter
// follows these events to serve at NVOS's address while the tray is on.

// nvosHostname is the host name NVOS sends (option 12) and NVUE reports.
func nvosHostname(d *model.Device) string { return "nvos-" + strings.ToLower(d.Identity.Serial) }

// nvosRequestList is option 55 as Debian's default dhclient.conf requests it (subnet-mask,
// broadcast-address, time-offset, routers, domain-name, domain-name-servers, domain-search,
// host-name, netbios-name-servers, netbios-scope, interface-mtu, rfc3442-classless-static-routes,
// ntp-servers).
var nvosRequestList = []dhcpv4.OptionCode{dhcpv4.OptionSubnetMask, dhcpv4.OptionBroadcastAddress, dhcpv4.OptionTimeOffset,
	dhcpv4.OptionRouter, dhcpv4.OptionDomainName, dhcpv4.OptionDomainNameServer, dhcpv4.OptionDNSDomainSearchList,
	dhcpv4.OptionHostName, dhcpv4.OptionNetBIOSOverTCPIPNameServer, dhcpv4.OptionNetBIOSOverTCPIPScope,
	dhcpv4.OptionInterfaceMTU, dhcpv4.OptionClasslessStaticRoute, dhcpv4.OptionNTPServers}

// nvosEndpoint builds the DHCP client of a switch tray's NVOS management port, or records why it
// has none.
func (a *Adapter) nvosEndpoint(p *model.Port) *endpoint {
	skip := func(why string) *endpoint {
		a.nosSkipped[why]++
		return nil
	}
	ns := a.nets[p.Network]
	if ns == nil {
		return skip("the port is on no network with DHCP")
	}
	ns.nosPorts++
	if ns.idle != "" {
		return skip("network " + p.Network + " has no DHCP server")
	}
	ep := a.portEndpoint(p.Device, p, ns, skip)
	if ep == nil {
		return nil
	}
	if ep.seg.relay == nil {
		// a real NOS relays the segment, and the TAP substrate gives only BMC ports a NIC
		return skip("its segment is relayed by a real NOS (" + ep.nos + "), and only BMC ports have a NIC on the TAP substrate")
	}
	ep.nvos, ep.dev = true, p.Device
	ep.opts = []dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptHostName(nvosHostname(p.Device))),
		dhcpv4.WithOption(dhcpv4.OptParameterRequestList(nvosRequestList...)),
	}
	return ep
}

// nvosUp reports whether NVOS runs: its tray has input power and is On. NVOS does not run on
// the BMC's standby power, and while the tray powers on or off it is not up.
func (a *Adapter) nvosUp(d *model.Device) bool {
	on := false
	a.rt.Engine.Read(func(*model.Model) { on = d.State.AC && d.State.Power == model.On })
	return on
}

// watchNVOS follows the trays' power through the engine's committed events, as OnBMCUp follows
// a BMC's controller: a tray reaching On starts its NVOS clients (INIT-REBOOT for the address
// NVOS had), and leaving On halts them at once. A subscription dropped for falling behind the
// log is renewed and every tray looked at again.
func (a *Adapter) watchNVOS(ctx context.Context, id int, ch <-chan schema.Event) {
	for {
		select {
		case <-ctx.Done():
			a.rt.Engine.Unsubscribe(id)
			return
		case ev, ok := <-ch:
			if !ok {
				id, ch = a.rt.Engine.Subscribe(4096)
				for _, eps := range a.nosByDev {
					a.nvosPower(eps, false)
				}
				continue
			}
			if ev.Kind == "request" || ev.State == nil || (ev.State.Field != "power" && ev.State.Field != "ac") {
				continue
			}
			if eps := a.nosByDev[ev.Path]; eps != nil {
				a.nvosPower(eps, false)
			}
		}
	}
}

// nvosPower starts a tray's NVOS clients when NVOS is up and they are not running, and halts
// them when it is down. At start the clients spread over the adapter's start window, as BMCs do;
// after a power-on over a quarter of it.
func (a *Adapter) nvosPower(eps []*endpoint, initial bool) {
	if a.stopped.Load() || len(eps) == 0 {
		return
	}
	up := a.nvosUp(eps[0].dev)
	for _, ep := range eps {
		switch {
		case up && !ep.running():
			stagger := a.spread(ep.mac)
			if !initial {
				stagger /= 4
			}
			a.startEndpoint(ep, stagger)
		case !up:
			ep.halt()
		}
	}
}

// nvosBound commits NVOS's lease as a state event on its port's path when its address changes
// (the first lease, or a new address after a NAK sent it back to INIT); a renewal of the same
// address commits nothing, as SetLease does for a BMC.
func (a *Adapter) nvosBound(ep *endpoint, ip, server, gw, src string) {
	a.mu.Lock()
	old := a.nosLeases[ep.port.Ref]
	a.nosLeases[ep.port.Ref] = ip
	a.mu.Unlock()
	if old == ip {
		return
	}
	from := server
	if from == "" {
		from = "an unidentified server"
	}
	a.rt.Engine.Commit(schema.Event{Kind: "state", Source: sourceOf(src, server), Path: ep.port.Ref,
		State:   &schema.EventState{Field: "lease", From: old, To: ip},
		Message: fmt.Sprintf("NVOS %s leased %s on %s from %s", ep.port.Name, ip, ep.port.Network, from),
		Data: map[string]any{"os": "nvos", "server": server, "gateway": gw, "circuitId": ep.circuit,
			"remoteId": ep.remote.String(), "hostname": nvosHostname(ep.dev)}})
}

// NVOSLeases returns the address each NVOS management port holds, by port ref.
func (a *Adapter) NVOSLeases() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.nosLeases))
	for k, v := range a.nosLeases {
		out[k] = v
	}
	return out
}

// nvosReport is the NVOS part of the fidelity report; the caller holds a.mu.
func (a *Adapter) nvosReport() []string {
	if len(a.nosEPs) == 0 && len(a.nosSkipped) == 0 {
		return nil
	}
	var lines []string
	if n := len(a.nosEPs); n > 0 {
		leased, on := 0, 0
		for ref := range a.nosEPs {
			if a.nosLeases[ref] != "" {
				leased++
			}
		}
		for _, eps := range a.nosByDev {
			if a.nvosUp(eps[0].dev) {
				on += len(eps)
			}
		}
		lines = append(lines, fmt.Sprintf("NVOS: %d NVLink switch tray NVOS management ports (mgmt0) are DHCP clients while their tray is powered on (NVOS runs on the tray's CPU, not the BMC's standby power), relayed with their own port's circuit id; %d of them up now, %d leased; option 12 nvos-<serial>, no option 60 (NICo identifies NVOS by MAC, and its own switch simulator sends none, no NVOS value being verified); each lease is a state event on the port (field lease), which the nvue adapter serves NVUE at",
			n, on, leased))
	}
	var why []string
	for w, n := range a.nosSkipped {
		why = append(why, fmt.Sprintf("no DHCP client: %d NVOS management ports — %s", n, w))
	}
	sort.Strings(why)
	return append(lines, why...)
}
