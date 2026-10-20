# dc-twin

**A standalone emulated datacenter for testing the software that runs real datacenters.**

dc-twin builds a datacenter out of emulated devices: BMCs, servers, accelerators, DPUs, switches,
fabric managers, power shelves, PDUs, CDUs and leak sensors. Every device speaks the protocol its
real counterpart speaks, so software installed on top can be tested **unmodified**. That software
includes bare-metal lifecycle managers, fabric managers, fleet-health agents, Kubernetes operators
and facility integrations.

The first stack it targets is **NVIDIA DSX OS**: Infra Controller (NICo), DOCA Platform Framework,
NVSentinel, Fleet Intelligence, DSX Exchange, AI Cluster Runtime and KAI Scheduler. The core is
vendor-neutral. AMD Instinct, Intel Gaudi, other OEMs and other rack formats are added as **packs**
of device profiles and stack definitions, without changing the engine.

> **Status: a first implementation runs, and real stacks run against it.** The engine, the
> compiler, ten packs and twelve protocol adapters (Redfish, DHCP with the twin relay and boot
> walker, DSX Exchange BMS, SNMP, IPMI, UFM, NVUE, NMX-C, RMS, a Kubernetes projection, accelerator
> exporters, and `vm` hosts on QEMU), on a loopback or a Linux TAP network substrate with real NOS
> containers, run, each checked against a real, unmodified client. Run against the twin, unmodified:
> **NVIDIA Infra Controller (NICo)** itself (22 PASS, 0 FAIL: it leases, explores and ingests every
> GB300 tray with its DPU, brings its NVLink switches and power shelves to `ready`, powers off a
> leaking tray through the twin's RMS and has the twin's BMS isolate a rack), NICo's `libredfish`,
> `libnmxc` and `librms`, **OpenStack Ironic** (125 PASS), **Tinkerbell** (144 of 144), **KAI
> Scheduler**, **AICR** and **Prometheus**, sushy, gofish, bmclib, ISC Kea, FRR with ISC dhcrelay,
> ipmitool, net-snmp and a kube-apiserver. Scenarios run end to end across power, cooling, leaks,
> accelerator faults and BMC failures. What is not built (the in-host agent, a DPU OS, SONiC-VS on
> ARM hosts) and every result with its limits are in the
> **[implementation status](docs/implementation.md)**. The specification (draft 0.3) is in
> [docs/spec](docs/spec/README.md); the [console](prototype/README.md) runs live against a running
> twin (`dctwin up … --console prototype`, then `dctwin ui`) and as an in-memory demo without one.

## Quick start

```bash
make build                                   # bin/dctwin, bin/refstack
bin/dctwin assets fetch                      # the captured BMC trees vendor dialects bind (MIT)
bin/dctwin up examples/estates/lab-model.yaml --adapters redfish,dhcp,bms,snmp,ipmi,ufm,nvue
bin/dctwin status
bin/dctwin fault inject 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79
bin/dctwin scenario run examples/scenarios/twin/xid-and-switch-tray.yaml
bin/dctwin down
make test                                    # unit, adapter and end-to-end suites
make compat-libredfish                       # NICo's libredfish integration suite against the twin
```

## What it is for

- **Testing a datacenter stack before the hardware arrives.** Rack ingestion, firmware baselines,
  tenant hand-over and sanitisation, fault remediation, leak response, power capping.
- **Running the failures you can't safely stage on real machines.** A leak in rack A07, a switch
  tray dying mid-allocation, a BMC that stops answering, a GPU that falls off the bus. Each one is
  injected into the model and shows up consistently on every interface that would reveal it.
- **Scale.** Thousands of emulated BMCs and Kubernetes nodes on one machine, at reduced fidelity.
- **Comparing stacks.** The same estate and the same scenarios, run against different vendors'
  software.
- **Seeing and steering it.** A web console shows the estate from hall floor to rack elevation to
  a single GPU. It adds racks and machines, injects faults and technician acts, and follows every
  request the software under test makes in response ([UI](docs/spec/12-ui.md)).

## What it is not

- **Not a performance simulator.** No CUDA or ROCm kernels run, no NCCL bandwidth exists, and no
  packet in the twin says anything about what real silicon would do.
- **Not a security proof.** Hardware-rooted guarantees, such as DPU-enforced isolation,
  confidential-computing attestation and real firmware signing, cannot be proven in emulation. The
  twin says so wherever a result depends on them ([fidelity](docs/spec/07-fidelity.md)).
- **Not a replacement for hardware.** Whatever the twin cannot model is listed, and its
  hardware-in-the-loop mode exists to keep the emulators honest.

## Prior art

- [RafaySystems/brave](https://github.com/RafaySystems/brave) — virtual bare metal for EKS
  Anywhere on one cloud instance. It inspired the "one big box" deployment model here.
- The mocks NVIDIA ships inside its own components. dc-twin reuses them where they fit
  ([NVIDIA DSX OS stack](docs/stacks/nvidia-dsx-os.md)).
- DMTF's Redfish mockups.
- [containerlab](https://containerlab.dev).

## License

Apache-2.0. dc-twin never redistributes vendor binaries or images that their licences don't allow
to be redistributed. A stack pack says how to obtain them ([D13](docs/spec/11-decisions.md)).
