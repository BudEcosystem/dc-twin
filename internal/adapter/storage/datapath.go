package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The data path of a storage system at fidelity vm (docs/reviews/2026-11-08-storage-deep-dive.md
// S-Q2): each storage node is an `os: twin` vm host whose image carries Linux's NFS server, nvmet
// and an S3 server (internal/adapter/vm/storagenode.go). This reconciler makes each node serve the
// objects the model homes on it, through the in-host agent (D50): the node's storage address on
// its data port, the pool on its first data drive, a file system per file system with its export
// rules, an NVMe/TCP subsystem per volume admitting its mapped hosts, a bucket per bucket. What
// the node holds is observed back into the model (D19). Each node's targets are published: at
// the node's storage address for the estate's vm hosts (a site route), and on this machine's
// loopback for clients outside the twin, through the segment gateway the twin is on the node's
// data port.

const (
	exportsRoot    = "/exports *(ro,fsid=0,no_subtree_check,crossmnt,insecure)"
	reconcileEvery = 3 * time.Second
)

var observer = schema.EventSource{Class: "observer", Principal: "storage"}

type execer interface {
	Exec(ctx context.Context, path, script string) (string, int, error)
}

type guestDialer interface {
	DialGuest(ctx context.Context, path, portRef string, gw netip.Prefix, dst netip.AddrPort) (net.Conn, error)
}

type datapath struct {
	s     *server
	sys   *model.StorageSystem
	nodes []*dpNode
}

type dpNode struct {
	dev     *model.Device
	portRef string
	mac     string
	serial  string
	addr    netip.Prefix // the node's storage address on its data segment
	gw      netip.Prefix // the segment gateway

	mu      sync.Mutex
	ups     map[string]bool
	err     string
	setups  int
	applied int
	pub     map[string]string // protocol -> this machine's loopback address
	conns   map[string]int
}

func newDatapath(s *server, sys *model.StorageSystem) *datapath {
	d := &datapath{s: s, sys: sys}
	var m *model.Model
	s.a.rt.Engine.Read(func(mm *model.Model) { m = mm })
	segs := dhcp.NetworkSegments(m, sys.DataNetwork)
	segOf := func(dev *model.Device) (dhcp.Segment, bool) {
		for _, sg := range segs {
			if rack := dev.RackOf(); rack != nil && sg.Rack == rack.Path && sg.Prefix.IsValid() {
				return sg, true
			}
		}
		return dhcp.Segment{}, false
	}
	// a node's address is .20 onwards of its segment, numbered over every vm system's nodes on
	// the network in path order: two systems sharing a rack, or racks sharing one subnet, never
	// hand out the same address (compile keeps DHCP off the network)
	var all []*model.Device
	for _, o := range m.StorageSystems {
		if o.Fidelity == "vm" && o.DataNetwork == sys.DataNetwork {
			all = append(all, o.Nodes...)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	seq, next := map[*model.Device]int{}, map[netip.Prefix]int{}
	for _, dev := range all {
		if sg, ok := segOf(dev); ok {
			seq[dev] = next[sg.Prefix]
			next[sg.Prefix]++
		}
	}
	for _, dev := range sys.Nodes {
		n := &dpNode{dev: dev, pub: map[string]string{}, conns: map[string]int{}}
		for _, p := range dev.Ports {
			if p.Network == sys.DataNetwork && p.MAC != "" {
				n.portRef, n.mac = p.Ref, strings.ToLower(p.MAC)
				break
			}
		}
		for _, c := range sys.Drives {
			if c.Device == dev {
				n.serial = c.State.Serial
				if n.serial == "" && c.Drive != nil {
					n.serial = c.Drive.Serial
				}
				break
			}
		}
		if sg, ok := segOf(dev); ok {
			a := sg.Prefix.Addr()
			for i := 0; i < 20+seq[dev]; i++ {
				a = a.Next()
			}
			if sg.Prefix.Contains(a) {
				n.addr = netip.PrefixFrom(a, sg.Prefix.Bits())
				n.gw = netip.PrefixFrom(sg.Gateway, sg.Prefix.Bits())
			}
		}
		if n.addr.IsValid() {
			s.a.rt.Engine.SetStorageAddress(sys.Path, "node:"+dev.Path, n.addr.Addr().String())
		}
		d.nodes = append(d.nodes, n)
	}
	return d
}

func (d *datapath) run(ctx context.Context) {
	for _, n := range d.nodes {
		if n.addr.IsValid() && n.portRef != "" {
			for proto, port := range map[string]int{"nfs": NFSPort, "nvme-tcp": NVMePort, "s3": S3Port} {
				d.publish(ctx, n, proto, port)
			}
		}
	}
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		for _, n := range d.nodes {
			if ctx.Err() != nil {
				return
			}
			d.reconcile(ctx, n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// publish serves one of a node's targets on this machine and at the node's storage address (a
// site route), each connection dialled into the guest from its segment gateway.
func (d *datapath) publish(ctx context.Context, n *dpNode, proto string, port int) {
	ln, err := d.s.listen(n.dev.Path + "#" + proto)
	if err != nil {
		n.setErr("publish " + proto + ": " + err.Error())
		return
	}
	local := ln.Addr().String()
	n.mu.Lock()
	n.pub[proto] = local
	n.mu.Unlock()
	d.s.a.rt.Net.Route(netip.AddrPortFrom(n.addr.Addr(), uint16(port)).String(), local)
	d.s.a.rt.Engine.SetStorageEndpoint(d.sys.Path, proto+"@"+chassisID(n.dev.Path), local)
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.splice(ctx, n, proto, port, c)
		}
	}()
}

func (d *datapath) splice(ctx context.Context, n *dpNode, proto string, port int, c net.Conn) {
	defer c.Close()
	if d.s.a.rt.Peer == nil {
		return
	}
	vm, ok := d.s.a.rt.Peer("vm").(guestDialer)
	if !ok {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	g, err := vm.DialGuest(dctx, n.dev.Path, n.portRef, n.gw, netip.AddrPortFrom(n.addr.Addr(), uint16(port)))
	cancel()
	if err != nil {
		n.setErr(proto + ": " + err.Error())
		return
	}
	defer g.Close()
	n.mu.Lock()
	n.conns[proto]++
	n.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(g, c); closeWrite(g) }()
	go func() { defer wg.Done(); io.Copy(c, g); closeWrite(c) }()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
			g.Close()
		case <-done:
		}
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func (n *dpNode) setErr(e string) {
	n.mu.Lock()
	n.err = e
	n.mu.Unlock()
}

// report is what a node's `dctwin-storage report` says it holds.
type report struct {
	ups                 map[string]bool
	fs, vol, bucket     map[string]int64
	exported            map[string]bool     // file system -> bind-mounted under /exports
	hosts               map[string][]string // nqn -> allowed hosts (a subsystem nvmet serves)
	exports             string
	poolUsed, poolTotal int64
}

func parseReport(out string) report {
	r := report{ups: map[string]bool{}, fs: map[string]int64{}, vol: map[string]int64{}, bucket: map[string]int64{}, exported: map[string]bool{}, hosts: map[string][]string{}}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		n := func(i int) int64 {
			if i >= len(f) {
				return 0
			}
			v, _ := strconv.ParseInt(f[i], 10, 64)
			return v
		}
		switch f[0] {
		case "up":
			r.ups[f[1]] = true
		case "fs":
			r.fs[f[1]], r.exported[f[1]] = n(2), n(3) == 1
		case "vol":
			r.vol[f[1]] = n(2)
		case "bucket":
			r.bucket[f[1]] = n(2)
		case "nqn":
			r.hosts[f[1]] = append([]string{}, f[2:]...)
		case "exports":
			r.exports = f[1]
		case "pool":
			r.poolUsed, r.poolTotal = n(1), n(2)
		}
	}
	return r
}

func (d *datapath) exec(ctx context.Context, n *dpNode, script string) (string, error) {
	var in execer
	ok := false
	if d.s.a.rt.Peer != nil {
		in, ok = d.s.a.rt.Peer("inhost").(execer)
	}
	if !ok {
		return "", fmt.Errorf("the inhost adapter is not running: a vm storage node is driven through its in-host agent (--adapters inhost)")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, rc, err := in.Exec(ctx, n.dev.Path, script)
	if err != nil {
		return out, err
	}
	if rc != 0 {
		return out, fmt.Errorf("%q: exit %d: %s", firstLine(script), rc, strings.TrimSpace(out))
	}
	return out, nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func (d *datapath) reconcile(ctx context.Context, n *dpNode) {
	if n.portRef == "" || !n.addr.IsValid() || n.serial == "" {
		n.setErr("no storage address, data port or pool drive: not served")
		return
	}
	out, err := d.exec(ctx, n, "dctwin-storage report")
	if err != nil {
		n.setErr(err.Error()) // the guest is off, booting, or its agent not attached
		return
	}
	r := parseReport(out)
	n.mu.Lock()
	n.ups = r.ups
	n.mu.Unlock()
	if !r.ups["nfs"] || !r.ups["s3"] || !r.ups["nvme-tcp"] {
		c := d.s
		if _, err := d.exec(ctx, n, fmt.Sprintf("dctwin-storage setup %s %s %s %s %s", n.mac, n.addr, n.serial, c.access, c.secret)); err != nil {
			n.setErr("setup: " + err.Error())
			return
		}
		n.mu.Lock()
		n.setups++
		n.mu.Unlock()
		return // the next pass sees it up
	}
	v, err := d.s.a.rt.Engine.StorageSystem(d.sys.Path)
	if err != nil {
		return
	}
	var cmds []string
	var exportLines []string
	fsID, fsPath := map[string]string{}, map[string]string{}
	for _, o := range v.FileSystems {
		if o.Home != n.dev.Path {
			continue
		}
		fsID[o.Name] = o.ID
		path := o.Path
		if path == "" {
			path = "/" + o.Name
		}
		fsPath[o.Name] = path
		if !r.exported[o.Name] { // absent, or its data in the pool but not bind-mounted (a reboot)
			cmds = append(cmds, "dctwin-storage fs "+o.Name+" "+path)
		}
		if !o.Online || len(o.Exports) == 0 {
			continue
		}
		id, _ := strconv.Atoi(o.ID)
		var rules []string
		for _, x := range o.Exports {
			opt := fmt.Sprintf("%s,sync,no_subtree_check,insecure,fsid=%d", x.Access, 100+id)
			if x.RootSquash {
				opt += ",root_squash"
			} else {
				opt += ",no_root_squash"
			}
			rules = append(rules, x.Clients+"("+opt+")")
		}
		exportLines = append(exportLines, "/exports"+path+" "+strings.Join(rules, " "))
	}
	for name := range r.fs {
		if _, ok := fsID[name]; !ok {
			cmds = append(cmds, "dctwin-storage rmfs "+name)
		}
	}
	sort.Strings(exportLines)
	content := exportsRoot + "\n"
	for _, l := range exportLines {
		content += l + "\n"
	}
	sum := md5.Sum([]byte(content))
	// a file system bind-mounted again is exported again: the kernel's export cache still holds
	// the directory under the old mount
	writeExports := hex.EncodeToString(sum[:])[:12] != r.exports || slices.ContainsFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "dctwin-storage fs ") })
	volID := map[string]string{}
	for _, o := range v.Volumes {
		if o.Home != n.dev.Path {
			continue
		}
		volID[o.Name] = o.ID
		if _, served := r.hosts[o.NQN]; !served { // absent, or its image in the pool but no subsystem (a reboot)
			cmds = append(cmds, fmt.Sprintf("dctwin-storage vol %s %s %d %s", o.Name, o.NQN, o.CapacityBytes, nguid(o.NGUID)))
		}
		want := []string{}
		if o.Online {
			want = append(want, o.Hosts...)
		}
		if !sameSet(want, r.hosts[o.NQN]) {
			cmds = append(cmds, strings.TrimSpace("dctwin-storage hosts "+o.NQN+" "+strings.Join(want, " ")))
		}
	}
	for name := range r.vol {
		if _, ok := volID[name]; !ok {
			cmds = append(cmds, fmt.Sprintf("dctwin-storage rmvol %s nqn.2026-11.io.dctwin:%s:%s", name, d.sys.Name, name))
		}
	}
	buckets := map[string]bool{}
	for _, o := range v.Buckets {
		if o.Home != n.dev.Path {
			continue
		}
		buckets[o.Name] = true
		if _, ok := r.bucket[o.Name]; !ok {
			cmds = append(cmds, "dctwin-storage bucket "+o.Name)
		}
	}
	for name := range r.bucket {
		if !buckets[name] {
			cmds = append(cmds, "dctwin-storage rmbucket "+name)
		}
	}
	// file systems before the exports that name them
	if writeExports {
		body := ""
		for _, l := range exportLines {
			body += l + "\n"
		}
		cmds = append(cmds, "dctwin-storage exports <<'DCTWIN-EXPORTS'\n"+body+"DCTWIN-EXPORTS")
	}
	if err := d.apply(ctx, n, cmds); err != nil {
		n.setErr(err.Error())
		return
	}
	n.setErr("")
	// what the node holds, observed back
	eng := d.s.a.rt.Engine
	for name, b := range r.fs {
		if id := fsID[name]; id != "" {
			_ = eng.ObserveStorageUse(d.sys.Path, "filesystem", id, b, observer)
		}
	}
	for name, b := range r.vol {
		if id := volID[name]; id != "" {
			_ = eng.ObserveStorageUse(d.sys.Path, "volume", id, b, observer)
		}
	}
	for name, b := range r.bucket {
		if buckets[name] {
			_ = eng.ObserveStorageUse(d.sys.Path, "bucket", name, b, observer)
		}
	}
}

// apply runs commands, as few execs as the agent's 2 KiB lines allow.
func (d *datapath) apply(ctx context.Context, n *dpNode, cmds []string) error {
	var batch []string
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		script := "set -e\n" + strings.Join(batch, "\n") + "\n"
		batch, size = nil, 0
		_, err := d.exec(ctx, n, script)
		if err == nil {
			n.mu.Lock()
			n.applied++
			n.mu.Unlock()
		}
		return err
	}
	for _, c := range cmds {
		if size+len(c) > 1800 {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, c)
		size += len(c) + 1
	}
	return flush()
}

// nguid formats an NGUID as nvmet's device_nguid takes it (8-4-4-4-12).
func nguid(h string) string {
	if len(h) != 32 {
		return h
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

// published is where this machine serves each node's targets.
func (d *datapath) published() map[string]string {
	out := map[string]string{}
	for _, n := range d.nodes {
		n.mu.Lock()
		for p, a := range n.pub {
			out[chassisID(n.dev.Path)+"/"+p] = a
		}
		n.mu.Unlock()
	}
	return out
}

func (d *datapath) reportLines() []string {
	var out []string
	for _, n := range d.nodes {
		n.mu.Lock()
		var ups []string
		for p := range n.ups {
			ups = append(ups, p)
		}
		sort.Strings(ups)
		line := fmt.Sprintf("%s: data path at %s on %s (pool drive %s): serving %s; set up %d times, %d changes applied, connections %v",
			n.dev.Path, n.addr, n.portRef, n.serial, orDash(strings.Join(ups, ",")), n.setups, n.applied, n.conns)
		if n.err != "" {
			line += "; last error: " + n.err
		}
		n.mu.Unlock()
		out = append(out, line)
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "nothing yet"
	}
	return s
}
