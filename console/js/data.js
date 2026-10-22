// data.js — shared constants and helpers: severities and their colours, rack formats, event
// kinds, the demo's fault catalogue and "is not" lists, and small formatting helpers.
window.TC = window.TC || {};
TC.data = (() => {
  const SEVN = { OK: 0, Warning: 1, Critical: 2, Unknown: 0 };
  const SEVC = { OK: '#757575', Warning: '#d1b854', Critical: '#ec7575', Unknown: '#757575' };
  const SEVM = { OK: '', Warning: '◆', Critical: '▲', Unknown: '' };

  // Rack formats as the console draws them. `cap` is the demo's rack capacity in watts; a live
  // twin reports its own capacity per rack (GET /v1/facility).
  const FMT = {
    nvl: { tag: 'NVL72', title: 'nvidia/gb300-nvl72', cap: 132000, cooling: 'liquid' },
    hgx: { tag: 'HGX H100', title: 'generic/hgx-8gpu · oem/dell/xe9680-h100', cap: 60000, cooling: 'air' },
    oam: { tag: 'MI355X', title: 'generic/oam-8gpu · amd/mi355x', cap: 60000, cooling: 'air' },
    gaudi: { tag: 'Gaudi 3', title: 'generic/gaudi3-8 · intel/gaudi3', cap: 50000, cooling: 'air' },
    cpu: { tag: 'CPU', title: 'generic/cpu-rack', cap: 20000, cooling: 'air' },
    net: { tag: 'NET', title: 'generic/network-rack', cap: 24000, cooling: 'air' },
  };
  const FORMAT_IDS = { 'nvidia/gb300-nvl72': 'nvl', 'generic/hgx-8gpu': 'hgx', 'generic/oam-8gpu': 'oam', 'generic/cpu-rack': 'cpu' };

  // A live format name to the console's drawing key and its short tag.
  function formatKey(name, archetype) {
    const n = String(name || '');
    if (archetype === 'rack-scale' || /nvl/.test(n)) return 'nvl';
    if (/oam|mi3/.test(n)) return 'oam';
    if (/gaudi/.test(n)) return 'gaudi';
    if (archetype === 'network' || /network/.test(n)) return 'net';
    if (archetype === 'cpu' || /cpu/.test(n)) return 'cpu';
    return 'hgx';
  }
  function formatTag(name, key) {
    const last = String(name || '').split('/').pop();
    const m = /nvl(\d+(x\d+)?)/i.exec(last);
    if (m) return 'NVL' + m[1];
    return { hgx: 'HGX', oam: 'OAM', gaudi: 'Gaudi 3', cpu: 'CPU', net: 'NET' }[key] || last;
  }

  const KIND_C = {
    fault: ['#ec7575', '#861a1a44'], state: ['#b3b3b3', '#1f1f1f'], request: ['#5b9dff', '#5b9dff1f'], stack: ['#b48ae8', '#965cde26'],
    kernel: ['#cfcfcf', '#212225'], bms: ['#d4a853', '#423a1a66'], nos: ['#5b9dff', '#5b9dff14'], act: ['#d4a853', '#423a1a66'],
    twin: ['#757575', '#161616'], read: ['#757575', '#161616'], behaviour: ['#b3b3b3', '#1f1f1f'],
  };

  const pad = (n) => String(n).padStart(2, '0');
  const hash = (s) => { let h = 2166136261; for (const c of String(s)) h = Math.imul(h ^ c.charCodeAt(0), 16777619); return h >>> 0; };
  const kw = (w) => (w / 1000).toFixed(1) + ' kW';
  const fmtClock = (d) => `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`;

  // The demo's fault catalogue (illustrative; a live twin publishes its own, GET /v1/catalogue).
  const FAULTS = [
    { id: 'nvidia/xid/xid-79', title: 'GPU has fallen off the bus (Xid 79)', on: ['compute-tray', 'gpu-server'], comp: true, sev: 'Critical',
      effects: ['health → Critical, state → offline', 'gpu power → 0 W', 'NVLink ports (scale-up) → down', 'kernel log "NVRM: Xid (PCI:…): 79"', 'shim: fallen-off-bus (vm only)', 'Redfish LogEntry (Critical)'], clears: 'on AC cycle, or manually' },
    { id: 'nvidia/xid/xid-48', title: 'Double-bit ECC error (Xid 48)', on: ['compute-tray', 'gpu-server'], comp: true, sev: 'Critical',
      effects: ['health → Critical', 'counter ecc.volatile.dbe +1', 'kernel log "NVRM: Xid (PCI:…): 48"'], clears: 'on device reset, or manually' },
    { id: 'generic/leak/tray-detector', title: 'In-tray leak detector trips', on: ['compute-tray'], sev: 'Critical',
      effects: ['LeakDetectors/*/DetectorState → Leak', 'health → Critical', 'Redfish LogEntry (Critical)'], clears: 'manually' },
    { id: 'generic/bmc/unreachable', title: 'BMC unreachable', on: ['compute-tray', 'gpu-server', 'cpu-server', 'switch-tray', 'power-shelf', 'switch'], sev: 'Warning',
      effects: ['Redfish, IPMI → no response', 'DHCP lease held', 'host keeps running'], clears: 'on BMC reset, or manually' },
    { id: 'generic/bmc/slow', title: 'BMC slow (latency, 5xx)', on: ['compute-tray', 'gpu-server', 'cpu-server', 'switch-tray', 'power-shelf'], sev: 'Warning',
      effects: ['Redfish latency → 4–9 s', '1 in 5 requests → 503'], clears: 'manually' },
    { id: 'generic/power/psu-failure', title: 'PSU failure', on: ['power-shelf', 'gpu-server', 'cpu-server'], sev: 'Warning',
      effects: ['health → Warning', 'one PSU → 0 W output', 'power.loss: evaluate redundancy (N+1 holds)'], clears: 'on replace, or manually' },
    { id: 'generic/thermal/fan-failure', title: 'Fan failure', on: ['gpu-server', 'cpu-server', 'switch'], sev: 'Warning',
      effects: ['health → Warning', 'exhaust +8 °C, rising', 'throttle at 85 °C'], clears: 'on replace, or manually' },
    { id: 'generic/link/cable-pulled', title: 'Cable pulled (uplink)', on: ['switch', 'switch-tray'], sev: 'Warning',
      effects: ['link → down (both ends)', 'LLDP neighbour lost', 'BGP session drops (NOS)'], clears: 'on replace' },
    { id: 'generic/firmware/drift', title: 'Firmware version drift', on: ['compute-tray', 'gpu-server', 'cpu-server', 'switch-tray', 'switch', 'power-shelf'], sev: 'Warning',
      effects: ['BMC firmware ≠ baseline', 'Redfish FirmwareInventory updated'], clears: 'on update, or manually' },
  ];
  const RACK_FAULTS = [
    { id: 'generic/leak/rack-point', title: 'Rack leak point (BMS)', on: ['nvl'], sev: 'Warning', effects: ['leak-sensor-02 → Leak', 'BMS RackLeakDetect → true', 'propagation: none (rack-level)'], clears: 'manually' },
    { id: 'generic/power/pdu-outlet-off', title: 'PDU outlet off (feed A)', on: ['hgx', 'oam', 'gaudi', 'cpu', 'net'], sev: 'Warning', effects: ['feed A to rack → 0 W', 'power.loss: devices with minSources 1 stay up', 'PSU redundancy lost'], clears: 'manually' },
  ];
  const ISNOT = {
    'compute-tray': ['Thermal behaviour is first-order: coolant-in follows the CDU supply plus a load term; no flow modelling inside the cold plate.', 'A real Xid 79 also leaves PCIe link-state artefacts the host firmware reports; this one does not.', 'Redfish tree captured from one firmware version (GB300 BMC 25.06); other versions differ in OEM paths.'],
    'gpu-server': ['GPU temperatures do not respond to workload placement; load is a seeded constant.', 'iDRAC dialect from a capture; lifecycle-controller jobs are stubbed.'],
    default: ['Power draw is a seeded constant per profile, not measured.', 'Firmware update paths are accepted but not executed.'],
  };

  // The cabled fabrics as the floor draws them: a colour and a short name each, by what the
  // fabric is (management, frontend, InfiniBand, RoCE), and a stable order. Colours stay clear of
  // power (violet), cooling (blue supply, amber return) and the severities.
  const FABRIC_KINDS = [
    [/oob|mgmt|management/i, 'oob', '#9aa5b8'],
    [/front/i, 'frontend', '#38c6e0'],
    [/(^|[-_.])ib($|[-_.])|infiniband/i, 'ib', '#f08bc4'],
    [/roce|ethernet/i, 'roce', '#b5e36b'],
  ];
  const FABRIC_SPARE = ['#e0a45b', '#c39bef', '#6fd3c1', '#e86f9a'];
  function fabricStyle(name, i) {
    const k = FABRIC_KINDS.findIndex(([re]) => re.test(name));
    return k >= 0 ? { short: FABRIC_KINDS[k][1], color: FABRIC_KINDS[k][2], order: k } : { short: name, color: FABRIC_SPARE[i % FABRIC_SPARE.length], order: 10 + i };
  }
  function styleFabrics(list) {
    return list.map((f, i) => ({ ...f, ...fabricStyle(f.name, i) })).sort((a, b) => a.order - b.order || a.name.localeCompare(b.name));
  }

  return { fabricStyle, styleFabrics, SEVN, SEVC, SEVM, FMT, FORMAT_IDS, formatKey, formatTag, KIND_C, pad, hash, kw, fmtClock, FAULTS, RACK_FAULTS, ISNOT };
})();
