//go:build linux

package linuxnet

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// ansibleImage is ansible-core with NVIDIA's nvidia.nvue collection from Ansible Galaxy
// (test/linuxnet/ansible/Containerfile; scripts/linux-substrate-e2e.sh builds it).
const ansibleImage = "dctwin-ansible-nvue:test"

// The first push of a factory Cumulus fabric by NVIDIA's own Ansible collection, not by the twin's
// reading of NVUE: nvidia.nvue's modules over its httpapi plugin (ansible.netcommon.httpapi, HTTPS
// on 8765, basic auth) open one revision per switch (config: new), stage the hostname, the
// loopback and fabric ports, BGP and its neighbours in it (system, interface, router and vrf, each
// with revid, as the collection's keyed lists normalise to NVUE's objects), apply it (config:
// apply, force, polling the revision until applied) and read the applied system back (api: get).
// The playbook runs in a container on the OOB network beside the tooling's Kea; every switch's
// address comes from Kea's leases and its factory password is changed at first login, as the
// collection cannot answer PAM's prompt. The fabric converges.
func TestDay0Ansible(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-ansible", nos: nos.NVUEFRR, install: "preinstalled", ztp: "disabled"})
	ctx := context.Background()
	if !r.d.HasImage(ctx, ansibleImage) {
		t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh builds it)", ansibleImage)
	}
	fac := r.factory()
	addr := map[string]string{}
	waitFor(t, 5*time.Minute, "every factory switch's management lease in Kea", func() bool {
		l := r.leases()
		for _, e := range fac {
			if l[strings.ToLower(e.MgmtMAC)] == "" {
				return false
			}
			addr[e.Serial] = l[strings.ToLower(e.MgmtMAC)]
		}
		return true
	})
	hosts := map[string]any{}
	for _, e := range fac {
		ip := addr[e.Serial]
		s, _ := r.planOf(e.Serial)
		waitFor(t, 3*time.Minute, e.Hostname+"'s sshd", func() bool {
			c, err := r.p.dial(ctx, "tcp", ip+":22")
			if err == nil {
				c.Close()
			}
			return err == nil
		})
		r.changePassword(ip, nvue.FactoryPassword, newPassword)
		hosts[e.Hostname] = ansibleHostVars(ip, s)
	}

	dir := filepath.Join(filepath.Dir(day0Binaries(t)), "ansible-"+r.c.estate+"-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	inv, _ := json.MarshalIndent(map[string]any{"all": map[string]any{"children": map[string]any{"switches": map[string]any{"hosts": hosts}},
		"vars": map[string]any{
			"ansible_connection": "ansible.netcommon.httpapi", "ansible_network_os": "nvidia.nvue.httpapi",
			"ansible_httpapi_port": nvue.APIPort, "ansible_httpapi_use_ssl": true, "ansible_httpapi_validate_certs": false,
			"ansible_user": nvue.User, "ansible_password": newPassword,
		}}}, "", "  ")
	for name, b := range map[string][]byte{"inventory.yaml": inv, "day0.yaml": []byte(ansiblePlaybook)} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// beside Kea on the OOB network: its network namespace, routed to the switches' subnets
	c, err := r.d.Run(ctx, nos.Spec{Name: "dctwin-ansible-" + r.c.estate + "-" + strconv.Itoa(os.Getpid()), Image: ansibleImage,
		Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"sleep infinity"}, NetworkMode: "container:" + r.kea.ID,
		Binds: []string{dir + ":/work"}, Labels: map[string]string{"dc-twin.test": "linuxnet"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.d.Remove(context.Background(), c.ID) })
	play := func(why string) {
		t.Helper()
		start := time.Now()
		out, code, err := r.d.Exec(ctx, c.ID, "/bin/sh", "-c",
			"cd /work && ANSIBLE_NOCOLOR=1 ANSIBLE_PERSISTENT_COMMAND_TIMEOUT=300 ANSIBLE_PERSISTENT_CONNECT_TIMEOUT=60 "+
				"ansible-playbook -i inventory.yaml day0.yaml -v 2>&1")
		t.Logf("ansible-playbook, %s (%s, exit %d):\n%s", why, time.Since(start).Round(time.Second), code, indent(out))
		if err != nil || code != 0 {
			t.Fatalf("nvidia.nvue's playbook failed (%s): exit %d, %v", why, code, err)
		}
		if !strings.Contains(out, "failed=0") || strings.Contains(out, "unreachable=1") {
			t.Fatalf("the play recap has failures (%s)", why)
		}
		took := r.waitConverged(10 * time.Minute)
		t.Logf("the fabric converged %s after nvidia.nvue's push (%s)", took.Round(time.Second), why)
	}
	play("the first push")
	r.checkPhases()
	// the same play again on the converged fabric applies and changes nothing that breaks it
	play("run again")
	// every switch factory-reset (dctwin switch reset --factory): the tooling changes the factory
	// password again and re-runs the same play, and the fabric is the same again (Z1)
	for _, e := range fac {
		if err := r.sub.FactoryReset(ctx, e.Path); err != nil {
			t.Fatalf("%s: factory reset: %v", e.Hostname, err)
		}
	}
	for _, e := range fac {
		ip := addr[e.Serial]
		waitFor(t, 3*time.Minute, e.Hostname+" taking its factory password again", func() bool {
			return r.nvueStatus(ip, nvue.User, newPassword) == http.StatusUnauthorized
		})
		waitFor(t, 3*time.Minute, e.Hostname+"'s sshd", func() bool {
			c, err := r.p.dial(ctx, "tcp", ip+":22")
			if err == nil {
				c.Close()
			}
			return err == nil
		})
		r.changePassword(ip, nvue.FactoryPassword, newPassword)
	}
	play("after a factory reset of every switch")
	r.checkPhases()
	applies := 0
	for _, ev := range r.tw.Engine.Events(0, nil) {
		if ev.Kind == "request" && ev.Request != nil && ev.Request.Canonical == "network.config.apply" && ev.Source.Principal == nvue.User {
			applies++
		}
	}
	if applies < len(fac) {
		t.Errorf("%d NVUE applies by user cumulus in the event log, want one per switch (%d)", applies, len(fac))
	}
	r.judge()
}

// ansibleHostVars are one switch's plan as nvidia.nvue's modules take it: keyed lists of {id: ...}
// (interfaces, addresses, link states, VRFs, neighbours) and snake_case keys, which the
// collection's httpapi plugin turns into NVUE's objects and hyphenated keys.
func ansibleHostVars(ip string, s day0.PlanSwitch) map[string]any {
	ifs := []any{map[string]any{"id": "lo", "type": "loopback", "ip": map[string]any{"address": []any{map[string]any{"id": s.Loopback}}}}}
	var nbrs []any
	for _, it := range s.Interfaces {
		if it.Kind != "fabric" {
			continue
		}
		ifs = append(ifs, map[string]any{"id": it.Name, "type": "swp", "link": map[string]any{"state": []any{map[string]any{"id": "up"}}},
			"ip": map[string]any{"address": []any{map[string]any{"id": it.Address}}}})
		nbrs = append(nbrs, map[string]any{"id": it.PeerAddr, "type": "numbered", "remote_as": strconv.FormatUint(uint64(it.PeerASN), 10)})
	}
	return map[string]any{
		"ansible_host":    ip,
		"day0_hostname":   s.Hostname,
		"day0_router":     map[string]any{"bgp": map[string]any{"enable": "on", "autonomous_system": s.ASN, "router_id": s.RouterID}},
		"day0_interfaces": ifs,
		"day0_vrfs": []any{map[string]any{"id": "default", "router": map[string]any{"bgp": map[string]any{"enable": "on", "neighbor": nbrs,
			"address_family": map[string]any{"ipv4_unicast": map[string]any{"enable": "on",
				"redistribute": map[string]any{"connected": map[string]any{"enable": "on"}}}}}}}},
	}
}

// ansiblePlaybook is the day-0 push with nvidia.nvue's modules, one revision per switch.
const ansiblePlaybook = `- name: day-0 push with nvidia.nvue
  hosts: switches
  gather_facts: false
  tasks:
    - name: open a revision
      nvidia.nvue.config:
        state: new
      register: rev

    - name: hostname
      nvidia.nvue.system:
        state: merged
        revid: "{{ rev.revid }}"
        data:
          hostname: "{{ day0_hostname }}"

    - name: loopback and fabric ports
      nvidia.nvue.interface:
        state: merged
        revid: "{{ rev.revid }}"
        data: "{{ day0_interfaces }}"

    - name: BGP
      nvidia.nvue.router:
        state: merged
        revid: "{{ rev.revid }}"
        data: "{{ day0_router }}"

    - name: BGP neighbours in the default VRF
      nvidia.nvue.vrf:
        state: merged
        revid: "{{ rev.revid }}"
        data: "{{ day0_vrfs }}"

    - name: apply the revision
      nvidia.nvue.config:
        state: apply
        revid: "{{ rev.revid }}"
        force: true
        wait: 120
      register: applied

    - name: the revision is applied
      ansible.builtin.assert:
        that: applied.revid.state == 'applied'
        fail_msg: "{{ applied.revid | to_json }}"

    - name: read the applied system back
      nvidia.nvue.api:
        operation: get
        path: system
      register: system

    - name: the switch runs its planned hostname
      ansible.builtin.assert:
        that: system.message.hostname == day0_hostname
        fail_msg: "{{ system | to_json }}"
`
