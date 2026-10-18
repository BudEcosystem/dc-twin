// packs.js — the hardware as data: device profiles, rack formats with layout, Redfish dialects and
// fault catalogues (spec 04, 05, 06, 08). Everything here is PROTOTYPE DATA: counts and versions are
// illustrative and nothing has been checked against hardware (07 — the fidelity ledger).
window.DCT = window.DCT || {};
DCT.packs = (() => {
  const profiles = {};
  const formats = {};
  const faults = [];
  const dialects = {};

  // ------------------------------------------------------------------ profiles
  const BMC = { kind: 'bmc', prefix: 'bmc', count: 1, model: 'BMC' };
  const TPM = { kind: 'tpm', prefix: 'tpm', count: 1, model: 'TPM 2.0' };

  profiles['nvidia/gb300-compute-tray'] = {
    pack: 'nvidia', class: 'compute-tray', vendor: 'NVIDIA', title: 'GB300 compute tray', units: 1, dialect: 'nvidia/gb-bmc',
    components: [
      { kind: 'cpu', prefix: 'cpu', count: 2, model: 'NVIDIA Grace, 72 cores' },
      { kind: 'accelerator', prefix: 'gpu', count: 4, model: 'NVIDIA B300', vendor: 'nvidia', nvlinkLanes: 18, maxW: 1400 },
      { kind: 'nic', prefix: 'cx8', count: 4, model: 'ConnectX-8 800G', role: 'scale-out' },
      { kind: 'dpu', prefix: 'bf3', count: 1, model: 'BlueField-3', role: 'frontend' },
      { kind: 'drive', prefix: 'nvme', count: 4, model: 'NVMe 3.84 TB' },
      TPM, { ...BMC, model: 'NVIDIA BMC + HMC' },
    ],
    ports: [
      { name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' },
      { name: 'bf3-p{0..1}', kind: 'ethernet', speed: '400G', network: 'frontend' },
      { name: 'cx8-{0..3}', kind: 'infiniband', speed: '800G', network: 'backend' },
      { name: 'pwr-in', kind: 'power-in', source: 'busbar' },
    ],
    power: { standbyW: 60, idleW: 1200, maxW: 6000 }, cooling: 'liquid',
    firmware: [['bmc', '25.09.12'], ['uefi', '1.07.03'], ['hmc', '25.09.12'], ['gpu-vbios', '97.10.3A.00.01'], ['cx8', '40.44.1036'], ['bluefield-bfb', '3.2.2'], ['cpld', '0.3.1']],
    behaviours: { onSeconds: 8, offSeconds: 3, postSeconds: 6 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/thermal', 'generic/firmware', 'generic/tpm', 'nvidia/xid', 'nvidia/dpu'],
    fidelity: { model: true, vm: { arch: 'aarch64', inHost: ['nvidia/mokka'] } },
    isNot: ['No NVLink data path: NVLink ports carry state only.', 'A vm-fidelity tray runs QEMU aarch64 cores, not Grace.', 'The HMC is projected through the BMC Redfish tree; it has no separate network presence.'],
  };
  profiles['nvidia/nvlink-switch-tray-gb300'] = {
    pack: 'nvidia', class: 'switch-tray', vendor: 'NVIDIA', title: 'NVLink switch tray', units: 1, dialect: 'nvidia/nvswitch-tray',
    components: [{ kind: 'asic', prefix: 'nvswitch', count: 2, model: 'NVSwitch 5' }, { ...BMC, model: 'NVIDIA BMC' }],
    ports: [{ name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'pwr-in', kind: 'power-in', source: 'busbar' }],
    nvlinkLanes: 144,
    power: { standbyW: 40, idleW: 600, maxW: 1500 }, cooling: 'liquid',
    firmware: [['bmc', '25.09.12'], ['nvos', '25.02.2'], ['nvswitch-fw', '96.10.44.00.02'], ['cpld', '0.2.4']],
    behaviours: { onSeconds: 6, offSeconds: 3 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/thermal', 'generic/firmware', 'nvidia/nvlink'],
    fidelity: { model: true },
    isNot: ['NVOS is emulated at the NVUE REST API; no NVOS image runs.', 'No NVLink data path.'],
  };
  profiles['nvidia/power-shelf-33kw'] = {
    pack: 'nvidia', class: 'power-shelf', vendor: 'Liteon', title: 'Power shelf 33 kW', units: 1, dialect: 'liteon/power-shelf',
    components: [{ kind: 'psu', prefix: 'psu', count: 6, model: 'PSU 5.5 kW' }, { ...BMC, model: 'Shelf controller' }],
    ports: [{ name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'ac-in-{a,b}', kind: 'power-in', source: 'feed' }, { name: 'dc-out', kind: 'power-out', target: 'busbar' }],
    power: { standbyW: 25, idleW: 25, maxW: 33000 }, cooling: 'air',
    firmware: [['shelf-fw', '2.4.0'], ['psu-fw', '1.12']],
    behaviours: { onSeconds: 2, offSeconds: 1 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/firmware'],
    fidelity: { model: true },
    isNot: ['Output power is the sum the model assigns; no electrical model.'],
  };
  profiles['generic/mgmt-switch-1g'] = {
    pack: 'generic', class: 'switch', vendor: 'Generic', title: 'Management switch 1G', units: 1, dialect: 'generic/openbmc',
    components: [{ kind: 'asic', prefix: 'asic', count: 1, model: 'Switch ASIC' }, { ...BMC, model: 'Switch CPU' }],
    ports: [{ name: 'eth{1..48}', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'up{1..2}', kind: 'ethernet', speed: '10G', network: 'oob' }],
    power: { standbyW: 20, idleW: 60, maxW: 120 }, cooling: 'air',
    firmware: [['nos', '4.4.0']], behaviours: { onSeconds: 4, offSeconds: 1 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/link'], fidelity: { model: true, vm: { nos: 'sonic-vs' } },
    isNot: ['At model fidelity no routing protocol runs; at vm the NOS is SONiC-VS, not the vendor image.'],
  };
  profiles['generic/leak-sensor'] = {
    pack: 'generic', class: 'sensor', vendor: 'Generic', title: 'Leak sensor (rope)', units: 0,
    components: [], ports: [], power: { standbyW: 1, idleW: 1, maxW: 1 }, cooling: 'none', firmware: [],
    behaviours: {}, faultPacks: ['generic/leak'], fidelity: { model: true }, bms: { objectType: 'Sensor', pointType: 'Leak' },
    isNot: ['A boolean. No fluid model: where a real leak would spread is the scenario\'s business.'],
  };
  profiles['generic/cdu-inrow'] = {
    pack: 'generic', class: 'cdu', vendor: 'Generic', title: 'In-row CDU', units: 0,
    components: [{ kind: 'pump', prefix: 'pump', count: 2, model: 'Pump' }, { kind: 'valve', prefix: 'valve', count: 1, model: 'Control valve' }],
    ports: [], power: { standbyW: 400, idleW: 2000, maxW: 6000 }, cooling: 'none', firmware: [['controller', '3.1']],
    behaviours: {}, faultPacks: ['generic/thermal'], fidelity: { model: true }, bms: { objectType: 'CDU' },
    isNot: ['First-order thermal model only: supply, return, flow and pressure track targets with a lag.'],
  };
  profiles['generic/pdu-vertical'] = {
    pack: 'generic', class: 'pdu', vendor: 'Generic', title: 'Vertical PDU (0U, 3-phase 63 A)', units: 0, dialect: 'generic/openbmc',
    components: [{ kind: 'breaker', prefix: 'breaker', count: 3, model: 'Breaker 63 A' }],
    ports: [{ name: 'mgmt', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'ac-in', kind: 'power-in', source: 'feed' }, { name: 'out{1..36}', kind: 'power-out' }],
    power: { standbyW: 10, idleW: 10, maxW: 22000 }, cooling: 'air', firmware: [['pdu-fw', '6.0.2']], behaviours: { onSeconds: 1, offSeconds: 1 },
    faultPacks: ['generic/power', 'generic/bmc'], fidelity: { model: true }, snmp: true,
    isNot: ['SNMP MIB data is generic; no vendor MIB.'],
  };
  profiles['generic/leaf-switch-800g'] = {
    pack: 'generic', class: 'switch', vendor: 'Generic', title: 'Leaf switch 64×800G', units: 1, dialect: 'generic/openbmc', nos: 'sonic-vs',
    components: [{ kind: 'asic', prefix: 'asic', count: 1, model: 'Switch ASIC 51.2T' }, { kind: 'psu', prefix: 'psu', count: 2, model: 'PSU' }, { kind: 'fan', prefix: 'fan', count: 6, model: 'Fan' }, { ...BMC, model: 'Switch BMC' }],
    ports: [{ name: 'mgmt', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'eth{1..64}', kind: 'ethernet', speed: '800G' }],
    power: { standbyW: 30, idleW: 900, maxW: 2200 }, cooling: 'air', firmware: [['nos', 'SONiC 202505'], ['bmc', '1.9']], behaviours: { onSeconds: 6, offSeconds: 1 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/thermal', 'generic/link'], fidelity: { model: true, vm: { nos: 'sonic-vs' } },
    isNot: ['At model fidelity: cabling, port state and LLDP only; no BGP or EVPN run (05 — Model switches).'],
  };
  profiles['generic/spine-switch-800g'] = { ...profiles['generic/leaf-switch-800g'], title: 'Spine switch 64×800G' };
  profiles['generic/rail-leaf-ib'] = { ...profiles['generic/leaf-switch-800g'], title: 'InfiniBand rail leaf (Quantum-X800)', vendor: 'NVIDIA', pack: 'nvidia', nos: 'mlnx-os',
    ports: [{ name: 'mgmt', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'ib{1..144}', kind: 'infiniband', speed: '800G' }],
    isNot: ['No InfiniBand packet path: ports, GUIDs and partitions exist behind UFM REST only.'] };
  profiles['generic/rail-leaf-roce'] = { ...profiles['generic/leaf-switch-800g'], title: 'RoCE rail leaf 64×800G' };
  profiles['generic/oob-agg-switch'] = { ...profiles['generic/mgmt-switch-1g'], title: 'OOB aggregation switch', ports: [{ name: 'eth{1..48}', kind: 'ethernet', speed: '10G', network: 'oob' }] };
  profiles['nvidia/ufm-appliance'] = {
    pack: 'nvidia', class: 'appliance', vendor: 'NVIDIA', title: 'UFM appliance', units: 1, dialect: 'oem/dell-idrac',
    components: [{ kind: 'cpu', prefix: 'cpu', count: 2, model: 'Xeon' }, { kind: 'nic', prefix: 'hca', count: 2, model: 'ConnectX-7' }, BMC],
    ports: [{ name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'ib{0..1}', kind: 'infiniband', speed: '400G', network: 'backend' }],
    power: { standbyW: 30, idleW: 300, maxW: 800 }, cooling: 'air', firmware: [['bmc', '7.20.60.50'], ['uefi', '2.7.5'], ['ufm', '6.22.1']], behaviours: { onSeconds: 10, offSeconds: 3 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/firmware'], fidelity: { model: true },
    isNot: ['UFM REST is served by the twin\'s adapter from the model; the appliance is a placeholder for where it would run.'],
  };

  // 8-GPU servers from three vendors — the same compiler and the same console draw all of them (D18).
  profiles['oem/dell/xe9680-h100'] = {
    pack: 'oem/dell', class: 'server', vendor: 'Dell', title: 'PowerEdge XE9680, 8× H100 SXM', units: 6, dialect: 'oem/dell-idrac',
    components: [
      { kind: 'cpu', prefix: 'cpu', count: 2, model: 'Xeon Platinum 8468' },
      { kind: 'accelerator', prefix: 'gpu', count: 8, model: 'NVIDIA H100 SXM 80 GB', vendor: 'nvidia', nvlinkLanes: 18, maxW: 700 },
      { kind: 'nic', prefix: 'cx7', count: 8, model: 'ConnectX-7 400G', role: 'scale-out' },
      { kind: 'nic', prefix: 'fe', count: 2, model: 'ConnectX-7 100G', role: 'frontend' },
      { kind: 'drive', prefix: 'nvme', count: 8, model: 'NVMe 3.84 TB' },
      { kind: 'psu', prefix: 'psu', count: 6, model: 'PSU 2.8 kW' }, { kind: 'fan', prefix: 'fan', count: 10, model: 'Fan' }, TPM, { ...BMC, model: 'iDRAC9' },
    ],
    ports: [{ name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'fe-{0..1}', kind: 'ethernet', speed: '100G', network: 'frontend' }, { name: 'be-{0..7}', kind: 'ethernet', speed: '400G', network: 'backend' }, { name: 'pwr-in-{a,b}', kind: 'power-in', source: 'pdu' }],
    power: { standbyW: 90, idleW: 1500, maxW: 10200 }, cooling: 'air',
    firmware: [['bmc', '7.20.60.50'], ['uefi', '2.7.4'], ['cpld', '1.6.0'], ['gpu-vbios', '96.00.74.00.1F'], ['cx7', '28.43.2026'], ['nvswitch-fw', '96.00.52.00.01']],
    behaviours: { onSeconds: 10, offSeconds: 3, postSeconds: 8 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/thermal', 'generic/firmware', 'generic/tpm', 'nvidia/xid'],
    fidelity: { model: true, vm: { arch: 'x86_64', inHost: ['nvidia/mokka'] } },
    isNot: ['The HGX baseboard\'s NVSwitches are projected through iDRAC; no baseboard BMC of its own.'],
  };
  profiles['amd/instinct-mi355x-platform'] = {
    pack: 'amd', class: 'server', vendor: 'AMD', title: 'Instinct MI355X platform, 8× OAM', units: 8, dialect: 'oem/supermicro',
    components: [
      { kind: 'cpu', prefix: 'cpu', count: 2, model: 'EPYC 9575F' },
      { kind: 'accelerator', prefix: 'gpu', count: 8, model: 'AMD Instinct MI355X', vendor: 'amd', maxW: 1400 },
      { kind: 'nic', prefix: 'pollara', count: 8, model: 'Pensando Pollara 400', role: 'scale-out' },
      { kind: 'nic', prefix: 'fe', count: 2, model: '100G NIC', role: 'frontend' },
      { kind: 'drive', prefix: 'nvme', count: 8, model: 'NVMe 7.68 TB' },
      { kind: 'psu', prefix: 'psu', count: 6, model: 'PSU 3 kW' }, { kind: 'fan', prefix: 'fan', count: 12, model: 'Fan' }, TPM, { ...BMC, model: 'AMI MegaRAC' },
    ],
    ports: [{ name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'fe-{0..1}', kind: 'ethernet', speed: '100G', network: 'frontend' }, { name: 'be-{0..7}', kind: 'ethernet', speed: '400G', network: 'backend' }, { name: 'pwr-in-{a,b}', kind: 'power-in', source: 'pdu' }],
    power: { standbyW: 90, idleW: 1800, maxW: 14000 }, cooling: 'air',
    firmware: [['bmc', '1.03.18'], ['uefi', '2.7'], ['cpld', '1.3.0'], ['gpu-vbios', '113-MI355X-02'], ['pollara', '1.2.0']],
    behaviours: { onSeconds: 10, offSeconds: 3, postSeconds: 8 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/thermal', 'generic/firmware', 'generic/tpm', 'amd/ras'],
    fidelity: { model: true, vm: { arch: 'x86_64', inHost: ['amd/smi-shim (Q7)'] } },
    isNot: ['The AMD SMI shim is an open question (Q7); at vm fidelity accelerators are model-projected only.', 'Infinity Fabric between OAMs is state, not a data path.'],
  };
  profiles['intel/gaudi3-hls'] = {
    pack: 'intel', class: 'server', vendor: 'Intel', title: 'Gaudi 3 HLS, 8× OAM', units: 8, dialect: 'oem/supermicro',
    components: [
      { kind: 'cpu', prefix: 'cpu', count: 2, model: 'Xeon 6' },
      { kind: 'accelerator', prefix: 'hpu', count: 8, model: 'Intel Gaudi 3', vendor: 'intel', maxW: 900, roce: 24 },
      { kind: 'nic', prefix: 'fe', count: 2, model: '100G NIC', role: 'frontend' },
      { kind: 'drive', prefix: 'nvme', count: 8, model: 'NVMe 3.84 TB' },
      { kind: 'psu', prefix: 'psu', count: 6, model: 'PSU 3 kW' }, { kind: 'fan', prefix: 'fan', count: 12, model: 'Fan' }, TPM, { ...BMC, model: 'AMI MegaRAC' },
    ],
    ports: [{ name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'fe-{0..1}', kind: 'ethernet', speed: '100G', network: 'frontend' }, { name: 'so-{0..23}', kind: 'ethernet', speed: '200G', network: 'backend', note: '3 of each accelerator\'s 24 RoCE ports leave the node' }, { name: 'pwr-in-{a,b}', kind: 'power-in', source: 'pdu' }],
    power: { standbyW: 90, idleW: 1500, maxW: 10000 }, cooling: 'air',
    firmware: [['bmc', '1.03.19'], ['uefi', '2.6'], ['cpld', '1.2.0'], ['hpu-fw', '1.20.0']],
    behaviours: { onSeconds: 10, offSeconds: 3, postSeconds: 8 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/thermal', 'generic/firmware', 'generic/tpm', 'intel/gaudi'],
    fidelity: { model: true, vm: { arch: 'x86_64', inHost: ['intel/hlml-shim (Q7)'] } },
    isNot: ['The HLML shim is an open question (Q7).', 'The 21 in-node RoCE ports per accelerator are not modelled individually.'],
  };
  profiles['oem/dell/r760'] = {
    pack: 'oem/dell', class: 'server', vendor: 'Dell', title: 'PowerEdge R760', units: 2, dialect: 'oem/dell-idrac',
    components: [{ kind: 'cpu', prefix: 'cpu', count: 2, model: 'Xeon Gold 6448Y' }, { kind: 'nic', prefix: 'fe', count: 2, model: '25G NIC' }, { kind: 'drive', prefix: 'nvme', count: 8, model: 'NVMe 1.92 TB' }, { kind: 'psu', prefix: 'psu', count: 2, model: 'PSU 1.4 kW' }, { kind: 'fan', prefix: 'fan', count: 6, model: 'Fan' }, TPM, { ...BMC, model: 'iDRAC9' }],
    ports: [{ name: 'bmc0', kind: 'ethernet', speed: '1G', network: 'oob' }, { name: 'fe-{0..1}', kind: 'ethernet', speed: '25G', network: 'frontend' }, { name: 'pwr-in-{a,b}', kind: 'power-in', source: 'pdu' }],
    power: { standbyW: 25, idleW: 250, maxW: 900 }, cooling: 'air',
    firmware: [['bmc', '7.20.60.50'], ['uefi', '2.7.5'], ['cpld', '1.2.6']], behaviours: { onSeconds: 8, offSeconds: 2, postSeconds: 6 },
    faultPacks: ['generic/power', 'generic/bmc', 'generic/thermal', 'generic/firmware', 'generic/tpm'], fidelity: { model: true, vm: { arch: 'x86_64' } },
    isNot: [],
  };

  // ---------------------------------------------------------- rack formats
  // Counts are parameters with ranges (D14). `layout.front` lists slot groups top to bottom; the compiler
  // assigns U positions from it, and the console draws from it (D18). `$name` refers to a parameter.
  formats['nvidia/gb300-nvl72'] = {
    pack: 'nvidia', title: 'GB300 NVL72', vendor: 'NVIDIA', tag: 'NVL', archetype: 'rack-scale', height: 48, cooling: 'liquid', busbar: true,
    params: {
      computeTrays: { default: 18, allowed: [18], label: 'Compute trays' },
      switchTrays: { default: 9, allowed: [9], label: 'NVLink switch trays' },
      shelves: { default: 8, range: [6, 8], label: 'Power shelves' },
      powerShelf: { default: 'nvidia/power-shelf-33kw', allowed: ['nvidia/power-shelf-33kw'], label: 'Power shelf profile' },
    },
    slots: [
      { names: 'power-shelf-{01..$shelves}', profile: '$powerShelf' },
      { names: 'compute-tray-{01..$computeTrays}', profile: 'nvidia/gb300-compute-tray' },
      { names: 'switch-tray-{01..$switchTrays}', profile: 'nvidia/nvlink-switch-tray-gb300' },
      { names: 'mgmt-switch', profile: 'generic/mgmt-switch-1g' },
      { names: 'leak-sensor-{01..04}', profile: 'generic/leak-sensor', at: ['compute-tray-01', 'compute-tray-09', 'switch-tray-05', 'compute-tray-15'] },
    ],
    layout: {
      front: [
        { slots: 'power-shelf-{01..04}', group: 'power shelves' },
        { slots: 'mgmt-switch' },
        { slots: 'compute-tray-{01..10}' },
        { slots: 'switch-tray-{01..09}', group: 'NVLink switch trays' },
        { slots: 'compute-tray-{11..18}' },
        { slots: 'power-shelf-{05..08}', group: 'power shelves' },
      ],
      back: ['nvlink-spine', 'busbar', 'oob', 'backend-rails', 'frontend'],
    },
    domains: [{ kind: 'nvlink', members: 'compute-tray-*/gpu-*', manager: 'switch-tray-01', formation: 'all switch trays healthy' }],
    power: { capacityW: 132000, perShelfW: 33000, minShelves: 6 },
    isNot: ['The NVLink spine is fixed links in the model; no cartridge or cable is individually modelled.', 'Rack-sequenced power-on is a hook with illustrative ordering.'],
  };
  formats['nvidia/gb200-nvl72'] = { ...formats['nvidia/gb300-nvl72'], title: 'GB200 NVL72', pack: 'nvidia' };
  formats['nvidia/gb200-nvl36x2'] = {
    ...formats['nvidia/gb300-nvl72'], title: 'GB200 NVL36x2 (one rack of the pair)', tag: 'NVL36',
    params: { ...formats['nvidia/gb300-nvl72'].params, computeTrays: { default: 18, allowed: [9, 18], label: 'Compute trays (9 × 4-GPU or 18 × 2-GPU)' }, peer: { default: '', label: 'Peer rack (the other half of the domain)', text: true } },
    domains: [{ kind: 'nvlink', members: 'compute-tray-*/gpu-*', manager: 'switch-tray-01', spans: '$peer', formation: 'all switch trays of both racks healthy' }],
    isNot: ['Cross-rack NVLink cables are fixed links in the model.'],
  };
  const serverRack = (title, tag, vendor, defaultServer, allowedServers, units, maxServers, pack, capacityW = 44000) => ({
    pack, title, tag, vendor, archetype: 'server-scale', height: 42, cooling: 'air', busbar: false,
    params: {
      server: { default: defaultServer, allowed: allowedServers, label: 'Server profile' },
      servers: { default: 4, range: [1, maxServers], label: 'Servers' },
    },
    slots: [
      { names: 'leaf-{01..02}', profile: 'generic/leaf-switch-800g' },
      { names: 'mgmt-switch', profile: 'generic/mgmt-switch-1g' },
      { names: 'server-{01..$servers}', profile: '$server' },
      { names: 'pdu-{a,b}', profile: 'generic/pdu-vertical', zeroU: true },
    ],
    layout: { front: [{ slots: 'leaf-{01..02}', group: 'leaf switches' }, { slots: 'mgmt-switch' }, { slots: 'server-{01..$servers}' }], back: ['frontend', 'backend-rails', 'oob', 'pdu'] },
    domains: [],
    power: { capacityW, pdus: 2 },
    isNot: ['Rail-optimised backend NICs are cabled by the fabric template; no optic or DAC is modelled.'],
  });
  formats['generic/hgx-8gpu'] = serverRack('HGX 8-GPU servers', 'HGX', 'Generic', 'oem/dell/xe9680-h100', ['oem/dell/xe9680-h100'], 6, 6, 'generic');
  formats['generic/oam-8gpu'] = serverRack('AMD Instinct OAM-8 servers', 'OAM', 'Generic', 'amd/instinct-mi355x-platform', ['amd/instinct-mi355x-platform'], 8, 4, 'generic', 60000);
  formats['generic/gaudi3-8'] = serverRack('Intel Gaudi 3 servers', 'GDI', 'Generic', 'intel/gaudi3-hls', ['intel/gaudi3-hls'], 8, 4, 'generic', 50000);
  formats['generic/cpu-rack'] = {
    ...serverRack('CPU / control-plane rack', 'CPU', 'Generic', 'oem/dell/r760', ['oem/dell/r760'], 2, 16, 'generic'),
    params: { server: { default: 'oem/dell/r760', allowed: ['oem/dell/r760'], label: 'Server profile' }, servers: { default: 12, range: [1, 16], label: 'Servers' } },
    power: { capacityW: 22000, pdus: 2 },
  };
  formats['generic/network-rack'] = {
    pack: 'generic', title: 'Network rack', tag: 'NET', vendor: 'Generic', archetype: 'network', height: 42, cooling: 'air', busbar: false,
    params: { spines: { default: 2, range: [2, 4], label: 'Spine switches' }, ibRails: { default: 4, range: [0, 8], label: 'InfiniBand rail leaves' }, roceRails: { default: 8, range: [0, 8], label: 'RoCE rail leaves' }, ufm: { default: 'nvidia/ufm-appliance', allowed: ['nvidia/ufm-appliance'], label: 'Fabric manager appliance' } },
    slots: [
      { names: 'spine-{01..$spines}', profile: 'generic/spine-switch-800g' },
      { names: 'oob-agg-01', profile: 'generic/oob-agg-switch' },
      { names: 'ufm-01', profile: '$ufm' },
      { names: 'rail-leaf-{01..$ibRails}', profile: 'generic/rail-leaf-ib' },
      { names: 'roce-leaf-{01..$roceRails}', profile: 'generic/rail-leaf-roce' },
      { names: 'pdu-{a,b}', profile: 'generic/pdu-vertical', zeroU: true },
    ],
    layout: { front: [{ slots: 'spine-{01..$spines}', group: 'spines' }, { slots: 'oob-agg-01' }, { slots: 'ufm-01' }, { slots: 'rail-leaf-{01..$ibRails}', group: 'InfiniBand rail leaves' }, { slots: 'roce-leaf-{01..$roceRails}', group: 'RoCE rail leaves' }], back: ['frontend', 'backend-rails', 'oob', 'pdu'] },
    domains: [], power: { capacityW: 48000, pdus: 2 },
    isNot: ['Leaf switches for NVL racks are generated here by the fabric template because the NVL rack has no free U.'],
  };

  // --------------------------------------------------------------- dialects
  // Identity strings matter: lifecycle managers choose code paths from them (05 — Dialects and quirks).
  // Identity strings as NICo's bmc-explorer matches them (exact): GB hosts are Product 'GB BMC' or
  // 'GB200 NVL', GB300 is told apart by a chassis Model 'NVIDIA GB300'; NVSwitch trays are 'P3809';
  // power shelves are Liteon/Delta types. See docs/stacks/nvidia-dsx-os.md — Dialects NICo recognises.
  dialects['nvidia/gb-bmc'] = { root: { Vendor: 'NVIDIA', Product: 'GB BMC', Oem: { Nvidia: {} } }, chassisModel: 'NVIDIA GB300', systemId: 'System_0', chassisId: 'Chassis_0', managerId: 'BMC_0', quirks: ['power-state-lag: PowerState reads On for ~3 s after ForceOff completes'] };
  dialects['nvidia/nvswitch-tray'] = { root: { Vendor: 'NVIDIA', Product: 'P3809', Oem: { Nvidia: {} } }, systemId: 'System_0', chassisId: 'Chassis_0', managerId: 'BMC_0', quirks: [] };
  dialects['liteon/power-shelf'] = { root: { Vendor: 'LITE-ON TECHNOLOGY CORP.', Product: 'PF-1333-7R', Oem: {} }, systemId: '1', chassisId: '1', managerId: '1', quirks: [] };
  dialects['oem/dell-idrac'] = { root: { Vendor: 'Dell', Product: 'Integrated Dell Remote Access Controller', Oem: { Dell: {} } }, systemId: 'System.Embedded.1', chassisId: 'System.Embedded.1', managerId: 'iDRAC.Embedded.1', quirks: ['job-queue: SimpleUpdate returns a Job, not a Task', 'session-limit: 16 sessions, then 503'] };
  dialects['oem/supermicro'] = { root: { Vendor: 'Supermicro', Product: 'Redfish Service', Oem: { Supermicro: {} } }, systemId: '1', chassisId: '1', managerId: '1', quirks: ['slow-inventory: FirmwareInventory takes 2–4 s'] };
  dialects['generic/openbmc'] = { root: { Vendor: 'OpenBMC', Product: 'OpenBMC', Oem: {} }, systemId: 'system', chassisId: 'chassis', managerId: 'bmc', quirks: [] };

  // ------------------------------------------------------- fault catalogues
  // Each fault is defined ONCE as effects on the model (08). `apply`/`clear` mutate the model through the
  // engine's helpers; `effects` is the human preview shown before injecting (12 — Faults).
  const F = (f) => faults.push(f);
  F({ id: 'nvidia/xid/xid-79', pack: 'nvidia/xid', title: 'GPU has fallen off the bus (Xid 79)', target: { component: 'accelerator', vendor: 'nvidia' },
    effects: ['health → Critical, state → offline', 'power → 0 W', 'the GPU\'s NVLink lanes → down', 'kernel log: NVRM: Xid (PCI:{pci}): 79, GPU has fallen off the bus', 'shim nvidia/mokka: fallen-off-bus', 'Redfish LogEntry (Critical) on the device'],
    clears: 'AC cycle, or by hand', clearsOn: ['ac-cycle'],
    apply(E, d, c) { c.health = 'Critical'; c.state = 'offline'; c.lanesUp = 0; E.kernelLog(d, `NVRM: Xid (PCI:${c.pci}): 79, pid=0, name=, GPU has fallen off the bus.`); E.shim(d, c, 'fallen-off-bus'); E.redfishLog(d, 'Critical', `${c.name}: GPU is not responding (Xid 79)`); },
    clear(E, d, c) { c.health = 'OK'; c.state = 'online'; c.lanesUp = c.lanes; E.redfishLog(d, 'OK', `${c.name}: GPU recovered`); } });
  F({ id: 'nvidia/xid/xid-48', pack: 'nvidia/xid', title: 'Double-bit ECC error (Xid 48)', target: { component: 'accelerator', vendor: 'nvidia' },
    effects: ['health → Critical', 'counter ecc.volatile.dbe +1', 'kernel log: NVRM: Xid (PCI:{pci}): 48, …', 'shim nvidia/mokka: xid 48'], clears: 'GPU reset, or by hand', clearsOn: ['reset'],
    apply(E, d, c) { c.health = 'Critical'; c.counters.dbe = (c.counters.dbe || 0) + 1; E.kernelLog(d, `NVRM: Xid (PCI:${c.pci}): 48, pid=0, An uncorrectable double bit error (DBE) has been detected on GPU in the framebuffer.`); E.shim(d, c, 'xid-48'); E.redfishLog(d, 'Critical', `${c.name}: uncorrectable ECC error`); },
    clear(E, d, c) { c.health = 'OK'; E.redfishLog(d, 'OK', `${c.name}: ECC error cleared by reset`); } });
  F({ id: 'nvidia/xid/xid-63', pack: 'nvidia/xid', title: 'Row remapping pending (Xid 63)', target: { component: 'accelerator', vendor: 'nvidia' },
    effects: ['health → Warning', 'counter remap.pending = 1', 'kernel log: NVRM: Xid (PCI:{pci}): 63, …'], clears: 'GPU reset, or by hand', clearsOn: ['reset'],
    apply(E, d, c) { c.health = 'Warning'; c.counters.remapPending = 1; E.kernelLog(d, `NVRM: Xid (PCI:${c.pci}): 63, pid=0, Row Remapper: New row marked for remapping, reset gpu to activate.`); E.shim(d, c, 'xid-63'); },
    clear(E, d, c) { c.health = 'OK'; c.counters.remapPending = 0; } });
  F({ id: 'nvidia/nvlink/switch-tray-failure', pack: 'nvidia/nvlink', title: 'NVLink switch tray failure', target: { device: 'switch-tray' },
    effects: ['device health → Critical', 'all 144 NVLink lanes → down', 'NVLink domain → degraded; NMX-C reports the tray lost', 'Redfish LogEntry (Critical)'], clears: 'by hand (replace tray)', clearsOn: [],
    apply(E, d) { d.health = 'Critical'; d.nvlinkUp = 0; d.failed = true; E.redfishLog(d, 'Critical', 'NVSwitch fabric: both ASICs unresponsive'); },
    clear(E, d) { d.failed = false; d.health = 'OK'; d.nvlinkUp = d.nvlinkLanes; E.redfishLog(d, 'OK', 'NVSwitch fabric restored'); } });
  F({ id: 'nvidia/dpu/agent-unresponsive', pack: 'nvidia/dpu', title: 'DPU agent unresponsive', target: { component: 'dpu' },
    effects: ['component health → Warning', 'DPU agent heartbeat stops (stack side)', 'frontend ports stay up'], clears: 'by hand', clearsOn: ['reset'],
    apply(E, d, c) { c.health = 'Warning'; c.state = 'agent-down'; E.redfishLog(d, 'Warning', `${c.name}: DPU OS heartbeat lost`); },
    clear(E, d, c) { c.health = 'OK'; c.state = 'online'; } });
  F({ id: 'amd/ras/uncorrectable-ecc', pack: 'amd/ras', title: 'Uncorrectable ECC (RAS UE)', target: { component: 'accelerator', vendor: 'amd' },
    effects: ['health → Critical', 'kernel log: amdgpu: [RAS] uncorrectable error …', 'shim amd/smi: ue'], clears: 'GPU reset, or by hand', clearsOn: ['reset'],
    apply(E, d, c) { c.health = 'Critical'; c.counters.ue = (c.counters.ue || 0) + 1; E.kernelLog(d, `amdgpu ${c.pci}: [RAS] uncorrectable hardware error (UE) detected in umc block`); E.redfishLog(d, 'Critical', `${c.name}: uncorrectable ECC`); },
    clear(E, d, c) { c.health = 'OK'; } });
  F({ id: 'intel/gaudi/device-error', pack: 'intel/gaudi', title: 'Gaudi device error (hard reset required)', target: { component: 'accelerator', vendor: 'intel' },
    effects: ['health → Critical', 'kernel log: habanalabs hlN: … hard reset required', 'shim intel/hlml: device-error'], clears: 'device reset, or by hand', clearsOn: ['reset'],
    apply(E, d, c) { c.health = 'Critical'; E.kernelLog(d, `habanalabs ${c.pci}: ${c.name}: device error detected, hard reset required`); E.redfishLog(d, 'Critical', `${c.name}: device error`); },
    clear(E, d, c) { c.health = 'OK'; } });
  F({ id: 'generic/bmc/unreachable', pack: 'generic/bmc', title: 'BMC unreachable', target: { deviceWith: 'bmc' },
    effects: ['Redfish and IPMI requests time out', 'the DHCP lease is not renewed', 'SOL console drops'], clears: 'by hand', clearsOn: [],
    apply(E, d) { d.bmc.reachable = false; }, clear(E, d) { d.bmc.reachable = true; } });
  F({ id: 'generic/bmc/slow', pack: 'generic/bmc', title: 'BMC slow (3–8 s, some 503)', target: { deviceWith: 'bmc' },
    effects: ['Redfish latency 3–8 s', 'about 1 in 8 requests → 503 ServiceUnavailable'], clears: 'by hand', clearsOn: ['reset'],
    apply(E, d) { d.bmc.slow = true; }, clear(E, d) { d.bmc.slow = false; } });
  F({ id: 'generic/power/psu-failure', pack: 'generic/power', title: 'PSU failure', target: { component: 'psu' },
    effects: ['PSU health → Critical, output 0', 'device health → Warning (redundancy lost); two failed PSUs → hard power loss', 'Redfish LogEntry'], clears: 'by hand (replace PSU)', clearsOn: [],
    apply(E, d, c) { c.health = 'Critical'; c.state = 'failed'; E.redfishLog(d, 'Critical', `${c.name}: input lost / output failed`); E.checkPowerLoss(d); },
    clear(E, d, c) { c.health = 'OK'; c.state = 'online'; E.redfishLog(d, 'OK', `${c.name}: restored`); } });
  F({ id: 'generic/power/shelf-output-loss', pack: 'generic/power', title: 'Power shelf output loss', target: { device: 'power-shelf' },
    effects: ['shelf output → 0 W, health → Critical', 'rack busbar capacity reduced; below the minimum shelf count every tray loses power (hard off)', 'rack power on PDUs/BMS drops'], clears: 'by hand', clearsOn: [],
    apply(E, d) { d.health = 'Critical'; d.outputLost = true; E.checkBusbar(d.rack); }, clear(E, d) { d.health = 'OK'; d.outputLost = false; } });
  F({ id: 'generic/thermal/fan-failure', pack: 'generic/thermal', title: 'Fan failure', target: { component: 'fan' },
    effects: ['fan health → Critical', 'exhaust and accelerator temperatures rise ~8 °C', 'device health → Warning'], clears: 'by hand', clearsOn: [],
    apply(E, d, c) { c.health = 'Critical'; c.state = 'stopped'; d.thermalOffset = (d.thermalOffset || 0) + 8; E.redfishLog(d, 'Warning', `${c.name}: fan speed 0 RPM`); },
    clear(E, d, c) { c.health = 'OK'; c.state = 'online'; d.thermalOffset = Math.max(0, (d.thermalOffset || 0) - 8); } });
  F({ id: 'generic/thermal/coolant-excursion', pack: 'generic/thermal', title: 'Coolant supply temperature excursion (+12 °C)', target: { device: 'cdu' },
    effects: ['CDU supply temperature target +12 °C', 'every rack on the loop: coolant-in rises; accelerator temperatures follow', 'BMS CDU points change; trays reach Warning thresholds'], clears: 'by hand', clearsOn: [],
    apply(E, d) { d.supplyOffset = 12; d.health = 'Warning'; }, clear(E, d) { d.supplyOffset = 0; d.health = 'OK'; } });
  F({ id: 'generic/leak/tray-level', pack: 'generic/leak', title: 'Leak detected (tray level)', target: { device: 'sensor' },
    effects: ['sensor leak → true', 'BMS point BMS/v1/PUB/Value/Sensor/Leak/… → true', 'the rack format\'s propagation: nearby trays report coolant warning'], clears: 'by hand (leak stopped)', clearsOn: [],
    apply(E, d) { d.leak = true; d.health = 'Critical'; E.bms(d, true); }, clear(E, d) { d.leak = false; d.health = 'OK'; E.bms(d, false); } });
  F({ id: 'generic/firmware/drift', pack: 'generic/firmware', title: 'Firmware drift from baseline (UEFI)', target: { deviceWith: 'firmware' },
    effects: ['UEFI version → an older release, status out-of-baseline', 'FirmwareInventory reports the drifted version', 'the stack\'s baseline check should quarantine and update'], clears: 'a firmware update applied on reset, or by hand', clearsOn: ['firmware-update'],
    apply(E, d) { const f = d.firmware.find((x) => x.component === 'uefi') || d.firmware[0]; f.version = f.baseline.replace(/(\d+)(?!.*\d)/, (m) => String(Math.max(0, parseInt(m, 10) - 2)).padStart(m.length, '0')); f.status = 'out-of-baseline'; },
    clear(E, d) { for (const f of d.firmware) { f.version = f.baseline; f.status = 'in-baseline'; } } });
  F({ id: 'generic/tpm/pcr-change', pack: 'generic/tpm', title: 'Measured-boot PCR change (boot chain tampered)', target: { component: 'tpm' },
    effects: ['PCR 0/7 values change on the next boot', 'attestation against the recorded policy fails', 'the stack should refuse the host'], clears: 'by hand', clearsOn: [],
    apply(E, d, c) { c.health = 'Warning'; c.state = 'pcr-mismatch'; d.attestation = 'fail'; }, clear(E, d, c) { c.health = 'OK'; c.state = 'online'; d.attestation = 'ok'; } });
  F({ id: 'generic/link/cable-pulled', pack: 'generic/link', title: 'Cable pulled', target: { port: true },
    effects: ['both ends of the link → down', 'LLDP neighbour lost on the switch', 'the NOS reports the port down; the host\'s NIC reports no carrier'], clears: 'cable replaced', clearsOn: [],
    apply(E, d, c, link) { E.setLink(link, 'down'); }, clear(E, d, c, link) { E.setLink(link, 'up'); } });

  const faultById = Object.fromEntries(faults.map((f) => [f.id, f]));
  // Which faults may target this (device, component?, port?) — the catalogue filtered to the selection.
  function applicable(dev, comp, port) {
    const profile = profiles[dev.profile] || {};
    const packs = new Set(profile.faultPacks || []);
    return faults.filter((f) => {
      if (port) return !!f.target.port;
      if (comp) { if (!f.target.component) return false; if (f.target.component !== comp.kind) return false; if (f.target.vendor && comp.vendor !== f.target.vendor) return false; return packs.has(f.pack) || f.pack.startsWith('generic'); }
      if (f.target.port || f.target.component) return false;
      if (f.target.device) return dev.class === f.target.device && (packs.has(f.pack) || dev.class === 'sensor' || dev.class === 'cdu');
      if (f.target.deviceWith === 'bmc') return !!dev.bmc && packs.has(f.pack);
      if (f.target.deviceWith === 'firmware') return dev.firmware.length > 0 && packs.has(f.pack);
      return false;
    });
  }

  const packList = [
    { name: 'generic@0.1', contents: 'generic server, switches, PDU, CDU, leak sensor; hgx-8gpu, oam-8gpu, gaudi3-8, cpu-rack, network-rack; clos-2tier, rail-optimised, oob-flat; generic faults' },
    { name: 'nvidia@0.1', contents: 'GB300 compute tray, NVLink switch tray, power shelf, UFM appliance; gb300-nvl72, gb200-nvl72, gb200-nvl36x2; nvlink-spine; nvidia/gb-bmc dialect; xid, nvlink, dpu faults; mokka in-host driver' },
    { name: 'oem/dell@0.1', contents: 'XE9680, R760; iDRAC dialect' },
    { name: 'amd@0.1', contents: 'Instinct MI355X platform; amd/ras faults; AMD SMI shim (Q7) — after DSX OS (10 — M8)' },
    { name: 'intel@0.1', contents: 'Gaudi 3 HLS; intel/gaudi faults; HLML shim (Q7) — after DSX OS (10 — M9)' },
    { name: 'oem/supermicro@0.1', contents: 'Supermicro Redfish dialect (used by the AMD and Intel platforms)' },
  ];

  return { profiles, formats, dialects, faults, faultById, applicable, packList };
})();
