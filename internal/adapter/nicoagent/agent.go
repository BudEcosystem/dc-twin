package nicoagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// What runs on the DPU once it has booted, impersonated (D55). NICo's DPU image (carbide.efi,
// booted over the network while the DPU is in DPUInitializing/Init) reports the DPU's hardware
// to nico-api (DiscoverMachine, reporter DPU_AGENT), takes the machine certificate it is given,
// and installs the DPU's OS; the OS's forge-dpu-agent then asks nico-api what to do
// (ForgeAgentControl: DiscoveryCompleted when told to finish discovery), and periodically fetches
// the managed host's network configuration (GetManagedHostNetworkConfig) and reports it applied
// with the DPU healthy (RecordDpuNetworkStatus). The stand-in makes those calls, with the
// payloads NICo's own simulator (machine-a-tron) sends, from the DPU's address; it applies
// nothing: no HBN, no OVS, no ConnectX configuration.

// installed is what a machine keeps across boots once NICo's image has run on it: where
// nico-api is, the site's root CA and the machine certificate it was issued (on a DPU, its disk;
// scout keeps it only while it runs, and the twin keeps it for the report).
type installed struct {
	InterfaceID string `json:"interfaceId"`
	MachineID   string `json:"machineId"`
	APIURL      string `json:"apiUrl"`
	PXEURL      string `json:"pxeUrl"`
	RootCA      []byte `json:"rootCa"`
	Cert        []byte `json:"cert,omitempty"`
	Key         []byte `json:"key,omitempty"`
}

func (b *machine) loadInstalled() *installed {
	raw, err := os.ReadFile(b.statePath)
	if err != nil {
		return nil
	}
	var in installed
	if json.Unmarshal(raw, &in) != nil || in.MachineID == "" {
		return nil
	}
	return &in
}

func (b *machine) saveInstalled(in *installed) error {
	raw, _ := json.MarshalIndent(in, "", "  ")
	return os.WriteFile(b.statePath, raw, 0o600)
}

// runImage is NICo's DPU image: discovery, then the installed OS's agent.
func (b *dpu) runImage(ctx context.Context, out bootOutcome) error {
	in := &installed{
		InterfaceID: cmdlineArg(out.cmdline, "machine_id"),
		APIURL:      cmdlineArg(out.cmdline, "server_uri"),
		PXEURL:      cmdlineArg(out.cmdline, "pxe_uri"),
	}
	if in.InterfaceID == "" {
		return fmt.Errorf("the kernel command line names no machine_id: %q", out.cmdline)
	}
	if in.APIURL == "" || strings.HasPrefix(in.APIURL, "[") {
		return fmt.Errorf("the kernel command line names no server_uri NICo filled in: %q", out.cmdline)
	}
	if in.PXEURL != "" && !strings.HasPrefix(in.PXEURL, "[") {
		u, err := url.Parse(strings.TrimRight(in.PXEURL, "/") + "/api/v0/tls/root_ca")
		if err != nil {
			return err
		}
		ca, err := b.get(ctx, u, true)
		if err != nil {
			return fmt.Errorf("the site's root CA: %w", err)
		}
		in.RootCA = ca
	}
	conn, err := b.connect(in)
	if err != nil {
		return err
	}
	res, err := b.forge.call(ctx, conn, "DiscoverMachine", obj{
		"machine_interface_id": obj{"value": in.InterfaceID},
		"info":                 b.discoveryInfo(),
		"create_machine":       true,
		"discovery_reporter":   "MACHINE_DISCOVERY_REPORTER_DPU_AGENT",
	})
	conn.Close()
	if err != nil {
		return fmt.Errorf("DiscoverMachine: %w", err)
	}
	in.MachineID = str(res, "machine_id", "id")
	if in.MachineID == "" {
		return errors.New("DiscoverMachine returned no machine id")
	}
	if mc, ok := res["machine_certificate"].(map[string]any); ok {
		in.Cert, _ = base64.StdEncoding.DecodeString(fmt.Sprint(mc["public_key"]))
		in.Key, _ = base64.StdEncoding.DecodeString(fmt.Sprint(mc["private_key"]))
	}
	if err := b.saveInstalled(in); err != nil {
		return err
	}
	b.note(fmt.Sprintf("discovered as NICo machine %s (interface %s)", in.MachineID, in.InterfaceID))
	return b.runAgent(ctx, in)
}

// runAgent is the installed DPU OS's forge-dpu-agent, until the DPU powers off or resets.
func (b *dpu) runAgent(ctx context.Context, in *installed) error {
	conn, err := b.connect(in)
	if err != nil {
		return err
	}
	defer conn.Close()
	b.step(stepOS)
	b.agentUp.Store(true)
	defer b.agentUp.Store(false)
	control := time.NewTicker(b.a.controlEvery)
	defer control.Stop()
	network := time.NewTicker(b.a.networkEvery)
	defer network.Stop()
	b.agentControl(ctx, conn, in)
	b.networkStatus(ctx, conn, in)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-control.C:
			b.agentControl(ctx, conn, in)
		case <-network.C:
			b.networkStatus(ctx, conn, in)
		}
	}
}

func (b *dpu) agentControl(ctx context.Context, conn *grpc.ClientConn, in *installed) {
	res, err := b.forge.call(ctx, conn, "ForgeAgentControl", obj{"machine_id": obj{"id": in.MachineID}})
	if err != nil {
		b.callFailed("ForgeAgentControl", err)
		return
	}
	b.called("ForgeAgentControl")
	action, _ := res["legacy_action"].(string)
	if _, ok := res["discovery"]; ok || action == "DISCOVERY" {
		if _, err := b.forge.call(ctx, conn, "DiscoveryCompleted", obj{"machine_id": obj{"id": in.MachineID}}); err != nil {
			b.callFailed("DiscoveryCompleted", err)
			return
		}
		b.called("DiscoveryCompleted")
	}
}

func (b *dpu) networkStatus(ctx context.Context, conn *grpc.ClientConn, in *installed) {
	cfg, err := b.forge.call(ctx, conn, "GetManagedHostNetworkConfig", obj{"dpu_machine_id": obj{"id": in.MachineID}})
	if err != nil {
		b.callFailed("GetManagedHostNetworkConfig", err)
		return
	}
	b.called("GetManagedHostNetworkConfig")
	st := obj{
		"dpu_machine_id":         obj{"id": in.MachineID},
		"network_config_version": str(cfg, "managed_host_config_version"),
		"dpu_agent_version":      agentVersion,
		"dpu_health":             obj{"source": "forge-dpu-agent", "successes": []any{}, "alerts": []any{}},
	}
	if id, ok := cfg["instance_id"]; ok {
		st["instance_id"] = id
	}
	var ifaces []any
	if use, _ := cfg["use_admin_network"].(bool); use {
		if a, ok := cfg["admin_interface"].(map[string]any); ok {
			ifaces = append(ifaces, observation(a, b.id.hostMAC))
		}
	} else {
		st["instance_network_config_version"] = str(cfg, "instance_network_config_version")
		if ts, ok := cfg["tenant_interfaces"].([]any); ok {
			for _, t := range ts {
				if m, ok := t.(map[string]any); ok {
					ifaces = append(ifaces, observation(m, b.id.hostMAC))
				}
			}
		}
	}
	if len(ifaces) > 0 {
		st["interfaces"] = ifaces
	}
	if _, err := b.forge.call(ctx, conn, "RecordDpuNetworkStatus", st); err != nil {
		b.callFailed("RecordDpuNetworkStatus", err)
		return
	}
	b.called("RecordDpuNetworkStatus")
	b.setHostConfig(cfg, in.PXEURL)
	b.synced(str(cfg, "managed_host_config_version"))
}

// agentVersion is what the stand-in reports as forge-dpu-agent's version: NICo records it and
// acts on it only under an agent upgrade policy, which is off by default.
const agentVersion = "dc-twin-impersonated"

// observation is an interface's status as the agent reports it applied: the addresses NICo
// configured on it.
func observation(cfg map[string]any, hostMAC string) obj {
	o := obj{"mac_address": hostMAC}
	if ft, ok := cfg["function_type"]; ok {
		o["function_type"] = ft
	}
	if vf, ok := cfg["virtual_function_id"]; ok {
		o["virtual_function_id"] = vf
	}
	if ip, gw, pfx, err := ifaceV4(cfg); err == nil {
		o["addresses"] = []any{ip.String()}
		o["prefixes"] = []any{firstOf(str(cfg, "interface_prefix"), pfx.String())}
		o["gateways"] = []any{gw.String()}
	}
	if nsg, ok := cfg["network_security_group"].(map[string]any); ok {
		o["network_security_group"] = obj{"source": nsg["source"], "id": nsg["id"], "version": nsg["version"]}
	}
	return o
}

// discoveryInfo is the DPU's hardware as NICo's DPU image reports it: a BlueField-3's (the
// payload machine-a-tron sends), with the identity the DPU's BMC serves: its system serial as
// the DMI product serial and the two placeholder serials a BlueField's DMI carries (NICo derives
// the DPU's machine id from these, and its site explorer derived the same from the BMC), and its
// BaseMAC as the factory MAC (the host PF's, by which NICo pairs the DPU with its host).
func (b *dpu) discoveryInfo() obj {
	return obj{
		"machine_type": "aarch64",
		"machine_arch": "AARCH64",
		"cpu_info":     []any{obj{"model": "Cortex-A78AE", "vendor": "ARM", "sockets": 1, "cores": 16, "threads": 16}},
		"block_devices": []any{
			obj{"model": "KBG40ZPZ128G TOSHIBA MEMORY", "revision": "AEGA0103", "serial": b.id.serial + "-EMMC", "device_type": "disk"},
		},
		"dmi_data": obj{
			"board_name": "BlueField-3 DPU", "board_version": "AG", "bios_version": b.id.uefi, "bios_date": "",
			"product_serial": b.id.serial, "board_serial": "Unspecified Base Board Serial Number",
			"chassis_serial": "Unspecified Chassis Board Serial Number", "product_name": b.id.product, "sys_vendor": "Nvidia",
		},
		"dpu_info": obj{
			"part_number": b.id.partNumber, "part_description": "NVIDIA BlueField-3 " + b.id.partNumber,
			"product_version": b.id.nic, "factory_mac_address": b.id.hostMAC, "firmware_version": b.id.nic,
			"firmware_date": "", "switches": []any{},
		},
	}
}

// connect dials nico-api from the machine's address over TLS, verifying it against the site's
// root CA, with the machine certificate once the machine has one.
func (b *machine) connect(in *installed) (*grpc.ClientConn, error) {
	u, err := url.Parse(in.APIURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("server_uri %q is not a URL", in.APIURL)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host, port = u.Host, "443"
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if len(in.RootCA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(in.RootCA) {
			return nil, errors.New("the site's root CA is not PEM")
		}
		cfg.RootCAs = pool
	} else if b.a.rootCA != nil {
		cfg.RootCAs = b.a.rootCA
	}
	if len(in.Cert) > 0 && len(in.Key) > 0 {
		pair, err := tls.X509KeyPair(in.Cert, in.Key)
		if err != nil {
			return nil, fmt.Errorf("the machine certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return grpc.NewClient("passthrough:///"+net.JoinHostPort(host, port),
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return b.nic.dial(ctx, "tcp", addr) }))
}

// str reads a string at a path of nested objects.
func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

// grpcReason is a call's failure as a short reason.
func grpcReason(err error) string {
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return s.Code().String() + ": " + s.Message()
	}
	return err.Error()
}
