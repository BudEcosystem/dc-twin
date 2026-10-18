// estate.js — the estate file (the only input, D1) and the compiler that turns it, with the packs, into
// the runtime model: devices, components, ports, links, domains, facility (spec 03).
window.DCT = window.DCT || {};
DCT.estate = (() => {
  const U = DCT.util, P = DCT.packs;

  // The estate file, as the YAML in examples/ would parse. Row positions may be empty.
  const file = {
    apiVersion: 'dc-twin/v1alpha1', kind: 'Estate', metadata: { name: 'hall-a-demo' },
    spec: {
      seed: 1729, clock: 'wall',
      packs: ['generic@0.1', 'nvidia@0.1', 'oem/dell@0.1', 'amd@0.1', 'intel@0.1', 'oem/supermicro@0.1'],
      fidelity: { default: 'model', overrides: [{ select: 'site-1/hall-a/a03/compute-tray-{01,02}', level: 'vm' }] },
      sites: [{ name: 'site-1', halls: [{ name: 'hall-a', rows: [
        { name: 'a', positions: 10, racks: [
          { name: 'a01', position: 1, format: 'nvidia/gb300-nvl72' },
          { name: 'a02', position: 2, format: 'nvidia/gb300-nvl72' },
          { name: 'a03', position: 3, format: 'nvidia/gb300-nvl72', params: { shelves: 8 } },
          { name: 'a04', position: 4, format: 'nvidia/gb300-nvl72' },
          { name: 'a05', position: 5, format: 'generic/hgx-8gpu', params: { server: 'oem/dell/xe9680-h100', servers: 4 } },
          { name: 'a06', position: 6, format: 'generic/hgx-8gpu', params: { server: 'oem/dell/xe9680-h100', servers: 4 } },
          { name: 'a10', position: 10, format: 'generic/network-rack' },
        ] },
        { name: 'b', positions: 8, racks: [
          { name: 'b01', position: 1, format: 'generic/hgx-8gpu', params: { servers: 4 } },
          { name: 'b02', position: 2, format: 'generic/oam-8gpu', params: { servers: 4 } },
          { name: 'b03', position: 3, format: 'generic/gaudi3-8', params: { servers: 4 } },
        ] },
        { name: 'c', positions: 4, racks: [
          { name: 'c01', position: 1, format: 'generic/cpu-rack', params: { servers: 12 } },
        ] },
      ] }] }],
      networks: {
        oob: { role: 'management', cidr: '10.10.0.0/22', dhcp: 'stack' },
        provisioning: { role: 'provisioning', cidr: '10.20.0.0/22', dhcp: 'stack' },
        frontend: { role: 'frontend', fabric: 'frontend' },
        backend: { role: 'scale-out', fabric: 'backend' },
      },
      fabrics: [
        { name: 'frontend', template: 'clos-2tier', nos: 'sonic-vs', params: { leafsPerRack: 2, spines: 2, underlay: 'bgp-unnumbered', overlay: 'evpn' } },
        { name: 'oob', template: 'oob-flat', nos: 'sonic-vs' },
        { name: 'backend', template: 'rail-optimised', technology: 'infiniband', manager: 'nvidia/ufm', params: { rails: 4 }, select: 'hall-a/a0[1-4]' },
        { name: 'backend-roce', template: 'rail-optimised', technology: 'ethernet-roce', nos: 'sonic-vs', params: { rails: 8 }, select: 'hall-a/{a05,a06,b0*}' },
      ],
      facility: {
        power: { feeds: [{ name: 'A', capacityW: 900000 }, { name: 'B', capacityW: 900000 }] },
        cooling: { loops: [{ name: 'cdu-1', profile: 'generic/cdu-inrow', racks: ['a01', 'a02', 'a03', 'a04'], capacityKW: 600 }] },
        bms: { dialect: 'nvidia/dsx-exchange' }, grid: { dialect: 'nvidia/dsx-flex' },
      },
      stacks: [{ name: 'nvidia-dsx-os', components: ['nico', 'nvsentinel', 'dsx-exchange', 'fleet-intelligence'], options: { dpuMode: false, scoutImpersonated: true } }],
    },
  };

  const SITE = 'site-1', HALL = 'hall-a', HALLPATH = `${SITE}/${HALL}`;

  function subst(s, params) { return String(s).replace(/\$(\w+)/g, (_, k) => params[k]); }
  function paramsFor(format, given) {
    const out = {};
    for (const [k, v] of Object.entries(format.params || {})) out[k] = given && given[k] !== undefined ? given[k] : v.default;
    return out;
  }

  function fidelityFor(path, spec) {
    for (const o of spec.fidelity.overrides || []) if (U.matchSelector(o.select, path)) return o.level;
    return spec.fidelity.default;
  }

  // ---- devices -------------------------------------------------------------
  function makeDevice(model, path, name, rackPath, profileId, slot, seed) {
    const pr = P.profiles[profileId];
    if (!pr) throw new Error('unknown profile ' + profileId);
    const r = U.rng(`${seed}:${path}`);
    const d = {
      path, name, rack: rackPath, profile: profileId, class: pr.class, vendor: pr.vendor, title: pr.title,
      fidelity: fidelityFor(path, model.estate.spec), slot, units: pr.units,
      identity: { serial: (pr.vendor === 'Dell' ? '' : '') + U.pick(r, ['1', '7', 'S', 'M']) + U.digits(r, 11), uuid: U.uuid(r) },
      power: { state: 'On', watts: 0, load: 0.45 + r() * 0.3, transition: null },
      health: 'OK', faults: [], components: [], ports: [], sensors: [], firmware: [], logs: [], requests: [], console: [], kmsg: [],
      bmc: pr.dialect ? { reachable: true, slow: false, dialect: pr.dialect, mac: U.mac(r, '3c:ec:ef'), lease: null, failures: 0 } : null,
      attestation: 'ok', thermalOffset: 0,
    };
    if (pr.class === 'switch-tray') { d.nvlinkLanes = pr.nvlinkLanes; d.nvlinkUp = pr.nvlinkLanes; }
    if (pr.class === 'cdu') { d.supplyOffset = 0; }
    if (pr.class === 'sensor') { d.leak = false; d.power.state = 'On'; }
    let pciSlot = 0x17;
    for (const c of pr.components) {
      for (let i = 0; i < c.count; i++) {
        const comp = { path: `${path}/${c.prefix}-${i}`, name: `${c.prefix}-${i}`, kind: c.kind, model: c.model, vendor: c.vendor, health: 'OK', state: 'online', counters: {} };
        if (c.kind === 'accelerator') { comp.pci = `0000:${(pciSlot += 0x10).toString(16).padStart(2, '0')}:00.0`; comp.lanes = c.nvlinkLanes || 0; comp.lanesUp = comp.lanes; comp.maxW = c.maxW; comp.temp = 0; }
        d.components.push(comp);
      }
    }
    for (const p of pr.ports) for (const name of U.expandBraces(p.name)) d.ports.push({ name, kind: p.kind, speed: p.speed, network: p.network, state: 'up', peer: null, link: null, note: p.note });
    for (const [c, v] of pr.firmware) d.firmware.push({ component: c, version: v, baseline: v, status: 'in-baseline' });
    model.devices[path] = d; model.order.push(path);
    return d;
  }

  function sensorsFor(d) {
    const s = [];
    const add = (name, quantity, unit, warn, crit) => s.push({ name, quantity, unit, value: null, warn, crit });
    switch (d.class) {
      case 'compute-tray': add('coolant-in', 'temperature', '°C', 45, 55); add('gpu-max', 'temperature', '°C', 85, 95); add('cpu-max', 'temperature', '°C', 90, 100); add('power', 'power', 'W', null, null); break;
      case 'switch-tray': add('coolant-in', 'temperature', '°C', 45, 55); add('asic-max', 'temperature', '°C', 90, 100); add('power', 'power', 'W', null, null); break;
      case 'server': add('inlet', 'temperature', '°C', 35, 40); add('exhaust', 'temperature', '°C', 60, 70); if (d.components.some((c) => c.kind === 'accelerator')) add('gpu-max', 'temperature', '°C', 85, 92); add('power', 'power', 'W', null, null); break;
      case 'power-shelf': add('output-power', 'power', 'W', null, null); add('psu-max', 'temperature', '°C', 70, 85); break;
      case 'switch': case 'appliance': add('inlet', 'temperature', '°C', 40, 45); add('power', 'power', 'W', null, null); break;
      case 'pdu': add('load', 'current', 'A', 50, 58); add('power', 'power', 'W', null, null); break;
      case 'cdu': add('supply', 'temperature', '°C', 38, 45); add('return', 'temperature', '°C', 50, 60); add('flow', 'flow', 'L/min', null, null); add('pressure', 'pressure', 'bar', null, null); add('power', 'power', 'W', null, null); break;
      case 'sensor': add('leak', 'leak', '', null, null); break;
    }
    d.sensors = s;
  }

  // ---- racks -------------------------------------------------------------------
  function compileRack(model, rowName, spec) {
    const fmt = P.formats[spec.format];
    if (!fmt) throw new Error('unknown format ' + spec.format);
    const params = paramsFor(fmt, spec.params);
    const rackPath = `${HALLPATH}/${spec.name}`;
    const rack = { path: rackPath, name: spec.name, row: rowName, position: spec.position, format: spec.format, params, height: fmt.height, tag: fmt.tag, title: fmt.title, cooling: fmt.cooling, busbar: fmt.busbar, devices: [], slots: [], zeroU: [], sensors: [], domains: [], capacityW: fmt.power.capacityW, feeds: ['A', 'B'], loop: null, isNot: fmt.isNot, archetype: fmt.archetype };
    model.racks[rackPath] = rack;
    // slot specs → name → profile
    const slotSpecs = {};
    for (const s of fmt.slots) {
      const profile = subst(s.profile, params);
      const names = U.expandBraces(subst(s.names, params));
      names.forEach((n, i) => { slotSpecs[n] = { profile, zeroU: !!s.zeroU, at: s.at ? s.at[i] : null }; });
    }
    // layout → U positions, top to bottom
    let u = 1;
    for (const entry of fmt.layout.front) {
      const names = U.expandBraces(subst(entry.slots, params)).filter((n) => slotSpecs[n]);
      for (const n of names) {
        const ss = slotSpecs[n]; const pr = P.profiles[ss.profile];
        const slot = { name: n, u, units: pr.units, group: entry.group || null };
        const d = makeDevice(model, `${rackPath}/${n}`, n, rackPath, ss.profile, slot, model.estate.spec.seed);
        sensorsFor(d); rack.devices.push(d.path); rack.slots.push(slot);
        u += pr.units;
        delete slotSpecs[n];
      }
    }
    // zero-U and unslotted (PDUs, leak sensors)
    for (const [n, ss] of Object.entries(slotSpecs)) {
      const d = makeDevice(model, `${rackPath}/${n}`, n, rackPath, ss.profile, { name: n, u: null, units: 0, zeroU: ss.zeroU, at: ss.at }, model.estate.spec.seed);
      sensorsFor(d); rack.devices.push(d.path);
      if (ss.zeroU) rack.zeroU.push(d.path); else rack.sensors.push(d.path);
      if (ss.at) { const atDev = model.devices[`${rackPath}/${ss.at}`]; d.slot.u = atDev ? atDev.slot.u : null; d.level = ss.at; }
    }
    for (const dom of fmt.domains || []) {
      rack.domains.push({ kind: dom.kind, path: `${rackPath}/domain/${dom.kind}`, members: `${spec.name}/${dom.members}`, manager: `${rackPath}/${dom.manager}`, formation: dom.formation, spans: dom.spans ? subst(dom.spans, params) : null, state: 'Ready' });
    }
    return rack;
  }

  // ---- fabrics (generated devices and links) -----------------------------------------
  function link(model, a, ap, b, bp, kind) {
    const A = model.devices[a], B = model.devices[b];
    const pa = A.ports.find((p) => p.name === ap), pb = B.ports.find((p) => p.name === bp);
    if (!pa || !pb) return;
    const l = { id: `${a}:${ap}<->${b}:${bp}`, a: { dev: a, port: ap }, b: { dev: b, port: bp }, kind, state: 'up' };
    pa.peer = `${U.lastSeg(b)}:${bp}`; pb.peer = `${U.lastSeg(a)}:${ap}`; pa.link = l.id; pb.link = l.id;
    model.links.push(l); model.linkById[l.id] = l;
  }
  function freePort(model, devPath, prefixRe) {
    const d = model.devices[devPath];
    return d.ports.find((p) => prefixRe.test(p.name) && !p.peer);
  }

  function compileFabrics(model, only) {
    const scope = (rack) => !only || only.includes(rack.path);
    const net = Object.values(model.racks).find((r) => r.format === 'generic/network-rack');
    if (!net) return;
    const netPath = net.path;
    const spines = net.devices.filter((p) => /spine-/.test(p));
    const ibRails = net.devices.filter((p) => /\/rail-leaf-/.test(p));
    const roceRails = net.devices.filter((p) => /\/roce-leaf-/.test(p));
    // oob: every BMC port → the rack's mgmt switch (or oob-agg in the network rack), mgmt switch → oob-agg
    const oobAgg = net.devices.find((p) => /oob-agg/.test(p));
    for (const rack of Object.values(model.racks)) {
      if (!scope(rack)) continue;
      const mgmt = rack.devices.find((p) => /\/mgmt-switch$/.test(p)) || oobAgg;
      for (const dp of rack.devices) {
        const d = model.devices[dp];
        if (dp === mgmt) continue;
        const bmcPort = d.ports.find((p) => p.network === 'oob' && !p.peer);
        if (!bmcPort) continue;
        const sp = freePort(model, mgmt, /^eth/); if (sp) link(model, dp, bmcPort.name, mgmt, sp.name, 'oob');
      }
      if (mgmt !== oobAgg && oobAgg) { const up = freePort(model, mgmt, /^up/); const ap = freePort(model, oobAgg, /^eth/); if (up && ap) link(model, mgmt, up.name, oobAgg, ap.name, 'oob'); }
    }
    // frontend clos-2tier: NVL racks get leaves generated in the network rack (no free U); server racks have in-rack leaves
    for (const rack of Object.values(model.racks)) {
      if (rack.archetype === 'network' || !scope(rack) || rack.leaves) continue;
      let leaves = rack.devices.filter((p) => /\/leaf-0[12]$/.test(p));
      if (leaves.length === 0) {
        leaves = [];
        for (const i of [1, 2]) {
          const name = `leaf-${rack.name}-${i}`;
          const d = makeDevice(model, `${netPath}/${name}`, name, netPath, 'generic/leaf-switch-800g', { name, u: null, units: 1, generated: true, forRack: rack.path }, model.estate.spec.seed);
          sensorsFor(d); net.devices.push(d.path); net.zeroU.push(d.path); leaves.push(d.path);
        }
      }
      rack.leaves = leaves;
      for (const lp of leaves) for (const sp of spines) { const a = freePort(model, lp, /^eth(6[0-4])$/), b = freePort(model, sp, /^eth/); if (a && b) link(model, lp, a.name, sp, b.name, 'frontend-uplink'); }
      // host frontend ports → leaves, alternating; backend → rails
      const isIB = rack.archetype === 'rack-scale';
      const rails = isIB ? ibRails : roceRails;
      for (const dp of rack.devices) {
        const d = model.devices[dp];
        if (!['compute-tray', 'server'].includes(d.class)) continue;
        d.ports.filter((p) => p.network === 'frontend' && !p.peer).forEach((p, i) => { const leaf = leaves[i % leaves.length]; const lp = freePort(model, leaf, /^eth([1-5]?\d)$/); if (lp) link(model, dp, p.name, leaf, lp.name, 'frontend'); });
        d.ports.filter((p) => p.network === 'backend' && !p.peer).forEach((p, i) => { if (!rails.length) return; const rail = rails[i % rails.length]; const rp = freePort(model, rail, /^(ib|eth)/); if (rp) link(model, dp, p.name, rail, rp.name, isIB ? 'backend-ib' : 'backend-roce'); });
      }
    }
    for (const sp of spines) model.devices[sp].slot.generatedFor = 'frontend';
  }

  // ---- facility ----------------------------------------------------------------
  function compileFacility(model) {
    const fac = model.estate.spec.facility;
    model.facility = { feeds: fac.power.feeds.map((f) => ({ ...f, watts: 0 })), loops: [], bms: { dialect: fac.bms.dialect, points: {} }, grid: fac.grid };
    for (const loop of fac.cooling.loops) {
      const path = `${HALLPATH}/facility/${loop.name}`;
      const d = makeDevice(model, path, loop.name, null, loop.profile, { name: loop.name, u: null, units: 0, facility: true }, model.estate.spec.seed);
      sensorsFor(d);
      const L = { name: loop.name, path, racks: loop.racks.map((r) => `${HALLPATH}/${r}`), capacityKW: loop.capacityKW, device: path };
      model.facility.loops.push(L);
      for (const rp of L.racks) if (model.racks[rp]) model.racks[rp].loop = loop.name;
    }
  }

  function compile(estateFile) {
    const model = { estate: estateFile, hall: { path: HALLPATH, name: HALL, site: SITE, rows: [] }, racks: {}, devices: {}, order: [], links: [], linkById: {}, facility: null };
    const hall = estateFile.spec.sites[0].halls[0];
    for (const row of hall.rows) {
      const R = { name: row.name, positions: [] };
      for (let i = 1; i <= row.positions; i++) {
        const spec = row.racks.find((r) => r.position === i);
        R.positions.push({ index: i, rack: spec ? compileRack(model, row.name, spec).path : null });
      }
      model.hall.rows.push(R);
    }
    compileFabrics(model);
    compileFacility(model);
    return model;
  }

  // Design mode applies a draft to a running model (03 — Changing a running estate). Returns the new rack.
  function addRack(model, rowName, position, spec) {
    const row = model.hall.rows.find((r) => r.name === rowName);
    const pos = row.positions.find((p) => p.index === position);
    if (!pos || pos.rack) throw new Error('position is not empty');
    const estRow = model.estate.spec.sites[0].halls[0].rows.find((r) => r.name === rowName);
    estRow.racks.push({ name: spec.name, position, format: spec.format, params: spec.params });
    const rack = compileRack(model, rowName, { ...spec, position });
    pos.rack = rack.path;
    for (const dp of rack.devices) { const d = model.devices[dp]; d.power.state = d.class === 'sensor' ? 'On' : 'Off'; d.power.watts = 0; d.justRacked = true; }
    const net = Object.values(model.racks).find((r) => r.format === 'generic/network-rack');
    if (net) {
      // cable it: reuse the fabric compiler for just this rack by running the per-rack parts
      const before = new Set(model.links.map((l) => l.id));
      compileFabrics(model, [rack.path]);
      rack.newLinks = model.links.filter((l) => !before.has(l.id)).map((l) => l.id);
    }
    const loop = model.facility.loops[0];
    if (rack.cooling === 'liquid' && loop) { loop.racks.push(rack.path); rack.loop = loop.name; }
    return rack;
  }
  function removeRack(model, rackPath) {
    const rack = model.racks[rackPath];
    if (!rack) return;
    for (const dp of [...rack.devices, ...(rack.leaves || []).filter((p) => model.devices[p]?.slot.generated)]) {
      const d = model.devices[dp]; if (!d) continue;
      for (const p of d.ports) if (p.link) { const l = model.linkById[p.link]; if (l) { const other = l.a.dev === dp ? l.b : l.a; const od = model.devices[other.dev]; const op = od && od.ports.find((x) => x.name === other.port); if (op) { op.peer = null; op.link = null; } delete model.linkById[l.id]; model.links = model.links.filter((x) => x.id !== l.id); } }
      delete model.devices[dp]; model.order = model.order.filter((p) => p !== dp);
      const net = Object.values(model.racks).find((r) => r.format === 'generic/network-rack');
      if (net) { net.devices = net.devices.filter((p) => p !== dp); net.zeroU = net.zeroU.filter((p) => p !== dp); }
    }
    for (const loop of model.facility.loops) loop.racks = loop.racks.filter((p) => p !== rackPath);
    const row = model.hall.rows.find((r) => r.name === rack.row); const pos = row.positions.find((p) => p.rack === rackPath); if (pos) pos.rack = null;
    const estRow = model.estate.spec.sites[0].halls[0].rows.find((r) => r.name === rack.row); estRow.racks = estRow.racks.filter((r) => r.name !== rack.name);
    delete model.racks[rackPath];
  }

  // Serialise the estate file back to YAML (for Export in design mode).
  function toYAML(obj, indent = 0) {
    const pad = ' '.repeat(indent);
    if (Array.isArray(obj)) return obj.map((v) => (typeof v === 'object' && v !== null ? `${pad}- ${toYAML(v, indent + 2).trimStart()}` : `${pad}- ${scalar(v)}`)).join('\n');
    if (typeof obj === 'object' && obj !== null) return Object.entries(obj).map(([k, v]) => (typeof v === 'object' && v !== null ? (Array.isArray(v) && v.length === 0 ? `${pad}${k}: []` : `${pad}${k}:\n${toYAML(v, indent + 2)}`) : `${pad}${k}: ${scalar(v)}`)).join('\n');
    return pad + scalar(obj);
  }
  function scalar(v) { if (typeof v === 'string' && /[:{}\[\],#&*!|>'"%@`]|^\s|\s$|^$/.test(v) && !/^[\w./-]+$/.test(v)) return JSON.stringify(v); return String(v); }

  return { file, compile, addRack, removeRack, toYAML, paramsFor, HALLPATH };
})();
