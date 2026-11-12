//go:build linux

package tap

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// Day-0 faults on the TAP substrate (packs/generic/faults/day0.yaml, cabling.yaml): the engine
// records each as a set on the switch or on a port (Engine.FaultSets, PortFaultSets) and the
// substrate carries it out, every time the links are synced:
//
//   - in the switch's programs: a file in its /etc/dctwin/faults that the twin's ZTP client and
//     ONIE stand-in read (the ZTP options stripped from the lease, a corrupted script, every
//     installer refused, power lost during the install);
//   - in its network namespace: nftables drops for its ZTP fetches and its NTP, as a firewall would;
//   - on its management port: no carrier while it is dead on arrival; another host answering ARP
//     for its lease;
//   - on a fabric cable between real switches: carrier flapping, no link for a speed mismatch, the
//     far end renamed to another of the far switch's ports.

// faultFiles maps a switch set to the file in its /etc/dctwin/faults the twin's programs read.
var faultFiles = map[string]string{
	"day0.dhcpStripZTP":  "dhcp-strip-ztp",
	"day0.ztpCorrupt":    "ztp-corrupt",
	"day0.oniePowerLoss": "onie-power-loss",
}

// faultTable is the nftables table the substrate keeps a factory switch's drops in.
const faultTable = "dctwin_day0_fault"

type day0FaultState struct {
	files map[string]bool // file -> present
	drops string          // the drops installed: "ztp,ntp"
	dupIP string          // the address a duplicate holds on the management segment
}

// syncDay0Faults carries out the day-0 faults open on factory switches and their cables.
func (s *Substrate) syncDay0Faults() {
	if !s.plan.Factory() {
		return
	}
	if s.faultState == nil {
		s.faultState = map[string]*day0FaultState{}
	}
	for _, sw := range s.switches {
		sp := sw.plan
		if sp.Config.Factory == nil {
			continue
		}
		st := s.faultState[sp.Device.Path]
		if st == nil {
			st = &day0FaultState{files: map[string]bool{}}
			s.faultState[sp.Device.Path] = st
		}
		sets := s.eng.FaultSets(sp.Device)
		on := func(k string) bool { v, ok := sets[k]; return ok && v != false && v != "false" }
		// files the twin's programs read
		dir := filepath.Join(s.day0Dir(sp), "faults")
		for set, file := range faultFiles {
			want := on(set)
			if want == st.files[file] {
				continue
			}
			_ = os.MkdirAll(dir, 0o755)
			if want {
				_ = os.WriteFile(filepath.Join(dir, file), []byte("set by the twin's "+set+" fault\n"), 0o644)
			} else {
				_ = os.Remove(filepath.Join(dir, file))
			}
			st.files[file] = want
		}
		// ONIE's own reject switch (internal/day0/onie: <dir>/onie-reject-all)
		reject := filepath.Join(s.day0Dir(sp), "onie-reject-all")
		if want := on("day0.onieRejectAll"); want != st.files["onie-reject-all"] {
			if want {
				_ = os.WriteFile(reject, []byte("set by the twin's day0.onieRejectAll fault\n"), 0o644)
			} else {
				_ = os.Remove(reject)
			}
			st.files["onie-reject-all"] = want
		}
		// drops in the switch's namespace
		drops := ""
		if on("day0.blockZTP") {
			drops += "ztp,"
		}
		if on("day0.blockNTP") {
			drops += "ntp,"
		}
		if drops != st.drops {
			if err := setFaultDrops(sw, on("day0.blockZTP"), on("day0.blockNTP")); err != nil {
				s.log.Warn("tap: a day-0 fault's drops", "switch", sp.Device.Path, "err", err)
			} else {
				st.drops = drops
			}
		}
		// a duplicate of the switch's management lease on its segment
		s.syncDuplicateIP(sw, st, on("day0.duplicateMgmtIP"))
	}
	s.syncCableFaults()
}

// mgmtFaultUp reports whether a factory switch's management port may have carrier: not while the
// switch is dead on arrival.
func (s *Substrate) mgmtFaultUp(mp *MgmtPlan) bool {
	sets := s.eng.FaultSets(mp.Switch.Device)
	v, ok := sets["day0.doa"]
	return !ok || v == false || v == "false"
}

// setFaultDrops installs (or removes) the switch's drops: its ZTP and ONIE fetches (TCP to HTTP
// ports, TFTP) and its NTP, leaving every other connection alone.
func setFaultDrops(sw *nosSwitch, ztp, ntp bool) error {
	c, err := nftables.New(nftables.WithNetNSFd(int(sw.ns)))
	if err != nil {
		return err
	}
	t := &nftables.Table{Family: nftables.TableFamilyINet, Name: faultTable}
	c.DelTable(t)
	_ = c.Flush() // absent the first time
	if !ztp && !ntp {
		return nil
	}
	t = c.AddTable(t)
	prio := nftables.ChainPriority(0)
	ch := c.AddChain(&nftables.Chain{Name: "out", Table: t, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookOutput, Priority: &prio})
	be16 := func(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
	drop := func(proto byte, port uint16) {
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(port)},
			&expr.Verdict{Kind: expr.VerdictDrop},
		}})
	}
	if ztp {
		for _, p := range []uint16{80, 443, 8080} {
			drop(unix.IPPROTO_TCP, p)
		}
		drop(unix.IPPROTO_UDP, 69)
	}
	if ntp {
		drop(unix.IPPROTO_UDP, 123)
	}
	return c.Flush()
}

// syncDuplicateIP puts another host on a factory switch's management segment holding the switch's
// own lease (mgmt-duplicate-ip): a veth in the segment's bridge, in the twin's namespace, with the
// address, which answers ARP for it.
func (s *Substrate) syncDuplicateIP(sw *nosSwitch, st *day0FaultState, want bool) {
	var mg *mgmtPort
	for _, m := range s.mgmt {
		if m.plan.Switch == sw.plan {
			mg = m
		}
	}
	if mg == nil {
		return
	}
	name := mg.plan.TwinIf + "d"
	if !want {
		if st.dupIP != "" {
			_ = s.nl(func(h *netlink.Handle) error {
				if l, err := h.LinkByName(name); err == nil {
					return h.LinkDel(l)
				}
				return nil
			})
			st.dupIP = ""
		}
		return
	}
	var lease string
	if s.day0 != nil {
		s.day0.mu.Lock()
		lease = s.day0.obs[sw.plan.Device.Path].Mgmt
		s.day0.mu.Unlock()
	}
	if lease == "" || lease == st.dupIP {
		return
	}
	pfx, err := netip.ParsePrefix(lease)
	if err != nil {
		return
	}
	br := mg.plan.TwinIf
	if mg.plan.Bridge != nil {
		br = mg.plan.Bridge.Name
	}
	err = s.nl(func(h *netlink.Handle) error {
		if l, err := h.LinkByName(name); err == nil {
			_ = h.LinkDel(l)
		}
		if err := h.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name + "x", MTU: 1500}, PeerName: name}); err != nil {
			return err
		}
		port, err := h.LinkByName(name + "x")
		if err != nil {
			return err
		}
		b, err := h.LinkByName(br)
		if err != nil {
			return err
		}
		if err := h.LinkSetMasterByIndex(port, b.Attrs().Index); err != nil {
			return err
		}
		dup, err := h.LinkByName(name)
		if err != nil {
			return err
		}
		if err := h.AddrAdd(dup, &netlink.Addr{IPNet: &net.IPNet{IP: pfx.Addr().AsSlice(), Mask: net.CIDRMask(pfx.Bits(), 32)}}); err != nil {
			return err
		}
		for _, l := range []netlink.Link{port, dup} {
			if err := h.LinkSetUp(l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Warn("tap: mgmt-duplicate-ip", "switch", sw.plan.Device.Path, "err", err)
		return
	}
	st.dupIP = lease
}

// cableFaultUp is whether a fabric cable may have carrier under its ports' faults: not with a
// speed mismatch, and in the down half of a flap's period.
func (s *Substrate) cableFaultUp(c *cable) bool {
	for _, p := range []*model.Port{c.plan.Link.A, c.plan.Link.B} {
		sets := s.eng.PortFaultSets(p)
		if v, ok := sets["day0.speedMismatch"]; ok && v != false && v != "false" {
			return false
		}
		if v, ok := sets["day0.cableFlap"]; ok {
			period, _ := strconv.Atoi(fmt.Sprint(v))
			if period < 1 {
				period = 10
			}
			if (time.Now().Unix()/int64(period))%2 == 1 {
				return false
			}
		}
	}
	return true
}

// syncCableFaults renames a fabric cable's far end to another port of the far switch while a
// cable-wrong-port fault holds on its near end, and back when it clears.
func (s *Substrate) syncCableFaults() {
	if s.renamedEnds == nil {
		s.renamedEnds = map[string]string{}
	}
	for _, c := range s.cables {
		lp := c.plan
		for _, end := range []struct {
			near, far *model.Port
			farSw     *SwitchPlan
			farIf     string
		}{{lp.Link.A, lp.Link.B, lp.B, lp.BPort}, {lp.Link.B, lp.Link.A, lp.A, lp.APort}} {
			want := ""
			if v, ok := s.eng.PortFaultSets(end.near)["day0.wrongPort"]; ok {
				want = end.farSw.NOS.Interface(end.farSw.Config, fmt.Sprint(v))
			}
			key := lp.Link.ID + "|" + end.far.Ref
			have := s.renamedEnds[key]
			if want == have {
				continue
			}
			sw := s.byPath[end.farSw.Device.Path]
			if sw == nil || end.farSw.NOS.Name == nos.SonicVS {
				// SONiC's ports are its config_db's: a cable in a port it has none for has no link
				continue
			}
			from, to := end.farIf, want
			if have != "" {
				from = have
			}
			if to == "" {
				to = end.farIf
			}
			if err := renameIn(sw, from, to, to != end.farIf); err != nil {
				s.log.Warn("tap: cable-wrong-port", "cable", lp.Link.ID, "from", from, "to", to, "err", err)
				continue
			}
			if to == end.farIf && sw.nvue != nil {
				// back in its own port: the switch's own configuration of it again
				if _, err := s.execIn(sw, "ifreload", "-a"); err != nil {
					s.log.Warn("tap: cable-wrong-port: ifreload", "switch", sw.plan.Device.Path, "err", err)
				}
			}
			if to == end.farIf {
				delete(s.renamedEnds, key)
			} else {
				s.renamedEnds[key] = to
			}
			s.log.Warn("tap: cable as built (generic/cabling)", "cable", lp.Link.ID, "far end", end.far.Ref, "now", to)
		}
	}
}

// renameIn renames a switch's interface, down for the rename and up again, as a cable moved
// between ports is. bare: it arrives in a port the switch has configured nothing on, so the
// addresses and the bridge the old port had stay behind (the cable is in another port).
func renameIn(sw *nosSwitch, from, to string, bare bool) error {
	l, err := sw.h.LinkByName(from)
	if err != nil {
		return err
	}
	if _, err := sw.h.LinkByName(to); err == nil {
		return fmt.Errorf("%s already has a %s", sw.plan.Config.Hostname, to)
	}
	if err := sw.h.LinkSetDown(l); err != nil {
		return err
	}
	if err := sw.h.LinkSetName(l, to); err != nil {
		return err
	}
	if bare {
		if addrs, err := sw.h.AddrList(l, netlink.FAMILY_ALL); err == nil {
			for _, a := range addrs {
				if a.IP.IsLinkLocalUnicast() {
					continue
				}
				_ = sw.h.AddrDel(l, &a)
			}
		}
		_ = sw.h.LinkSetNoMaster(l)
	}
	return sw.h.LinkSetUp(l)
}
