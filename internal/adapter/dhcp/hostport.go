package dhcp

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// plugPort plugs a DHCP client the twin does not run into the twin relay of the segment a host's
// port is cabled to: a `vm` host's NIC, whose guest's own firmware and OS speak DHCP through the
// port's switch port in the vm adapter (netsim.HostPort, D48), or a second client behind a port
// another client holds, by its own MAC: a BlueField's Arm, whose out-of-band NIC shares the
// switch port its BMC's DHCP client is behind (D52). The relay treats each as it treats a BMC:
// the client's own socket is how it knows the ingress port, so the circuit id, remote id and
// link selection are the cabling's, never anything the guest claims; a reply comes back to the
// client by the circuit id and, within the port, chaddr, so two clients behind one port each get
// their own (relay.go). A second client gets no address from the port's cabling (that is the
// port's own client's): the twin site DHCP leases it from its dynamic range. Registered with the
// substrate as its relay directory (netsim.Net.SetRelayDirectory).
func (a *Adapter) plugPort(ref string, client net.HardwareAddr) (netsim.PortRelay, error) {
	if a.stopped.Load() {
		return nil, errors.New("the dhcp adapter has stopped")
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return nil, fmt.Errorf("%s is not a port ref", ref)
	}
	var d *model.Device
	var p *model.Port
	a.rt.Engine.Read(func(m *model.Model) {
		if d = m.DeviceBy[ref[:i]]; d != nil {
			p = d.PortBy[ref[i+1:]]
		}
	})
	if p == nil {
		return nil, fmt.Errorf("no port %s in the model", ref)
	}
	ns := a.nets[p.Network]
	if ns == nil {
		return nil, fmt.Errorf("network %q has no DHCP", p.Network)
	}
	if ns.idle != "" {
		return nil, errors.New(ns.idle)
	}
	var why string
	ep := a.portEndpoint(d, p, ns, func(w string) *endpoint { why = w; return nil })
	if ep == nil {
		return nil, errors.New(why)
	}
	if ep.seg.relay == nil {
		return nil, fmt.Errorf("a real NOS (%s) relays the segment of %s", ep.nos, ref)
	}
	if len(client) == 6 && client.String() != ep.mac.String() {
		ep.mac, ep.offset, ep.second = append(net.HardwareAddr(nil), client...), 0, true
	}
	ep.dev = d
	c, err := a.rt.Net.ListenUDP(d.Path, "dhcp-port:"+p.Name)
	if err != nil {
		return nil, err
	}
	ep.conn = c
	ep.addr = c.LocalAddr().(*net.UDPAddr).AddrPort()
	ep.seg.relay.attach(ep)
	a.mu.Lock()
	a.plugged[ep.plugKey()] = ep
	a.mu.Unlock()
	return &portRelay{a: a, ep: ep}, nil
}

// portRelay is a plugged port: its socket is the port, and the relay's socket the segment.
type portRelay struct {
	a    *Adapter
	ep   *endpoint
	once sync.Once
}

func (r *portRelay) Send(payload []byte) error {
	_, err := r.ep.conn.WriteToUDPAddrPort(payload, r.ep.seg.relay.addr())
	return err
}

func (r *portRelay) Receive(b []byte) (int, error) {
	for {
		n, from, err := r.ep.conn.ReadFromUDPAddrPort(b)
		if err != nil {
			return 0, netsim.ErrClosed
		}
		if from == r.ep.seg.relay.addr() {
			return n, nil
		}
	}
}

func (r *portRelay) Gateway() (netip.Prefix, net.HardwareAddr) {
	sg := r.ep.seg
	var mac net.HardwareAddr
	if len(sg.switches) > 0 {
		mac = switchMAC(sg.switches[0])
	}
	if !sg.Gateway.IsValid() {
		return netip.Prefix{}, mac
	}
	return netip.PrefixFrom(sg.Gateway, sg.Prefix.Bits()), mac
}

func (r *portRelay) Server() string { return r.ep.seg.relay.up.label }

func (r *portRelay) Close() error {
	r.once.Do(func() {
		r.ep.seg.relay.detach(r.ep)
		r.ep.conn.Close()
		r.a.mu.Lock()
		if r.a.plugged[r.ep.plugKey()] == r.ep {
			delete(r.a.plugged, r.ep.plugKey())
		}
		r.a.mu.Unlock()
	})
	return nil
}

// plugKey is a plugged client's key: its port's ref, and its MAC when it is a second client
// behind the port.
func (ep *endpoint) plugKey() string {
	if ep.second {
		return ep.port.Ref + " " + ep.mac.String()
	}
	return ep.port.Ref
}
