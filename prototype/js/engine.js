// engine.js — the single writer of the model (spec 02): clock, event log, behaviours (power, boot,
// firmware, sensors, health roll-up), the fault engine, protocol requests, selectors, drafts and the
// fidelity report. Durations are SHORTENED for the prototype so a demo stays interactive.
window.DCT = window.DCT || {};
DCT.engine = (() => {
  const U = DCT.util, P = DCT.packs;
  const E = { model: null, now: null, seq: 0, events: [], faultInstances: [], scheduled: [], listeners: [], session: [], sessionStart: null,
    chaos: { enabled: false, ratePerDeviceHour: 2 }, paused: false };
  const MAX_EVENTS = 6000;
  let rand = U.rng('engine');

  // ---------------------------------------------------------------- events
  E.on = (fn) => E.listeners.push(fn);
  E.emit = (ev) => {
    ev.seq = ++E.seq; ev.t = new Date(E.now.getTime());
    E.events.push(ev);
    if (E.events.length > MAX_EVENTS) E.events.splice(0, 500);
    for (const l of E.listeners) { try { l(ev); } catch (err) { console.error(err); } }
    return ev;
  };
  const dev = (p) => E.model.devices[p];
  E.dev = dev;
  E.rack = (p) => E.model.racks[p];
  E.profile = (d) => P.profiles[d.profile];

  // --------------------------------------------------------------- helpers used by fault catalogues
  E.kernelLog = (d, line) => { d.kmsg.unshift({ t: new Date(E.now), line }); d.kmsg.length = Math.min(d.kmsg.length, 100); if (d.fidelity === 'vm') E.emit({ kind: 'kernel', path: d.path, msg: line, source: 'in-host agent', minor: true }); };
  E.redfishLog = (d, severity, msg) => { d.logs.unshift({ t: new Date(E.now), severity, msg }); d.logs.length = Math.min(d.logs.length, 200); };
  E.shim = (d, c, fault) => { if (d.fidelity === 'vm') E.emit({ kind: 'state', what: 'shim', path: c.path, msg: `in-host shim (${(E.profile(d).fidelity.vm?.inHost || ['shim'])[0]}): ${fault}`, source: 'in-host agent', minor: true }); };
  E.bms = (d, value) => {
    const topic = bmsTopic(d, 'Leak');
    E.model.facility.bms.points[topic] = { value, t: new Date(E.now) };
    E.emit({ kind: 'bms', path: d.path, msg: `${topic} → ${value}`, source: 'bms publisher', data: { topic, value } });
  };
  function bmsTopic(d, pointType) {
    const pr = E.profile(d); const tag = d.path.split('/').slice(1).join('.');
    return `BMS/v1/PUB/Value/${pr.bms?.objectType || 'GenericObject'}/${pointType}/${tag}`;
  }
  E.bmsTopic = bmsTopic;
  E.checkPowerLoss = (d) => {
    const failed = d.components.filter((c) => c.kind === 'psu' && c.state === 'failed').length;
    if (d.class === 'power-shelf') { if (failed >= 3 && !d.outputLost) { d.outputLost = true; d.ownHealth = 'Critical'; E.emit({ kind: 'state', what: 'power', path: d.path, msg: `shelf output lost: ${failed} of 6 PSUs failed`, source: 'behaviour' }); E.checkBusbar(d.rack); } return; }
    if (failed >= 2 && d.power.state !== 'Off') hardOff(d, `power loss: ${failed} PSUs failed`);
  };
  E.checkBusbar = (rackPath) => {
    const rack = E.rack(rackPath); if (!rack || !rack.busbar) return;
    const fmt = P.formats[rack.format];
    const shelves = rack.devices.map(dev).filter((d) => d.class === 'power-shelf');
    const ok = shelves.filter((s) => !s.outputLost && s.power.state !== 'Off').length;
    rack.busbarOk = ok >= (fmt.power.minShelves || 1);
    if (!rack.busbarOk) {
      E.emit({ kind: 'state', what: 'power', path: rack.path, msg: `busbar undervoltage: ${ok} of ${shelves.length} shelves supplying (minimum ${fmt.power.minShelves})`, source: 'behaviour' });
      for (const dp of rack.devices) { const d = dev(dp); if (['compute-tray', 'switch-tray'].includes(d.class) && d.power.state !== 'Off') hardOff(d, 'busbar undervoltage'); }
    }
  };
  E.setLink = (link, state) => {
    if (!link || link.state === state) return;
    link.state = state;
    for (const end of [link.a, link.b]) { const d = dev(end.dev); const p = d && d.ports.find((x) => x.name === end.port); if (p) p.state = state; }
    const sw = [link.a, link.b].map((e) => dev(e.dev)).find((d) => d && d.class === 'switch');
    const host = [link.a, link.b].map((e) => dev(e.dev)).find((d) => d && d.class !== 'switch') || dev(link.a.dev);
    E.emit({ kind: 'state', what: 'link', path: host.path, msg: `link ${link.a.port} ↔ ${U.lastSeg(link.b.dev)}:${link.b.port} → ${state}`, source: 'behaviour', data: { link: link.id, state } });
    if (sw) E.emit({ kind: 'nos', path: sw.path, msg: `${sw.name}: port ${sw === dev(link.a.dev) ? link.a.port : link.b.port} ${state === 'down' ? 'down — LLDP neighbour lost' : 'up — LLDP neighbour ' + U.lastSeg(host.path)}`, source: `nos:${sw.name}` });
  };

  // ------------------------------------------------------------- power behaviour
  function hardOff(d, why) {
    d.power.state = 'Off'; d.power.transition = null;
    E.emit({ kind: 'state', what: 'power', path: d.path, msg: `power On → Off (hard: ${why})`, source: 'behaviour' });
    afterOff(d);
  }
  function afterOff(d) {
    for (const c of d.components) if (c.kind === 'accelerator') c.lanesUp = 0;
    if (d.fidelity === 'vm') d.console.unshift({ t: new Date(E.now), line: '<power lost>' });
  }
  function afterOn(d) {
    for (const c of d.components) if (c.kind === 'accelerator' && c.state !== 'offline') c.lanesUp = c.lanes;
    // staged firmware applies on the reset that completes here
    let applied = [];
    for (const f of d.firmware) if (f.status === 'staged') { f.version = f.staged; f.status = 'in-baseline'; delete f.staged; applied.push(f.component); }
    if (applied.length) { E.emit({ kind: 'state', what: 'firmware', path: d.path, msg: `firmware applied on reset: ${applied.join(', ')} → baseline`, source: 'behaviour' }); clearFaultsOn(d, 'firmware-update'); }
    clearFaultsOn(d, 'reset');
    if (d.fidelity === 'vm') {
      const lines = ['UEFI POST … TPM 2.0 measured boot: PCR0 PCR2 PCR7 extended', `PXE: DHCP via ${U.lastSeg(d.rack)} relay → 10.20.0.${30 + (U.hash(d.path) % 200)} (stack)`, 'iPXE: chain http://10.20.0.5/boot/${d.identity.serial} … ok', d.attestation === 'fail' ? 'attestation: PCR 7 does not match policy — REFUSED by stack' : 'attestation: quote accepted (estate EK CA)', 'Linux 6.12 … nvidia 580 (mokka shim) loaded, 4 devices'];
      lines.forEach((l, i) => setTimeout(() => d.console.unshift({ t: new Date(E.now), line: l.replace('${d.identity.serial}', d.identity.serial) }), 400 * (i + 1)));
    }
  }
  function clearFaultsOn(d, trigger) {
    for (const inst of E.faultInstances.filter((i) => !i.cleared && i.device === d.path)) {
      const f = P.faultById[inst.fault];
      if ((f.clearsOn || []).includes(trigger)) E.clearFault(inst.id, { source: 'behaviour', why: trigger });
    }
  }
  const canPowerOn = (d) => {
    const rack = E.rack(d.rack);
    if (rack && rack.busbar && ['compute-tray', 'switch-tray'].includes(d.class)) { E.checkBusbarQuiet(rack); if (rack.busbarOk === false) return 'busbar undervoltage'; }
    const failed = d.components.filter((c) => c.kind === 'psu' && c.state === 'failed').length; if (failed >= 2) return `${failed} PSUs failed`;
    return null;
  };
  E.checkBusbarQuiet = (rack) => { const fmt = P.formats[rack.format]; const shelves = rack.devices.map(dev).filter((d) => d.class === 'power-shelf'); const ok = shelves.filter((s) => !s.outputLost && s.power.state !== 'Off').length; rack.busbarOk = ok >= (fmt.power.minShelves || 1); };

  // action: On | ForceOff | GracefulShutdown | ForceRestart | PowerCycle | ACCycle
  E.power = (path, action, opts = {}) => {
    const d = dev(path); if (!d || d.class === 'sensor') return { ok: false, why: 'no power control' };
    const b = E.profile(d).behaviours || {}; const src = opts.source || 'user';
    const t = (s) => new Date(E.now.getTime() + s * 1000);
    const was = d.power.state;
    const ev = (msg) => E.emit({ kind: 'state', what: 'power', path, msg, source: src, cause: opts.cause });
    switch (action) {
      case 'On': {
        if (was === 'On' || was === 'PoweringOn') return { ok: true, noop: true };
        const why = canPowerOn(d); if (why) { ev(`power-on refused: ${why}`); return { ok: false, why }; }
        d.power.state = 'PoweringOn'; d.power.transition = { to: 'On', endsAt: t(b.onSeconds || 5) }; ev(`power ${was} → PoweringOn`); break; }
      case 'ForceOff': case 'GracefulShutdown': {
        if (was === 'Off' || was === 'PoweringOff') return { ok: true, noop: true };
        d.power.state = 'PoweringOff'; d.power.transition = { to: 'Off', endsAt: t(action === 'ForceOff' ? (b.offSeconds || 2) : (b.offSeconds || 2) * 3) }; ev(`power ${was} → PoweringOff (${action})`); break; }
      case 'ForceRestart': case 'PowerCycle': case 'ACCycle': {
        d.power.state = 'PoweringOff'; d.power.transition = { to: 'Off', endsAt: t(b.offSeconds || 2), then: 'On', ac: action === 'ACCycle' }; ev(`power ${was} → PoweringOff (${action})`); break; }
      default: return { ok: false, why: 'unknown action' };
    }
    if (src.startsWith('user')) E.session.push({ t: new Date(E.now), type: 'power', path, action });
    return { ok: true };
  };

  function progressTransitions() {
    for (const p of E.model.order) {
      const d = dev(p); const tr = d.power.transition; if (!tr || E.now < tr.endsAt) continue;
      d.power.transition = null; d.power.state = tr.to;
      E.emit({ kind: 'state', what: 'power', path: p, msg: `power → ${tr.to}`, source: 'behaviour' });
      if (tr.to === 'Off') { afterOff(d); if (tr.ac) clearFaultsOn(d, 'ac-cycle'); if (tr.then === 'On') { const why = canPowerOn(d); if (!why) { d.power.state = 'PoweringOn'; d.power.transition = { to: 'On', endsAt: new Date(E.now.getTime() + (E.profile(d).behaviours.onSeconds || 5) * 1000) }; } } }
      else if (tr.to === 'On') afterOn(d);
      if (d.class === 'power-shelf') E.checkBusbarQuiet(E.rack(d.rack));
    }
  }

  // --------------------------------------------------------------- sensors & power draw
  function loopSupply(rackPath) {
    const loop = E.model.facility.loops.find((l) => l.racks.includes(rackPath));
    if (!loop) return 22; const cdu = dev(loop.device); return 30 + (cdu.supplyOffset || 0);
  }
  function tickPower() {
    const jitter = () => 1 + (rand() - 0.5) * 0.04;
    const rackW = {};
    for (const p of E.model.order) {
      const d = dev(p); const pr = E.profile(d); const pw = pr.power;
      let w;
      if (['power-shelf', 'pdu', 'cdu', 'sensor'].includes(d.class)) continue;
      if (d.power.state === 'Off') w = pw.standbyW; else if (d.power.state !== 'On') w = pw.idleW;
      else {
        const acc = d.components.filter((c) => c.kind === 'accelerator');
        const healthy = acc.length ? acc.filter((c) => c.state !== 'offline').length / acc.length : 1;
        w = (pw.idleW + d.power.load * (pw.maxW - pw.idleW) * healthy) * jitter();
      }
      d.power.watts = w;
      if (d.rack) rackW[d.rack] = (rackW[d.rack] || 0) + w;
    }
    for (const rack of Object.values(E.model.racks)) {
      rack.watts = rackW[rack.path] || 0;
      const shelves = rack.devices.map(dev).filter((d) => d.class === 'power-shelf');
      const okShelves = shelves.filter((s) => !s.outputLost && s.power.state !== 'Off');
      for (const s of shelves) { s.output = okShelves.includes(s) ? rack.watts / okShelves.length : 0; s.power.watts = 25 + s.output * 0.03; }
      const pdus = rack.zeroU.map(dev).filter((d) => d.class === 'pdu');
      for (const pd of pdus) pd.power.watts = rack.watts / pdus.length;
    }
    for (const f of E.model.facility.feeds) f.watts = 0;
    for (const rack of Object.values(E.model.racks)) for (const f of E.model.facility.feeds) f.watts += rack.watts / E.model.facility.feeds.length;
    for (const loop of E.model.facility.loops) { const cdu = dev(loop.device); loop.heatW = loop.racks.reduce((s, r) => s + (E.model.racks[r]?.watts || 0), 0); cdu.power.watts = 2000 + loop.heatW * 0.02; }
  }
  function tickSensors() {
    const lag = 0.25;
    const track = (s, target) => { s.value = s.value === null ? target : s.value + (target - s.value) * lag; };
    for (const p of E.model.order) {
      const d = dev(p); const on = d.power.state === 'On'; const load = on ? d.power.load : 0; const off = d.thermalOffset || 0;
      for (const s of d.sensors) {
        let tgt;
        switch (s.name) {
          case 'coolant-in': tgt = loopSupply(d.rack) + 0.5; break;
          case 'gpu-max': tgt = (d.class === 'compute-tray' ? loopSupply(d.rack) + 24 : 22 + 33) + 14 * load + off; break;
          case 'cpu-max': tgt = loopSupply(d.rack) + 20 + 10 * load + off; break;
          case 'asic-max': tgt = loopSupply(d.rack) + 18 + 12 * (on ? 0.6 : 0) + off; break;
          case 'inlet': tgt = 22 + (d.class === 'switch' ? 2 : 0); break;
          case 'exhaust': tgt = 22 + 10 + 22 * load + off; break;
          case 'power': case 'output-power': tgt = d.class === 'power-shelf' ? (d.output || 0) : d.power.watts; break;
          case 'psu-max': tgt = 35 + 30 * ((d.output || 0) / 33000); break;
          case 'load': tgt = d.power.watts / (Math.sqrt(3) * 400); break;
          case 'supply': tgt = 30 + (d.supplyOffset || 0); break;
          case 'return': { const loop = E.model.facility.loops.find((l) => l.device === d.path); tgt = 30 + (d.supplyOffset || 0) + 8 + (loop?.heatW || 0) / 40000; break; }
          case 'flow': tgt = 420 + (rand() - 0.5) * 6; break;
          case 'pressure': tgt = 2.1; break;
          case 'leak': s.value = d.leak ? 1 : 0; continue;
          default: tgt = s.value ?? 0;
        }
        track(s, tgt);
      }
    }
  }

  // ------------------------------------------------------------- health roll-up with change events
  function rollupAll() {
    for (const p of E.model.order) {
      const d = dev(p);
      for (const c of d.components) { if (c.lastHealth === undefined) c.lastHealth = c.health; if (c.health !== c.lastHealth) { const was = c.lastHealth; c.lastHealth = c.health; E.emit({ kind: 'state', what: 'health', path: c.path, msg: `${c.name} health ${was} → ${c.health}`, source: 'behaviour' }); } }
      const sevs = [d.ownHealth || 'OK', ...d.components.map((c) => c.health)];
      for (const s of d.sensors) if (s.value !== null && s.crit !== null && s.crit !== undefined) { if (s.value >= s.crit) sevs.push('Critical'); else if (s.value >= s.warn) sevs.push('Warning'); }
      const h = U.maxSeverity(sevs);
      if (h !== d.health) { const was = d.health; d.health = h; E.emit({ kind: 'state', what: 'health', path: p, msg: `health ${was} → ${h}`, source: 'behaviour' }); }
    }
    for (const rack of Object.values(E.model.racks)) {
      const ds = rack.devices.map(dev);
      rack.health = U.maxSeverity(ds.map((d) => d.health));
      for (const dom of rack.domains) {
        if (dom.kind !== 'nvlink') continue;
        const trays = ds.filter((d) => d.class === 'switch-tray'); const ok = trays.filter((t) => !t.failed && t.power.state === 'On').length;
        const state = ok === trays.length ? 'Ready' : ok === 0 ? 'Down' : 'Degraded';
        if (state !== dom.state) { dom.state = state; dom.traysOk = ok; dom.traysTotal = trays.length; E.emit({ kind: 'state', what: 'domain', path: dom.path, msg: `NVLink domain ${rack.name}: ${state} (${ok}/${trays.length} switch trays)`, source: 'behaviour' }); }
        dom.traysOk = ok; dom.traysTotal = trays.length;
      }
    }
  }
  E.rollup = rollupAll;

  // --------------------------------------------------------------- faults
  function resolveTarget(f, targetPath) {
    if (f.target.port) { const link = E.model.linkById[targetPath]; if (!link) return null; return { device: dev(link.a.dev), comp: null, link }; }
    if (dev(targetPath)) return { device: dev(targetPath), comp: null, link: null };
    const d = dev(U.parentPath(targetPath)); if (!d) return null;
    const comp = d.components.find((c) => c.path === targetPath); if (!comp) return null;
    return { device: d, comp, link: null };
  }
  E.inject = (faultId, targetPath, opts = {}) => {
    const f = P.faultById[faultId]; if (!f) return { ok: false, why: 'unknown fault' };
    const T = resolveTarget(f, targetPath); if (!T) return { ok: false, why: 'target not found' };
    const dup = E.faultInstances.find((i) => !i.cleared && i.fault === faultId && i.target === targetPath); if (dup) return { ok: false, why: 'already active' };
    const beforeHealth = T.device.health;
    const inst = { id: ++E.seq, fault: faultId, title: f.title, target: targetPath, device: T.device.path, comp: T.comp ? T.comp.name : null, link: T.link ? T.link.id : null, since: new Date(E.now), by: opts.source || 'user', cleared: null };
    E.faultInstances.push(inst); T.device.faults.push(inst.id);
    T.device.health = beforeHealth; // apply sets ownHealth through the device's own field when device-level
    const prevOwn = T.device.ownHealth || 'OK';
    const snapshot = T.device.health;
    f.apply(E, T.device, T.comp, T.link);
    if (!T.comp && !T.link && T.device.health !== snapshot) { T.device.ownHealth = T.device.health; T.device.health = snapshot; } else if (!T.comp && !T.link) T.device.ownHealth = prevOwn;
    const ev = E.emit({ kind: 'fault', path: targetPath, msg: `${f.title} ← ${faultId}`, source: opts.source || 'user', cause: opts.cause, data: { inst: inst.id, fault: faultId } });
    inst.eventSeq = ev.seq;
    if (T.device.class === 'switch-tray' && f.id.includes('switch-tray')) E.emit({ kind: 'state', what: 'nvlink', path: T.device.path, msg: `NVLink lanes ${T.device.nvlinkLanes} → 0`, source: 'behaviour', cause: ev.seq });
    rollupAll();
    if ((opts.source || 'user').startsWith('user')) E.session.push({ t: new Date(E.now), type: 'fault', fault: faultId, target: targetPath });
    if (opts.clearAfter) E.scheduled.push({ at: new Date(E.now.getTime() + opts.clearAfter * 1000), clear: inst.id, source: opts.source || 'user' });
    return { ok: true, inst };
  };
  E.clearFault = (instId, opts = {}) => {
    const inst = E.faultInstances.find((i) => i.id === instId); if (!inst || inst.cleared) return { ok: false };
    const f = P.faultById[inst.fault]; const T = resolveTarget(f, inst.target); if (!T) return { ok: false };
    const snapshot = T.device.health;
    f.clear(E, T.device, T.comp, T.link);
    if (!T.comp && !T.link) { const others = E.faultInstances.filter((i) => !i.cleared && i.id !== instId && i.device === T.device.path && !i.comp && !i.link); T.device.ownHealth = others.length ? T.device.ownHealth : 'OK'; T.device.health = snapshot; }
    inst.cleared = new Date(E.now); inst.clearedBy = opts.source || 'user';
    T.device.faults = T.device.faults.filter((x) => x !== instId);
    E.emit({ kind: 'fault', path: inst.target, msg: `cleared: ${f.title}${opts.why ? ' (' + opts.why + ')' : ''}`, source: opts.source || 'user', cause: inst.eventSeq, data: { inst: inst.id, cleared: true } });
    if (T.device.class === 'switch-tray' && f.id.includes('switch-tray')) E.emit({ kind: 'state', what: 'nvlink', path: T.device.path, msg: `NVLink lanes 0 → ${T.device.nvlinkLanes}`, source: 'behaviour' });
    rollupAll();
    if ((opts.source || 'user').startsWith('user')) E.session.push({ t: new Date(E.now), type: 'clear', fault: inst.fault, target: inst.target });
    return { ok: true };
  };
  E.schedule = (s) => E.scheduled.push(s);
  E.activeFaults = () => E.faultInstances.filter((i) => !i.cleared);
  function tickScheduled() {
    const due = E.scheduled.filter((s) => E.now >= s.at); E.scheduled = E.scheduled.filter((s) => E.now < s.at);
    for (const s of due) {
      if (s.clear) E.clearFault(s.clear, { source: s.source, why: 'clear-after' });
      else { const r = E.inject(s.fault, s.target, { source: s.source, clearAfter: s.clearAfter }); if (s.every) E.scheduled.push({ ...s, at: new Date(E.now.getTime() + s.every * 1000) }); }
    }
  }
  function tickChaos() {
    if (!E.chaos.enabled) return;
    const p = E.chaos.ratePerDeviceHour / 3600;
    for (const path of E.model.order) {
      if (rand() >= p) continue;
      const d = dev(path); if (d.class === 'sensor' && rand() < 0.7) continue;
      const comps = [null, ...d.components];
      const c = U.pick(rand, comps);
      const list = P.applicable(d, c, null).filter((f) => f.id !== 'generic/leak/tray-level' || rand() < 0.2);
      if (!list.length) continue;
      E.inject(U.pick(rand, list).id, c ? c.path : d.path, { source: 'chaos' });
    }
  }

  // --------------------------------------------------------------- protocol requests (Redfish)
  E.redfishTree = (d) => {
    const dl = P.dialects[d.bmc?.dialect || 'generic/openbmc']; const pr = E.profile(d);
    const S = `/redfish/v1/Systems/${dl.systemId}`, C = `/redfish/v1/Chassis/${dl.chassisId}`, M = `/redfish/v1/Managers/${dl.managerId}`;
    const T = {};
    T['/redfish/v1'] = { '@odata.id': '/redfish/v1', Id: 'RootService', RedfishVersion: '1.18.0', ...dl.root, Systems: { '@odata.id': '/redfish/v1/Systems' }, Chassis: { '@odata.id': '/redfish/v1/Chassis' }, Managers: { '@odata.id': '/redfish/v1/Managers' }, UpdateService: { '@odata.id': '/redfish/v1/UpdateService' }, EventService: { '@odata.id': '/redfish/v1/EventService' }, Links: { Sessions: { '@odata.id': '/redfish/v1/SessionService/Sessions' } } };
    const ps = d.power.state === 'PoweringOn' ? 'PoweringOn' : d.power.state === 'PoweringOff' ? 'PoweringOff' : d.power.state;
    T['/redfish/v1/Systems'] = { Members: [{ '@odata.id': S }], 'Members@odata.count': 1 };
    T[S] = { '@odata.id': S, Id: dl.systemId, Manufacturer: d.vendor, Model: pr.title, SerialNumber: d.identity.serial, UUID: d.identity.uuid, PowerState: ps, Status: { Health: d.health === 'Unknown' ? 'Warning' : d.health, State: ps === 'On' ? 'Enabled' : 'Disabled' }, Boot: { BootSourceOverrideEnabled: 'Disabled', BootSourceOverrideTarget: 'None', BootOrder: ['Pxe', 'Hdd'] }, ProcessorSummary: { Count: d.components.filter((c) => c.kind === 'cpu').length }, Processors: { '@odata.id': `${S}/Processors` }, Actions: { '#ComputerSystem.Reset': { target: `${S}/Actions/ComputerSystem.Reset`, 'ResetType@Redfish.AllowableValues': ['On', 'ForceOff', 'GracefulShutdown', 'ForceRestart', 'PowerCycle'] } }, Links: { Chassis: [{ '@odata.id': C }] } };
    T[`${S}/Processors`] = { Members: d.components.filter((c) => ['cpu', 'accelerator'].includes(c.kind)).map((c) => ({ '@odata.id': `${S}/Processors/${c.name}` })) };
    for (const c of d.components.filter((c) => ['cpu', 'accelerator'].includes(c.kind))) T[`${S}/Processors/${c.name}`] = { Id: c.name, ProcessorType: c.kind === 'accelerator' ? 'GPU' : 'CPU', Model: c.model, Status: { Health: c.health, State: c.state === 'offline' ? 'Absent' : 'Enabled' }, ...(c.kind === 'accelerator' ? { Oem: { [d.vendor]: { PCIeAddress: c.pci, Counters: c.counters, NVLinkLanesUp: c.lanesUp } } } : {}) };
    T['/redfish/v1/Chassis'] = { Members: [{ '@odata.id': C }] };
    T[C] = { '@odata.id': C, Id: dl.chassisId, ChassisType: d.class === 'compute-tray' || d.class === 'switch-tray' ? 'Sled' : 'RackMount', Manufacturer: d.vendor, Model: dl.chassisModel || pr.title, SerialNumber: d.identity.serial, PowerState: ps, Status: { Health: d.health }, Sensors: { '@odata.id': `${C}/Sensors` }, Links: { ComputerSystems: [{ '@odata.id': S }] } };
    T[`${C}/Sensors`] = { Members: d.sensors.map((s) => ({ '@odata.id': `${C}/Sensors/${s.name}` })) };
    for (const s of d.sensors) T[`${C}/Sensors/${s.name}`] = { Id: s.name, ReadingType: s.quantity, Reading: s.value === null ? null : Math.round(s.value * 10) / 10, ReadingUnits: s.unit, Thresholds: s.warn !== null ? { UpperCaution: { Reading: s.warn }, UpperCritical: { Reading: s.crit } } : undefined, Status: { Health: s.value !== null && s.crit !== null && s.value >= s.crit ? 'Critical' : s.value !== null && s.warn !== null && s.value >= s.warn ? 'Warning' : 'OK' } };
    T['/redfish/v1/Managers'] = { Members: [{ '@odata.id': M }] };
    T[M] = { '@odata.id': M, Id: dl.managerId, ManagerType: 'BMC', FirmwareVersion: (d.firmware.find((f) => f.component === 'bmc') || d.firmware[0] || {}).version, Status: { Health: 'OK', State: 'Enabled' }, EthernetInterfaces: { '@odata.id': `${M}/EthernetInterfaces` }, LogServices: { '@odata.id': `${M}/LogServices` } };
    T[`${M}/EthernetInterfaces`] = { Members: [{ '@odata.id': `${M}/EthernetInterfaces/eth0` }] };
    T[`${M}/EthernetInterfaces/eth0`] = { Id: 'eth0', MACAddress: d.bmc?.mac, IPv4Addresses: d.bmc?.lease ? [{ Address: d.bmc.lease, AddressOrigin: 'DHCP' }] : [], DHCPv4: { DHCPEnabled: true } };
    T[`${M}/LogServices`] = { Members: [{ '@odata.id': `${M}/LogServices/Log` }] };
    T[`${M}/LogServices/Log/Entries`] = { Members: d.logs.slice(0, 20).map((l, i) => ({ Id: String(i), Created: l.t.toISOString(), Severity: l.severity, Message: l.msg })) };
    T['/redfish/v1/UpdateService'] = { Id: 'UpdateService', ServiceEnabled: true, FirmwareInventory: { '@odata.id': '/redfish/v1/UpdateService/FirmwareInventory' }, Actions: { '#UpdateService.SimpleUpdate': { target: '/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate', '@Redfish.OperationApplyTimeSupport': { SupportedValues: ['OnReset', 'Immediate'] } } } };
    T['/redfish/v1/UpdateService/FirmwareInventory'] = { Members: d.firmware.map((f) => ({ '@odata.id': `/redfish/v1/UpdateService/FirmwareInventory/${f.component}` })) };
    for (const f of d.firmware) T[`/redfish/v1/UpdateService/FirmwareInventory/${f.component}`] = { Id: f.component, Version: f.version, Updateable: true, Status: { State: f.status === 'staged' ? 'StandbySpare' : 'Enabled', Health: f.status === 'out-of-baseline' ? 'Warning' : 'OK' } };
    T['/redfish/v1/EventService'] = { Id: 'EventService', ServiceEnabled: true, ServerSentEventUri: '/redfish/v1/EventService/SSE', Subscriptions: { '@odata.id': '/redfish/v1/EventService/Subscriptions' } };
    return T;
  };
  // A request from the software under test (or the operator) to a device's BMC. Returns {status, latency}.
  E.request = (path, req) => {
    const d = dev(path); const t = new Date(E.now);
    let status, latency, note;
    if (!d || !d.bmc) { status = 0; latency = 3000; note = 'no route'; }
    else if (!d.bmc.reachable) { status = 0; latency = 5000; note = 'timeout'; }
    else {
      latency = Math.round(40 + rand() * 140);
      if (d.bmc.slow) { latency = Math.round(3000 + rand() * 5000); if (rand() < 0.125) { status = 503; note = 'ServiceUnavailable'; } }
      if (!status) {
        const tree = E.redfishTree(d);
        if (req.method === 'GET') status = tree[req.rpath] ? 200 : 404;
        else if (req.method === 'POST') status = /Actions\//.test(req.rpath) ? 202 : 404;
        else status = 405;
      }
    }
    d && d.requests.unshift({ t, method: req.method, rpath: req.rpath, status, latency, from: req.from || 'operator', note });
    if (d) d.requests.length = Math.min(d.requests.length, 80);
    if (!req.quiet) E.emit({ kind: 'request', path, msg: `${req.method} ${req.rpath} → ${status || 'timeout'} (${latency} ms)`, source: req.from || 'operator', cause: req.cause, data: { status, latency } });
    return { status, latency };
  };
  // A Redfish action with its semantic: logs the request, then performs it if the BMC answered.
  E.redfishAction = (path, action, body, opts = {}) => {
    const d = dev(path); if (!d) return { status: 0 };
    const dl = P.dialects[d.bmc?.dialect || 'generic/openbmc'];
    let rpath;
    if (action === 'Reset') rpath = `/redfish/v1/Systems/${dl.systemId}/Actions/ComputerSystem.Reset`;
    else if (action === 'SimpleUpdate') rpath = '/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate';
    const r = E.request(path, { method: 'POST', rpath, from: opts.from, cause: opts.cause });
    if (r.status === 202) {
      if (action === 'Reset') E.power(path, body.ResetType, { source: opts.from || 'stack', cause: opts.cause });
      if (action === 'SimpleUpdate') { for (const f of d.firmware) if (f.status === 'out-of-baseline') { f.status = 'staged'; f.staged = f.baseline; } }
    }
    return r;
  };

  // --------------------------------------------------------------- selectors
  E.allPaths = () => { const out = []; for (const p of E.model.order) { out.push(p); for (const c of dev(p).components) out.push(c.path); } for (const r of Object.keys(E.model.racks)) out.push(r); return out; };
  E.select = (sel) => E.allPaths().filter((p) => U.matchSelector(sel, p));
  E.selectDevices = (sel) => E.model.order.filter((p) => U.matchSelector(sel, p) || U.matchSelector(sel, dev(p).rack || '') || (dev(p).rack && U.matchSelector(sel, dev(p).rack + '/**') && false));

  // --------------------------------------------------------------- drafts (design mode)
  E.applyDraft = async (draft, onStep) => {
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    const step = (m) => { onStep && onStep(m); E.emit({ kind: 'twin', what: 'apply', path: E.model.hall.path, msg: m, source: 'design' }); };
    for (const rp of draft.removes) {
      const rack = E.rack(rp); if (!rack) continue;
      step(`${rack.name}: powering off ${rack.devices.length} devices`); for (const dp of rack.devices) { const d = dev(dp); if (d.power.state !== 'Off') { d.power.state = 'Off'; d.power.transition = null; } }
      await sleep(600); step(`${rack.name}: unplugged from feeds A and B${rack.cooling === 'liquid' ? ', coolant isolated' : ''}`); await sleep(500);
      step(`${rack.name}: cables removed, de-racked`); E.emit({ kind: 'twin', what: 'deracked', path: rp, msg: `de-racked ${rack.name}`, source: 'design', data: { devices: rack.devices.slice() } });
      DCT.estate.removeRack(E.model, rp); await sleep(400);
    }
    for (const a of draft.adds) {
      step(`${a.name}: racked at row ${a.row} position ${a.position} (${P.formats[a.format].title})`); await sleep(500);
      const rack = DCT.estate.addRack(E.model, a.row, a.position, a);
      for (const dp of rack.devices) { const d = dev(dp); sensorsInit(d); }
      for (const f of draft.fidelity) for (const p of E.select(f.select)) if (rack.devices.includes(p)) dev(p).fidelity = f.level; // racked at its fidelity, not reseated
      step(`${a.name}: cabled — ${rack.newLinks?.length || 0} links (oob, frontend, backend rails)`); await sleep(600);
      step(`${a.name}: power connected to feeds A and B${rack.cooling === 'liquid' ? `; coolant connected to ${rack.loop}` : ''}`); await sleep(500);
      const bmcs = rack.devices.filter((p) => dev(p).bmc).length;
      step(`${a.name}: ${bmcs} BMCs on standby → requesting DHCP through the relay`);
      E.emit({ kind: 'twin', what: 'racked', path: rack.path, msg: `racked ${a.name}: ${rack.devices.length} devices, ${bmcs} BMCs on standby`, source: 'design', data: { devices: rack.devices.slice() } });
      await sleep(400);
    }
    for (const f of draft.fidelity) {
      for (const p of E.select(f.select)) { const d = dev(p); if (!d || d.fidelity === f.level || d.justRacked) continue; const was = d.fidelity; d.fidelity = f.level; d.power.state = 'Off'; d.power.transition = null; step(`${U.shortPath(p)}: fidelity ${was} → ${f.level} (reseated; substrate ${f.level === 'vm' ? 'VM created' : 'VM destroyed'})`); E.emit({ kind: 'twin', what: 'reseated', path: p, msg: `reseated at fidelity ${f.level}`, source: 'design' }); }
      await sleep(400);
    }
    const spec = E.model.estate.spec; for (const f of draft.fidelity) spec.fidelity.overrides.push({ select: f.select, level: f.level });
    E.model.estate.spec.clock = E.model.order.some((p) => dev(p).fidelity !== 'model') ? 'wall' : 'virtual';
    rollupAll(); tickPower();
    step('apply complete');
  };
  function sensorsInit(d) { for (const s of d.sensors) s.value = null; }

  // --------------------------------------------------------------- fidelity report (D11)
  E.fidelityReport = (paths) => {
    const ds = paths.map(dev).filter(Boolean);
    const counts = { vm: 0, model: 0, hardware: 0 }; for (const d of ds) counts[d.fidelity]++;
    const notes = new Map();
    for (const d of ds) { const pr = E.profile(d); for (const n of pr.isNot || []) notes.set(`${d.profile}: ${n}`, d.profile); if (d.fidelity === 'vm' && (pr.fidelity.vm?.inHost || []).includes('nvidia/mokka')) notes.set('nvidia/mokka: no CUDA execution; diagnostics that run kernels return the shim\'s canned result.', 'nvidia/mokka'); }
    const racks = new Set(ds.map((d) => d.rack).filter(Boolean)); for (const r of racks) for (const n of E.rack(r)?.isNot || []) notes.set(`${E.rack(r).format}: ${n}`, E.rack(r).format);
    const stack = DCT.stack;
    return { counts, impersonated: stack.options.scoutImpersonated && counts.model ? [`NICo scout (machine-a-tron) on ${ds.filter((d) => d.fidelity === 'model' && ['compute-tray', 'server'].includes(d.class)).length} model hosts`] : [], stubs: ['external/stub:nvidia-nras (attestation answered with test values)'], unsynchronised: [], addressing: 'stack (via NOS relays)', notModelled: [...notes.keys()], unverified: [...new Set(ds.map((d) => d.profile))].map((p) => `${p} (source: documentation; no hardware-run ledger entry)`) };
  };

  // --------------------------------------------------------------- lifecycle
  E.init = (model) => {
    E.model = model; E.now = new Date(); E.sessionStart = new Date(E.now);
    rand = U.rng(`engine:${model.estate.spec.seed}`);
    for (const r of Object.values(model.racks)) E.checkBusbarQuiet(r);
    tickPower(); for (let i = 0; i < 12; i++) tickSensors(); rollupAll();
    E.emit({ kind: 'twin', path: model.hall.path, msg: `estate ${model.estate.metadata.name} up: ${Object.keys(model.racks).length} racks, ${model.order.length} devices, ${model.links.length} links; clock ${model.estate.spec.clock}`, source: 'twin' });
  };
  E.tick = () => {
    if (E.paused) return;
    E.now = new Date();
    progressTransitions(); tickPower(); tickSensors(); tickScheduled(); tickChaos(); rollupAll();
    DCT.stack.tick(E);
    for (const l of E.listeners) { try { l({ kind: 'tick', t: E.now }); } catch (err) { console.error(err); } }
  };
  E.start = () => { if (E.timer) return; E.timer = setInterval(E.tick, 1000); };

  return E;
})();
