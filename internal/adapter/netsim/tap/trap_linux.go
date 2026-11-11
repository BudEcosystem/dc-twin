//go:build linux

package tap

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// metaBridgeBroute is NFT_META_BRI_BROUTE (include/uapi/linux/netfilter/nf_tables.h), which the
// nftables library names no constant for.
const metaBridgeBroute expr.MetaKey = 35

// trapTable is the bridge table the twin installs in an emulated BlueField (D71).
const trapTable = "dctwin_trap"

// trapDHCP makes an emulated BlueField's HBN take the DHCP requests that arrive on its host
// representors up to itself, on the port they arrived on, rather than bridging them: HBN traps
// DHCP to the Arm, where NICo's forge-dhcp-server listens on each representor (a UDP socket bound
// to pf0hpf_if, which a bridge port's frames never reach). The kernel's bridge broute does it
// (nftables' meta broute, which the container's nft is too old to write), installed over netlink
// in the BlueField's namespace once, whatever its configuration: a port that is not in the bridge
// is not seen by the bridge's hooks.
func trapDHCP(ns netns.NsHandle, ports []string) error {
	c, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyBridge, Name: trapTable})
	prio := nftables.ChainPriority(-350)
	ch := c.AddChain(&nftables.Chain{Name: "trap", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPrerouting, Priority: &prio})
	be16 := func(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
	for _, p := range ports {
		name := make([]byte, unix.IFNAMSIZ)
		copy(name, p)
		for _, f := range []struct {
			ether         uint16
			protoOff, dst uint32
			dport         uint16
		}{{unix.ETH_P_IP, 9, 0, 67}, {unix.ETH_P_IPV6, 6, 0, 547}} {
			c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: name},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 12, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(f.ether)},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: f.protoOff, Len: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: be16(f.dport)},
				&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(1)},
				&expr.Meta{Key: metaBridgeBroute, SourceRegister: true, Register: 1},
			}})
		}
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("the DHCP trap on %s: %w", strings.Join(ports, ", "), err)
	}
	return nil
}
