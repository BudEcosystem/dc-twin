package nicoagent

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
)

// dpu is one BlueField's stand-in.
type dpu struct {
	*machine
	id identity

	// what its forge-dhcp-server answers its host with: the managed host's network
	// configuration as the agent last fetched it, and NICo's PXE service (dpuserve.go)
	hostMu  sync.Mutex
	hostCfg obj
	pxeURL  string
}

// runDPU is one boot of the DPU: the network, then its disk.
func (b *dpu) runDPU(ctx context.Context, why string) {
	if err := b.prepare(); err != nil {
		b.end("not booted: " + err.Error())
		return
	}
	out := b.netbootRetrying(ctx)
	if ctx.Err() != nil {
		return
	}
	switch {
	case out.kernel != "":
		b.end("booted " + baseName(out.kernel) + " (" + why + ")")
		b.retry(ctx, func() error { return b.runImage(ctx, out) })
		return
	case out.failed != "":
		b.note("network boot: " + out.failed)
	}
	// the firmware's next boot option: the DPU's disk
	in := b.loadInstalled()
	if in == nil {
		b.end("no OS: the network boot ended (" + firstOf(out.failed, "exit") + ") and nothing is installed")
		return
	}
	b.end("booted the installed DPU OS (" + why + ")")
	b.retry(ctx, func() error { return b.runAgent(ctx, in) })
}

// prepare reads the DPU's identity from its BMC and cables its out-of-band NIC to the BMC's
// switch port, once (the host's stand-in may ask first: it needs the DPU's host MAC).
func (b *dpu) prepare() error {
	b.prepMu.Lock()
	defer b.prepMu.Unlock()
	if b.nic != nil {
		return nil
	}
	id, err := b.readIdentity()
	if err != nil {
		return err
	}
	b.id = id
	b.sm = smbios{manufacturer: id.manufacturer, product: id.product, serial: id.serial, uuid: id.uuid}
	mac, err := net.ParseMAC(id.oobMAC)
	if err != nil {
		return fmt.Errorf("the BMC serves no usable oob0 MAC (%q)", id.oobMAC)
	}
	b.mac = mac
	p := bmcPort(b.dev)
	if p == nil {
		return fmt.Errorf("%s has no management port", b.path)
	}
	ref := p.Ref
	// a second DHCP client behind the BMC's port, by the OOB MAC (D52's relay)
	n, err := newNIC(mac, b.a.portConfig(ref, func() (netsim.PortRelay, error) { return b.a.rt.Net.PlugRelay(ref, mac) }))
	if err != nil {
		return err
	}
	b.nic = n
	return nil
}
