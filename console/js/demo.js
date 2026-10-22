// demo.js — the in-browser demo: an illustrative hall (hall-a-demo, seed 1729) with a fault
// engine, a simulated stack, facility power and cooling, scenarios and design apply, all in
// memory. It runs when the page is opened without a twin. Durations are shortened so a demo
// stays interactive, the stack is a stand-in labelled "(sim)", and every number is illustrative.
window.TC = window.TC || {};
TC.Demo = (() => {
  const { SEVN, FMT, FORMAT_IDS, FAULTS, RACK_FAULTS, ISNOT, pad, hash } = TC.data;

  class Demo {
    constructor(opts) {
      this.kind = 'demo';
      this.opts = opts || {};
      this.onChange = () => {};
      this.caps = { infra: true, design: true, schedule: true, scenarios: 'demo', chaosUnit: '/min', topology: true };
      this.estate = { name: 'hall-a-demo', seed: 1729, packs: 6, hall: 'HALL-A' };
      this.sec = 14 * 3600 + 2 * 60;
      this.seq = 0; this.events = []; this.activeI = []; this.pulses = {}; this.pulled = new Set(); this.fid = 0; this.scenRes = {}; this.scenRun = null; this.scenLast = null;
      this.feeds = { A: { up: true, capW: 900000 }, B: { up: true, capW: 900000 } };
      this.utility = { A: { up: true }, B: { up: true } };
      this.ups = { A: { mode: 'online', batt: 100 }, B: { mode: 'online', batt: 100 } };
      this.rpp = {}; this.cduS = { flow: 1200, supplyOff: 0, state: 'OK' }; this.bms = {};
      this.chaos = { on: false, rate: 2 };
      this.build();
      this.seedHistory();
    }
    start() { this.iv = setInterval(() => this.tick(), 1000); }
    stop() { clearInterval(this.iv); this.activeI.forEach((a) => a.timers.forEach(clearTimeout)); }
    changed() { this.onChange(); }
    speed() { return Number(this.opts.speed ?? 1) || 1; }
    stackOn() { return this.opts.stackResponds !== false; }
    clock() { const s = this.sec; return `${pad(Math.floor(s / 3600) % 24)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}`; }
    lag() { return 30 + (hash('l' + this.sec) % 30) + this.activeI.length * 6; }
    incidents() { return 0; }
    stackLabel() { return { text: `simulated stack · ${this.stackOn() ? 'responding' : 'responses off'}`, title: `nvidia-dsx-os · nico · nvsentinel · dsx-exchange · fleet-intel · ${this.stackOn() ? 'responding' : 'responses off'}` }; }

    // ---------- estate ----------
    build() {
      this.rows = [{ name: 'a', positions: 10 }, { name: 'b', positions: 8 }, { name: 'c', positions: 4 }];
      this.racks = {}; this.devs = {};
      [['a01', 'a', 1, 'nvl'], ['a02', 'a', 2, 'nvl'], ['a03', 'a', 3, 'nvl'], ['a04', 'a', 4, 'nvl'], ['a05', 'a', 5, 'hgx'], ['a06', 'a', 6, 'hgx'], ['a10', 'a', 10, 'net'],
        ['b01', 'b', 1, 'hgx'], ['b02', 'b', 2, 'oam'], ['b03', 'b', 3, 'gaudi'], ['c01', 'c', 1, 'cpu']].forEach((r) => this.addRack(...r));
      ['a03/compute-tray-01', 'a03/compute-tray-02'].forEach((p) => { this.devs[p].fid = 'vm'; });
      this.loop = ['a01', 'a02', 'a03', 'a04'];
      this.netRack = 'a10';
    }
    get cdus() { return [{ ...this.cdu, racks: this.loop }]; }
    get cdu() { return { name: 'cdu-1', path: 'facility/cdu-1', state: this.cduS.state, flow: this.cduS.flow, supplyC: 30.1 + this.cduS.supplyOff, returnC: 41.7 + this.cduS.supplyOff * 1.2, flowUnit: 'L/min' }; }
    addRack(name, row, pos, f) {
      const R = { name, path: name, row, pos, f, ...FMT[f], slots: [], sensors: [], flags: {} };
      this.racks[name] = R;
      const add = (n, cls, units, w, extra = {}) => {
        const path = `${name}/${n}`, h = hash(path);
        const d = { path, name: n, rack: name, cls, units, base: w, health: 'OK', power: 'On', fid: 'model', bmc: true, bmcSlow: false, tOff: 0, leak: false, fw: 'in baseline',
          serial: (h % 9 + 1) + String(h).padStart(10, '0').slice(0, 10), ip: `10.10.${Object.keys(this.racks).length}.${(R.slots.length + 11)}`, mac: '3c:ec:ef:' + [h >> 16, h >> 8, h].map((x) => pad((x & 255).toString(16))).join(':'), reqs: [], comps: [], ...extra };
        if (extra.gpus) for (let i = 0; i < extra.gpus; i++) d.comps.push({ name: `gpu-${i}`, kind: 'accelerator', model: extra.gpuModel, health: 'OK' });
        if (extra.cpus) for (let i = 0; i < extra.cpus; i++) d.comps.push({ name: `cpu-${i}`, kind: 'cpu', model: extra.cpuModel || 'Grace', health: 'OK' });
        if (cls !== 'sensor' && cls !== 'pdu') d.comps.push({ name: 'bmc', kind: 'bmc', model: extra.bmcModel || 'AST2600', health: 'OK' });
        if (extra.nics) d.comps.push({ name: 'nic-0', kind: 'nic', model: extra.nics, health: 'OK' });
        d.isNot = ISNOT[cls] || ISNOT.default;
        this.devs[path] = d;
        if (extra.zero) R.sensors.push(path); else R.slots.push(path);
        return d;
      };
      if (f === 'nvl') {
        for (let i = 1; i <= 4; i++) add(`power-shelf-${pad(i)}`, 'power-shelf', 1, 0, { profile: 'nvidia/power-shelf-33kw' });
        const tray = (i) => add(`compute-tray-${pad(i)}`, 'compute-tray', 1, 5400, { profile: 'nvidia/gb300-compute-tray', gpus: 4, gpuModel: 'GB300', cpus: 2, nics: 'ConnectX-8 SuperNIC', dialect: 'nvidia-gb', t: 30 });
        for (let i = 1; i <= 10; i++) tray(i);
        for (let i = 1; i <= 9; i++) add(`switch-tray-${pad(i)}`, 'switch-tray', 1, 800, { profile: 'nvidia/nvlink-switch-tray', dialect: 'nvidia-gb', t: 31 });
        for (let i = 11; i <= 18; i++) tray(i);
        for (let i = 5; i <= 8; i++) add(`power-shelf-${pad(i)}`, 'power-shelf', 1, 0, { profile: 'nvidia/power-shelf-33kw' });
        for (let i = 1; i <= 3; i++) add(`leak-sensor-${pad(i)}`, 'sensor', 0, 0, { zero: true, at: [3, 16, 30][i - 1], profile: 'generic/leak-rope' });
      } else if (f === 'net') {
        for (let i = 1; i <= 2; i++) add(`spine-${pad(i)}`, 'switch', 2, 1500, { profile: 'generic/spine-switch-800g', t: 24 });
        for (let i = 1; i <= 4; i++) add(`rail-leaf-${pad(i)}`, 'switch', 1, 1200, { profile: 'nvidia/quantum-x800-ib', t: 24 });
        for (let i = 1; i <= 8; i++) add(`roce-leaf-${pad(i)}`, 'switch', 1, 1100, { profile: 'generic/roce-leaf-400g', t: 24 });
        add('oob-agg', 'switch', 1, 200, { profile: 'generic/oob-switch-1g', t: 24 });
      } else if (f === 'cpu') {
        add('mgmt-switch', 'switch', 1, 150, { profile: 'generic/oob-switch-1g', t: 23 });
        for (let i = 1; i <= 12; i++) add(`server-${pad(i)}`, 'cpu-server', 1, 650, { profile: 'oem/supermicro/sys-121h', cpus: 2, cpuModel: 'Xeon 6 6960P', dialect: 'supermicro', t: 23, bmcModel: 'AST2600' });
      } else {
        add('mgmt-switch', 'switch', 1, 150, { profile: 'generic/oob-switch-1g', t: 23 });
        add('leaf-01', 'switch', 1, 900, { profile: 'generic/leaf-switch-800g', t: 24 }); add('leaf-02', 'switch', 1, 900, { profile: 'generic/leaf-switch-800g', t: 24 });
        const m = { hgx: ['oem/dell/xe9680-h100', 'H100 SXM', 10200, 'idrac'], oam: ['amd/mi355x-8oam', 'MI355X', 9800, 'amd-oam'], gaudi: ['intel/gaudi3-8', 'Gaudi 3', 7600, 'intel'] }[f];
        for (let i = 1; i <= 4; i++) add(`server-${pad(i)}`, 'gpu-server', 6, m[2], { profile: m[0], gpus: 8, gpuModel: m[1], cpus: 2, cpuModel: 'Xeon 8480+', nics: 'ConnectX-7', dialect: m[3], t: 24, bmcModel: f === 'hgx' ? 'iDRAC9' : 'AST2600' });
        add('pdu-a', 'pdu', 0, 0, { zero: true, at: 1, profile: 'generic/pdu-3ph' }); add('pdu-b', 'pdu', 0, 0, { zero: true, at: 14, profile: 'generic/pdu-3ph' });
      }
    }
    devWatts(d) {
      if (d.power !== 'On' || !d.base) return 0;
      let w = d.base * (0.92 + ((hash(d.path + Math.floor(this.sec / 3)) % 100) / 100) * 0.1);
      const dead = d.comps.filter((c) => c.kind === 'accelerator' && c.state === 'offline').length;
      if (dead) w -= (d.base / (d.comps.filter((c) => c.kind === 'accelerator').length + 1)) * dead;
      return w;
    }
    devTemp(d) {
      if (!d.t) return null;
      const loopOff = this.loop.includes(d.rack) ? this.cduS.supplyOff : 0;
      const j = ((hash(d.path + this.sec) % 10) - 5) / 20;
      return d.power === 'On' ? d.t + d.tOff + loopOff + j : d.t - 4 + loopOff;
    }
    rackCap(R) { return R.cap; }
    rackAgg(R) {
      let w = 0, h = 0, flagged = 0, unreach = 0; const temps = [];
      for (const p of [...R.slots, ...R.sensors]) { const d = this.devs[p]; w += this.devWatts(d); h = Math.max(h, SEVN[d.health]); if (d.health !== 'OK') flagged++; if (!d.bmc) unreach++; const t = this.devTemp(d); if (t != null) temps.push(t); }
      for (const v of Object.values(R.flags)) h = Math.max(h, SEVN[v]);
      const temp = temps.length ? temps.reduce((a, b) => a + b, 0) / temps.length : null;
      const health = ['OK', 'Warning', 'Critical'][h];
      let dom = null;
      if (R.f === 'nvl') { const deg = R.slots.filter((p) => { const d = this.devs[p]; return (d.cls === 'compute-tray' && d.comps.some((c) => c.state === 'offline')) || (d.cls === 'switch-tray' && d.health !== 'OK'); }).length; dom = deg ? `Degraded (${deg})` : 'Ready'; }
      return { w, health, temp, flagged, unreach, dom };
    }
    feedShare(n) { return !this.feeds[n].up ? 0 : this.feeds[n === 'A' ? 'B' : 'A'].up ? 0.5 : 1; }
    feedLoad(n, aggs) { return Object.values(aggs).reduce((a, b) => a + b.w, 0) * this.feedShare(n); }
    rackFeedW(R, a, n) { return a.w * this.feedShare(n); }
    rowFeedW(row, n, aggs) { return Object.values(this.racks).filter((r) => r.row === row).reduce((a, r) => a + aggs[r.name].w, 0) * this.feedShare(n); }
    feedBroken(R, n) { return !this.feeds[n].up || (n === 'A' && !!R.flags.pdu) || !!this.rpp[`${R.row}-${n}`]; }
    upsLabel(n) { const U = this.ups[n]; return !this.feeds[n].up ? (U.mode === 'fault' ? 'fault · 0 kW' : 'depleted · 0 kW') : U.mode === 'battery' ? `on battery · ${U.batt} %` : 'online'; }

    // ---------- events ----------
    emit(kind, path, msg, src = 'twin', depth = 0, sev = '') {
      const e = { seq: ++this.seq, t: this.clock(), at: Date.now(), kind, path, msg, src, depth, sev };
      this.events.push(e); if (this.events.length > 400) this.events.shift();
      return e;
    }
    seedHistory() {
      const s0 = this.sec; this.sec -= 95;
      this.emit('twin', 'estate', 'estate hall-a-demo up · 11 racks · 6 packs · clock wall');
      this.emit('stack', 'a03/compute-tray-01', 'nico (sim): machine Ready · serial ' + this.devs['a03/compute-tray-01'].serial, 'stack', 0);
      this.sec += 30; this.emit('request', 'a03/compute-tray-07', 'Redfish GET /redfish/v1/Systems/1 · 200 · 14 ms', 'stack 10.10.0.41', 0);
      this.sec += 20; this.emit('bms', 'facility/cdu-1', 'BMS/v1/PUB/Value/CDU/SupplyTemp = 30.1', 'twin', 0);
      this.sec += 15; this.emit('nos', 'a10/spine-01', 'BGP 20 sessions Established (sonic-vs)', 'nos', 0);
      this.sec = s0;
      for (const p of ['a03/compute-tray-07', 'a03/compute-tray-01', 'a05/server-02']) {
        this.devs[p].reqs.push({ t: '14:00:41', m: 'GET', p: '/redfish/v1/Systems/1', s: '200' }, { t: '14:01:11', m: 'GET', p: '/redfish/v1/Chassis/1/ThermalSubsystem', s: '200' }, { t: '14:01:41', m: 'GET', p: '/redfish/v1/Systems/1', s: '200' });
      }
    }
    pulse(path) { const now = Date.now(); this.pulses[path] = now; const rk = path.split('/')[0]; this.pulses[rk] = now; }
    eventTarget(e) { const p = e.path.split('/').slice(0, 2).join('/'); if (this.devs[p]) return { dev: p }; const r = e.path.split('/')[0]; if (this.racks[r]) return { rack: r }; return null; }

    // ---------- faults ----------
    get active() {
      return this.activeI.map((a) => ({ key: a.id, title: a.F.title, target: a.comp ? `${a.target}/${a.comp}` : a.target, by: a.by, t: a.t, raw: a }));
    }
    faultsFor(d) { return FAULTS.filter((f) => f.on.includes(d.cls)).map((f) => ({ ...f, comps: f.comp ? d.comps.filter((c) => c.kind === 'accelerator').map((c) => c.name) : null })); }
    rackFaults(R) { return RACK_FAULTS.filter((f) => f.on.includes(R.f)); }
    activeOn(d) { return this.activeI.filter((a) => a.target === d.path || (a.target === d.rack && /rack-point|pdu/.test(a.F.id))).length; }
    setv(inst, obj, key, val) { if (!inst.undo.some((u) => u.o === obj && u.k === key)) inst.undo.push({ o: obj, k: key, v: obj[key] }); obj[key] = val; }
    run(inst, steps) {
      const sp = this.speed(); const stackOn = this.stackOn();
      for (const s of steps) {
        if (s.stack && !stackOn) continue;
        const fire = () => { if (inst.cleared) return; s.fn && s.fn(); if (s.msg) this.emit(s.kind, s.path, s.msg, s.src || 'twin', s.d ?? 1, s.sev || ''); if (s.pulse) this.pulse(s.pulse); if (s.req) { const d = this.devs[s.path]; d && d.reqs.push({ t: this.clock(), m: s.req[0], p: s.req[1], s: s.req[2] }); } this.changed(); };
        if (!s.at) fire(); else inst.timers.push(setTimeout(fire, (s.at * 1000) / sp));
      }
    }
    // when: 'now' or seconds from now. Returns a message for the panel, or ''.
    inject(faultId, target, comp, by = 'user:priya', when = 'now') {
      if (when !== 'now') {
        const n = Number(when); this.emit('fault', target, `${faultId} scheduled in ${n} s`, by, 0);
        setTimeout(() => this.inject(faultId, target, comp, by), (n * 1000) / this.speed()); this.changed();
        return `scheduled for ${n} s from now`;
      }
      const F = [...FAULTS, ...RACK_FAULTS].find((f) => f.id === faultId); if (!F) return '';
      const inst = { id: ++this.fid, F, target, comp, by, t: this.clock(), timers: [], undo: [] };
      this.activeI.push(inst);
      const d = this.devs[target], R = this.racks[target.split('/')[0]];
      const tgt = comp ? `${target}/${comp}` : target;
      const S = 'stack 10.10.0.41', RF = '/redfish/v1';
      const st = [{ kind: 'fault', path: tgt, msg: `${F.id} → ${tgt}`, src: by, d: 0, sev: 'crit', pulse: target }];
      const trays = R ? R.slots.filter((p) => /switch-tray/.test(p)) : [];
      switch (F.id) {
        case 'nvidia/xid/xid-79': {
          const c = d.comps.find((x) => x.name === comp) || d.comps[0];
          st.push({ at: 0.4, kind: 'state', path: tgt, msg: 'health OK → Critical · state online → offline · power → 0 W', sev: 'crit', fn: () => { this.setv(inst, c, 'health', 'Critical'); this.setv(inst, c, 'state', 'offline'); this.setv(inst, d, 'health', 'Critical'); } });
          if (d.cls === 'compute-tray') st.push({ at: 0.9, kind: 'state', path: `${R.name}/switch-tray-01…09`, msg: `18 NVLink ports → down · NVLink domain ${R.name} degraded`, sev: 'warn', fn: () => trays.forEach((p) => this.pulse(p)) });
          st.push({ at: 1.3, kind: 'kernel', path: target, msg: d.fid === 'vm' ? 'NVRM: Xid (PCI:0000:47:00.0): 79, pid=*, GPU has fallen off the bus.' : 'kernel line recorded on the model (device is model fidelity)' });
          st.push({ at: 1.6, kind: 'state', path: target, msg: 'Redfish LogEntry (Critical) · Base.1.18.ResourceErrorsDetected' });
          st.push({ at: 3.2, kind: 'stack', path: target, msg: 'nvsentinel (sim): health event gpu-fallen-off-bus', src: 'stack', d: 2, stack: 1 });
          st.push({ at: 4.6, kind: 'stack', path: target, msg: 'nvsentinel (sim): cordon + drain node', src: 'stack', d: 2, stack: 1 });
          st.push({ at: 6.4, kind: 'request', path: target, msg: `Redfish POST ${RF}/Systems/1/Actions/ComputerSystem.Reset {ForceRestart} · 204`, src: S, d: 2, stack: 1, req: ['POST', '/Systems/1/Actions/ComputerSystem.Reset', '204'], fn: () => this.setv(inst, d, 'power', 'PoweringOff') });
          st.push({ at: 7.2, kind: 'state', path: target, msg: 'power On → PoweringOff → Off', d: 2, stack: 1, fn: () => { d.power = 'Off'; } });
          st.push({ at: 9.4, kind: 'state', path: target, msg: 'power Off → On · fault persists across reboot (clears on AC cycle)', d: 2, stack: 1, sev: 'warn', fn: () => { d.power = 'On'; } });
          st.push({ at: 11.5, kind: 'stack', path: target, msg: 'nico (sim): machine → Quarantined · preventAllocations', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        }
        case 'nvidia/xid/xid-48': {
          const c = d.comps.find((x) => x.name === comp) || d.comps[0];
          st.push({ at: 0.4, kind: 'state', path: tgt, msg: 'health OK → Critical · ecc.volatile.dbe 0 → 1', sev: 'crit', fn: () => { this.setv(inst, c, 'health', 'Critical'); this.setv(inst, d, 'health', 'Critical'); } });
          st.push({ at: 1.0, kind: 'kernel', path: target, msg: d.fid === 'vm' ? 'NVRM: Xid (PCI:0000:47:00.0): 48, pid=*, DBE (double bit error) ECC error' : 'kernel line recorded on the model' });
          st.push({ at: 3.0, kind: 'stack', path: target, msg: 'nvsentinel (sim): health event ecc-dbe · requests GPU reset', src: 'stack', d: 2, stack: 1 });
          st.push({ at: 5.0, kind: 'stack', path: target, msg: 'nico (sim): machine → Maintenance', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        }
        case 'generic/leak/tray-detector':
          st.push({ at: 0.4, kind: 'state', path: target, msg: 'LeakDetectors/tray-leak-0 DetectorState OK → Leak · health → Critical', sev: 'crit', fn: () => { this.setv(inst, d, 'leak', true); this.setv(inst, d, 'health', 'Critical'); } });
          st.push({ at: 2.2, kind: 'request', path: target, msg: `Redfish GET ${RF}/Chassis/1/ThermalSubsystem/LeakDetection · 200`, src: S, d: 2, stack: 1, req: ['GET', '/Chassis/1/ThermalSubsystem/LeakDetection', '200'] });
          st.push({ at: 3.6, kind: 'request', path: target, msg: `Redfish POST ${RF}/Systems/1/Actions/ComputerSystem.Reset {GracefulShutdown} · power.off.graceful`, src: S, d: 2, stack: 1, req: ['POST', '/Systems/1/Actions/ComputerSystem.Reset', '204'], fn: () => this.setv(inst, d, 'power', 'PoweringOff') });
          st.push({ at: 5.6, kind: 'state', path: target, msg: 'power PoweringOff → Off · 0 W', d: 2, stack: 1, fn: () => { d.power = 'Off'; } });
          st.push({ at: 6.4, kind: 'stack', path: target, msg: 'nico (sim): alert TrayLeak · machine → Maintenance', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        case 'generic/bmc/unreachable':
          st.push({ at: 0.4, kind: 'state', path: target, msg: 'BMC reachable → false (host unaffected)', sev: 'warn', fn: () => { this.setv(inst, d, 'bmc', false); this.setv(inst, d, 'health', d.health === 'Critical' ? 'Critical' : 'Warning'); } });
          st.push({ at: 3.0, kind: 'request', path: target, msg: `Redfish GET ${RF}/Systems/1 · timeout 30 s`, src: S, d: 2, stack: 1, sev: 'warn', req: ['GET', '/Systems/1', 'tmo'] });
          st.push({ at: 6.0, kind: 'stack', path: target, msg: 'nico (sim): machine → BmcUnreachable · retry backoff 60 s', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        case 'generic/bmc/slow':
          st.push({ at: 0.4, kind: 'state', path: target, msg: 'BMC latency 14 ms → 6.2 s · 503 rate 20 %', sev: 'warn', fn: () => { this.setv(inst, d, 'bmcSlow', true); this.setv(inst, d, 'health', d.health === 'OK' ? 'Warning' : d.health); } });
          st.push({ at: 2.4, kind: 'request', path: target, msg: `Redfish GET ${RF}/Systems/1 · 503 · 6.2 s`, src: S, d: 2, stack: 1, sev: 'warn', req: ['GET', '/Systems/1', '503'] });
          break;
        case 'generic/power/psu-failure':
          st.push({ at: 0.4, kind: 'state', path: target, msg: 'psu-2 output → 0 W · health → Warning', sev: 'warn', fn: () => this.setv(inst, d, 'health', 'Warning') });
          st.push({ at: 0.9, kind: 'state', path: target, msg: 'power.loss evaluated: redundancy N+1 → N · nothing powered off' });
          st.push({ at: 3.0, kind: 'request', path: target, msg: `Redfish GET ${RF}/Chassis/1/PowerSubsystem/PowerSupplies · 200`, src: S, d: 2, stack: 1, req: ['GET', '/Chassis/1/PowerSubsystem/PowerSupplies', '200'] });
          break;
        case 'generic/thermal/fan-failure':
          st.push({ at: 0.4, kind: 'state', path: target, msg: 'fan-3 → 0 RPM · health → Warning', sev: 'warn', fn: () => { this.setv(inst, d, 'health', 'Warning'); this.setv(inst, d, 'tOff', 8); } });
          st.push({ at: 4.0, kind: 'stack', path: target, msg: 'fleet-intelligence (sim): thermal anomaly on node', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        case 'generic/link/cable-pulled': {
          const lid = this.linkFor(target);
          st.push({ at: 0.3, kind: 'state', path: target, msg: `link ${lid || 'uplink'} → down (both ends) · LLDP neighbour lost`, sev: 'warn', fn: () => { if (lid) this.pulled.add(lid); inst.undo.push({ fn: () => lid && this.pulled.delete(lid) }); this.setv(inst, d, 'health', 'Warning'); } });
          st.push({ at: 2.2, kind: 'nos', path: target, msg: 'BGP session to peer → Idle (sonic-vs)', src: 'nos', sev: 'warn' });
          st.push({ at: 4.0, kind: 'stack', path: target, msg: 'nico (sim): network health → link down on uplink', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        }
        case 'generic/firmware/drift':
          st.push({ at: 0.4, kind: 'state', path: target, msg: 'BMC firmware 25.06.2 → 25.04.1 (baseline 25.06.2)', sev: 'warn', fn: () => { this.setv(inst, d, 'fw', 'drifted'); this.setv(inst, d, 'health', d.health === 'OK' ? 'Warning' : d.health); } });
          st.push({ at: 3.0, kind: 'stack', path: target, msg: 'nico (sim): firmware compliance → out of baseline', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        case 'generic/leak/rack-point': {
          const ls = this.devs[`${R.name}/leak-sensor-02`];
          st[0].path = `${R.name}/leak-sensor-02`; st[0].msg = `${F.id} → ${R.name}/leak-sensor-02`;
          st.push({ at: 0.4, kind: 'state', path: ls.path, msg: 'leak point OK → Leak', sev: 'warn', fn: () => { this.setv(inst, ls, 'leak', true); this.setv(inst, ls, 'health', 'Warning'); } });
          st.push({ at: 0.9, kind: 'bms', path: 'facility/bms', msg: `BMS/v1/PUB/Value/Rack/RackLeakDetect/${R.name} = true`, fn: () => { this.bms[`Rack/RackLeakDetect/${R.name}`] = 'true'; inst.undo.push({ fn: () => delete this.bms[`Rack/RackLeakDetect/${R.name}`] }); } });
          st.push({ at: 2.6, kind: 'stack', path: R.name, msg: 'nico (sim): alert RackLeak · preventAllocations: true · nothing powered off', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
        }
        case 'generic/power/pdu-outlet-off':
          st[0].path = `${R.name}/pdu-a`;
          st.push({ at: 0.4, kind: 'state', path: `${R.name}/pdu-a`, msg: 'outlets → off · feed A to rack 0 W', sev: 'warn', fn: () => this.setv(inst, R.flags, 'pdu', 'Warning') });
          st.push({ at: 0.9, kind: 'state', path: R.name, msg: 'power.loss evaluated: every device has a B source · PSU redundancy lost' });
          st.push({ at: 3.0, kind: 'stack', path: R.name, msg: 'nico (sim): rack power redundancy alert', src: 'stack', d: 2, stack: 1, sev: 'warn' });
          break;
      }
      this.run(inst, st);
      this.changed();
      return '';
    }
    clear(a) { this.clearInst(a.raw || a); this.changed(); }
    clearInst(inst, why = 'cleared by user:priya') {
      inst.cleared = true; inst.timers.forEach(clearTimeout);
      for (const u of inst.undo.slice().reverse()) { if (u.fn) u.fn(); else u.o[u.k] = u.v; }
      const d = this.devs[inst.target]; if (d && d.power !== 'On' && !this.activeI.some((a) => a !== inst && a.target === inst.target)) d.power = 'On';
      this.activeI = this.activeI.filter((a) => a !== inst);
      this.emit('fault', inst.target, `${inst.F.id} cleared (${why})`, 'user:priya', 0, '');
      this.pulse(inst.target);
    }
    has(id) { return this.activeI.some((x) => x.F.id === id); }

    // ---------- facility ----------
    rackFeedFlags(inst, pred, key) { Object.values(this.racks).filter(pred).forEach((r) => { this.setv(inst, r.flags, key, 'Warning'); this.pulse(r.name); }); }
    powerFault(kind) {
      const [type, a, b] = kind.split('-');
      const id = type === 'utility' ? `generic/power/utility-loss · feed ${a}` : type === 'ups' ? `generic/power/ups-failure · ups-${a}` : `generic/power/breaker-trip · rpp-${a} ${b}`;
      const ex = this.activeI.find((x) => x.F.id === id); if (ex) { this.clearInst(ex, 'restored from floor'); return; }
      const inst = { id: ++this.fid, F: { id, title: type === 'utility' ? `Utility loss · feed ${a}` : type === 'ups' ? `UPS failure · ups-${a}` : `Breaker trip · rpp-${a} feed ${b}` }, target: `facility/${type === 'rpp' ? 'rpp-' + a : type + '-' + a}`, by: 'user:priya', t: this.clock(), timers: [], undo: [] };
      this.activeI.push(inst);
      const other = (n) => (n === 'A' ? 'B' : 'A');
      const lose = (n) => () => { this.setv(inst, this.feeds[n], 'up', false); this.rackFeedFlags(inst, () => true, 'feed' + n); };
      if (type === 'utility') {
        const n = a, U = this.ups[n];
        this.run(inst, [
          { kind: 'fault', path: `facility/swgr-${n}`, msg: `generic/power/utility-loss → swgr-${n}`, src: 'user:priya', d: 0, sev: 'crit', fn: () => { this.setv(inst, this.utility[n], 'up', false); this.setv(inst, U, 'mode', 'battery'); this.setv(inst, U, 'batt', 100); } },
          { at: 0.5, kind: 'state', path: `facility/ups-${n}`, msg: `ups-${n} → on battery · runtime 16 s (shortened for the demo)`, sev: 'warn' },
          { at: 0.9, kind: 'bms', path: 'facility/bms', msg: `BMS/v1/PUB/Value/UPS/ups-${n}/OnBattery = true` },
          { at: 2.5, kind: 'stack', path: 'facility', msg: `dsx-exchange (sim): grid event · feed ${n} on UPS · cap power 85 %`, src: 'stack', d: 2, stack: 1, sev: 'warn' },
          { at: 4, kind: 'state', path: `facility/ups-${n}`, msg: 'battery 75 %', fn: () => { U.batt = 75; } },
          { at: 8, kind: 'state', path: `facility/ups-${n}`, msg: 'battery 50 %', fn: () => { U.batt = 50; } },
          { at: 12, kind: 'state', path: `facility/ups-${n}`, msg: 'battery 25 % · low-battery alarm', sev: 'warn', fn: () => { U.batt = 25; } },
          { at: 16, kind: 'state', path: `facility/feed-${n}`, msg: `ups-${n} depleted · feed ${n} lost · every rack on feed ${other(n)} only · nothing powered off`, sev: 'crit', fn: () => { U.batt = 0; U.mode = 'depleted'; lose(n)(); } },
          { at: 17, kind: 'bms', path: 'facility/bms', msg: `BMS/v1/PUB/Value/Feed/${n}/Status = Lost` },
        ]);
      }
      if (type === 'ups') {
        const n = a;
        this.run(inst, [
          { kind: 'fault', path: `facility/ups-${n}`, msg: `generic/power/ups-failure → ups-${n}`, src: 'user:priya', d: 0, sev: 'crit', fn: () => { this.setv(inst, this.ups[n], 'mode', 'fault'); lose(n)(); } },
          { at: 0.5, kind: 'state', path: `facility/feed-${n}`, msg: `feed ${n} lost (no static bypass) · power.loss evaluated: racks on feed ${other(n)} only`, sev: 'crit' },
          { at: 1, kind: 'bms', path: 'facility/bms', msg: `BMS/v1/PUB/Alarm/UPS/ups-${n}/Fault = true` },
          { at: 3, kind: 'stack', path: 'facility', msg: 'dsx-exchange (sim): feed redundancy lost · alert to nico', src: 'stack', d: 2, stack: 1, sev: 'warn' },
        ]);
      }
      if (type === 'rpp') {
        const row = a, n = b, key = `${row}-${n}`;
        this.run(inst, [
          { kind: 'fault', path: `facility/rpp-${row}`, msg: `generic/power/breaker-trip → rpp-${row} feed ${n}`, src: 'user:priya', d: 0, sev: 'crit', fn: () => { this.setv(inst, this.rpp, key, true); this.rackFeedFlags(inst, (r) => r.row === row, 'rpp' + n); } },
          { at: 0.5, kind: 'state', path: `row ${row}`, msg: `busway ${row}-${n} → 0 kW · racks in row ${row} on feed ${other(n)} only`, sev: 'warn' },
          { at: 2.5, kind: 'request', path: `row ${row}`, msg: 'Redfish GET /redfish/v1/Chassis/1/PowerSubsystem · PSU input lost (×N racks)', src: 'stack 10.10.0.41', d: 2, stack: 1 },
          { at: 4, kind: 'stack', path: `row ${row}`, msg: `nico (sim): row ${row} power redundancy alert`, src: 'stack', d: 2, stack: 1, sev: 'warn' },
        ]);
      }
    }
    facility(kind) {
      const inst = { id: ++this.fid, F: { id: kind, title: kind }, target: 'facility', by: 'user:priya', t: this.clock(), timers: [], undo: [] };
      if (kind === 'feed-A' || kind === 'feed-B') {
        const n = kind.slice(-1), f = this.feeds[n];
        if (!f.up) { const a = this.activeI.find((x) => x.F.id === `generic/power/feed-loss · feed ${n}`); if (a) this.clearInst(a, 'feed restored'); this.changed(); return; }
        inst.F = { id: `generic/power/feed-loss · feed ${n}`, title: `Feed ${n} loss` }; this.activeI.push(inst);
        this.run(inst, [
          { kind: 'fault', path: `facility/feed-${n}`, msg: `generic/power/feed-loss → feed ${n}`, src: 'user:priya', d: 0, sev: 'crit', fn: () => { this.setv(inst, f, 'up', false); Object.values(this.racks).forEach((r) => { this.setv(inst, r.flags, 'feed', 'Warning'); this.pulse(r.name); }); } },
          { at: 0.6, kind: 'state', path: 'facility', msg: `power.loss evaluated: all racks on feed ${n === 'A' ? 'B' : 'A'} only · nothing powered off`, sev: 'warn' },
          { at: 1.2, kind: 'bms', path: 'facility/bms', msg: `BMS/v1/PUB/Value/Feed/${n}/Status = Lost` },
          { at: 3.0, kind: 'stack', path: 'facility', msg: 'dsx-exchange (sim): grid event · power capping requested 85 %', src: 'stack', d: 2, stack: 1, sev: 'warn' },
        ]);
      }
      if (/^(utility|ups|rpp)-/.test(kind)) { this.powerFault(kind); this.changed(); return; }
      if (kind === 'cdu') {
        if (this.cduS.state !== 'OK') { const a = this.activeI.find((x) => x.F.id === 'generic/thermal/cdu-flow-loss'); if (a) this.clearInst(a, 'flow restored'); this.changed(); return; }
        inst.F = { id: 'generic/thermal/cdu-flow-loss', title: 'CDU flow loss' }; inst.target = 'facility/cdu-1'; this.activeI.push(inst);
        const heat = (v) => () => { this.setv(inst, this.cduS, 'supplyOff', v); };
        const warnTrays = () => this.loop.forEach((r) => this.racks[r].slots.filter((p) => /compute-tray/.test(p)).forEach((p) => { const d = this.devs[p]; this.setv(inst, d, 'health', d.health === 'OK' ? 'Warning' : d.health); }));
        this.run(inst, [
          { kind: 'fault', path: 'facility/cdu-1', msg: 'generic/thermal/cdu-flow-loss → cdu-1', src: 'user:priya', d: 0, sev: 'crit', fn: () => { this.setv(inst, this.cduS, 'flow', 140); this.setv(inst, this.cduS, 'state', 'Critical'); this.loop.forEach((r) => this.pulse(r)); } },
          { at: 1, kind: 'state', path: 'facility/cdu-1', msg: 'flow 1200 → 140 L/min · supply rising', sev: 'crit', fn: heat(4) },
          { at: 3, kind: 'state', path: 'a01…a04', msg: 'coolant-in 30 → 38 °C', fn: heat(8) },
          { at: 5, kind: 'state', path: 'a01…a04/compute-tray-*', msg: 'coolant-in > 45 °C warn threshold · 72 trays → Warning', sev: 'warn', fn: () => { heat(16)(); warnTrays(); } },
          { at: 5.6, kind: 'bms', path: 'facility/bms', msg: 'BMS/v1/PUB/Alarm/CDU/cdu-1/FlowLow = true' },
          { at: 7.5, kind: 'stack', path: 'a01…a04', msg: 'nico (sim): thermal alert · drain loop cdu-1 racks', src: 'stack', d: 2, stack: 1, sev: 'warn' },
        ]);
      }
      if (kind === 'rack-leak') { this.inject('generic/leak/rack-point', this.loop[2] || this.loop[0], null); return; }
      this.changed();
    }
    facilityActs() {
      return [{ label: this.cduS.state === 'OK' ? 'CDU flow loss' : 'Restore CDU flow', kind: 'cdu' }, { label: 'Rack leak point a03', kind: 'rack-leak' }];
    }
    bmsPoints() {
      return [['BMS/v1/PUB/Value/CDU/cdu-1/SupplyTemp', (30.1 + this.cduS.supplyOff).toFixed(1)], ['BMS/v1/PUB/Value/CDU/cdu-1/Flow', String(this.cduS.flow)], ['BMS/v1/PUB/Value/Feed/A/Status', this.feeds.A.up ? 'Normal' : 'Lost'], ['BMS/v1/PUB/Value/Feed/B/Status', this.feeds.B.up ? 'Normal' : 'Lost'],
        ...Object.entries(this.bms).map(([k, v]) => [`BMS/v1/PUB/Value/${k}`, v])];
    }
    act(kind, path) {
      const d = this.devs[path]; if (!d) return;
      const S = 'user:priya'; const inst = { timers: [], undo: [], cleared: false };
      if (kind === 'off') { this.emit('act', d.path, 'power off at PDU outlet', S, 0); d.power = 'Off'; this.pulse(d.path); }
      if (kind === 'on') { this.emit('act', d.path, 'power on at PDU outlet', S, 0); d.power = 'On'; }
      if (kind === 'ac') {
        this.emit('act', d.path, 'AC cycle', S, 0); d.power = 'Off'; this.pulse(d.path);
        this.run(inst, [{ at: 1.5, kind: 'state', path: d.path, msg: 'AC restored · power Off → On · BMC booting', fn: () => { d.power = 'On'; this.activeI.filter((a) => a.target === d.path && /xid-79|bmc\/unreachable/.test(a.F.id)).forEach((a) => this.clearInst(a, 'clears on AC cycle')); } }]);
      }
      if (kind === 'reseat') { this.emit('act', d.path, 'reseat', S, 0); d.power = 'Off'; this.run(inst, [{ at: 2, kind: 'state', path: d.path, msg: 'reseated · power On · re-enumerated', fn: () => { d.power = 'On'; } }]); }
      if (kind === 'bmc') { this.emit('act', d.path, 'BMC reset', S, 0); this.run(inst, [{ at: 2, kind: 'state', path: d.path, msg: 'BMC back · Redfish serving', fn: () => this.activeI.filter((a) => a.target === d.path && /bmc\//.test(a.F.id)).forEach((a) => this.clearInst(a, 'BMC reset')) }]); }
      this.changed();
    }
    setChaos(on, rate) {
      if (on !== this.chaos.on) this.emit('act', 'estate', `chaos ${on ? 'enabled' : 'disabled'} · ${rate}/min · seed 1729`, 'user:priya', 0);
      this.chaos = { on, rate }; this.changed();
    }

    // ---------- topology ----------
    topoModel() {
      const racks = Object.values(this.racks).filter((r) => r.f !== 'net').sort((a, b) => (a.row + a.pos).localeCompare(b.row + b.pos, undefined, { numeric: true }));
      const n = racks.length, W = 960, x0 = 80, span = W - x0 - 30;
      const nodes = [], links = [];
      const sp = [{ id: 'a10/spine-01', x: x0 + span * 0.3 }, { id: 'a10/spine-02', x: x0 + span * 0.7 }];
      sp.forEach((s) => nodes.push({ id: s.id, label: s.id.split('/')[1], x: s.x, y: 40, w: 84 }));
      racks.forEach((r, i) => {
        const cx = x0 + (span / n) * (i + 0.5);
        const rh = this.rackAgg(r).health;
        nodes.push({ id: r.name, label: r.name, x: cx, y: 300, w: 46, health: rh });
        [1, 2].forEach((k) => {
          const lid = `leaf-${r.name}-${k}`, lx = cx + (k === 1 ? -20 : 20);
          nodes.push({ id: lid, label: `${r.name}·${k}`, x: lx, y: 170, w: 38 });
          sp.forEach((s) => links.push({ id: `${s.id.split('/')[1]}<->${lid}`, x1: s.x, y1: 51, x2: lx, y2: 159 }));
          links.push({ id: `${lid}<->${r.name}`, x1: lx, y1: 181, x2: cx, y2: 289 });
        });
      });
      for (const l of links) l.down = this.pulled.has(l.id);
      for (const nd of nodes) nd.down = [...this.pulled].some((id) => id.includes(nd.id.split('/').pop()));
      return { nodes, links, title: 'frontend · clos-2tier · sonic-vs · click a link to pull its cable', pulled: [...this.pulled].map((id) => ({ id, label: id })) };
    }
    rackLinkDown(R) { return [...this.pulled].some((id) => id.includes(`leaf-${R.name}-`)); }
    // The hall's cabled fabrics: every rack's BMCs on OOB and hosts on the frontend, NVL72 trays
    // on InfiniBand rails and 8-GPU servers on RoCE rails, all switched in the network rack.
    fabrics() {
      const all = Object.keys(this.racks), net = this.racks[this.netRack] ? this.netRack : null;
      const of = (fs) => new Set([...all.filter((n) => fs.includes(this.racks[n].f)), ...(net ? [net] : [])]);
      return TC.data.styleFabrics([
        { name: 'oob', racks: new Set(all), hub: net, down: new Set() },
        { name: 'frontend', racks: new Set(all), hub: net, down: new Set(all.filter((n) => this.rackLinkDown(this.racks[n]))) },
        { name: 'backend-ib', racks: of(['nvl']), hub: net, down: new Set() },
        { name: 'backend-roce', racks: of(['hgx', 'oam', 'gaudi']), hub: net, down: new Set() },
      ]);
    }
    linkFor(path) {
      const d = this.devs[path]; if (!d) return null;
      if (/spine-0(\d)/.test(path)) return `spine-0${path.match(/spine-0(\d)/)[1]}<->leaf-a03-1`;
      if (/leaf-0(\d)$/.test(path)) return `leaf-${d.rack}-${path.slice(-1)}<->${d.rack}`;
      return `leaf-${d.rack === 'a10' ? 'a03' : d.rack}-1<->${d.rack === 'a10' ? 'a03' : d.rack}`;
    }
    pullLink(id) {
      if (this.pulled.has(id)) { this.pulled.delete(id); this.emit('act', id, 'cable replaced · link → up · LLDP neighbour restored', 'user:priya', 0); }
      else { this.pulled.add(id); this.emit('fault', id, `generic/link/cable-pulled → ${id}`, 'user:priya', 0, 'crit'); this.emit('nos', id, 'oper-status down both ends · BGP peer → Idle', 'nos', 1, 'warn'); }
      this.changed();
    }

    // ---------- scenarios ----------
    scenarios() {
      return {
        'nvl72-leak-response': { desc: 'Tray leak shuts one tray; rack leak blocks allocation', req: 'requires nvidia/gb300-nvl72 · facility/bms:nvidia/dsx-exchange · stack nico, dsx-exchange',
          steps: [
            { kind: 'wait', text: 'all a03/compute-tray-* Ready (within 20m)', dur: 1.5 },
            { kind: 'fault', text: 'inject generic/leak/tray-detector → a03/compute-tray-07', dur: 0.5, run: () => this.inject('generic/leak/tray-detector', 'a03/compute-tray-07', null, 'scenario') },
            { kind: 'expect', text: 'within 3m', dur: 7, asserts: [
              ['redfish a03/compute-tray-07 LeakDetectors/*/DetectorState == Leak', () => this.devs['a03/compute-tray-07'].leak],
              ['event power.off.graceful from stack on a03/compute-tray-07', () => this.devs['a03/compute-tray-07'].reqs.some((r) => /Reset/.test(r.p))],
              ['redfish Systems/*/PowerState == "Off"', () => this.devs['a03/compute-tray-07'].power === 'Off'],
              ['not power.off.graceful on a03/compute-tray-{01..06,08..18}', () => true]] },
            { kind: 'fault', text: 'inject generic/leak/rack-point → a03/leak-sensor-02', dur: 0.5, run: () => this.inject('generic/leak/rack-point', 'a03', null, 'scenario') },
            { kind: 'expect', text: 'within 2m', dur: 3.5, asserts: [
              ['bms BMS/v1/PUB/Value/Rack/RackLeakDetect/# == true', () => !!this.bms['Rack/RackLeakDetect/a03']],
              ['accessor alerts(rack: a03) == {kind: RackLeak, preventAllocations: true}', () => this.stackOn()],
              ['not power.off.graceful on a03/compute-tray-* (beyond path 1)', () => true]] },
            { kind: 'fault', text: 'clear tray · clear rack', dur: 0.5, run: () => this.activeI.filter((a) => a.by === 'scenario').forEach((a) => this.clearInst(a, 'scenario step')) },
          ] },
        'tray-leak-isolation': { desc: 'A tray leak powers off only that tray', req: 'requires nvidia/gb300-nvl72 · stack nico',
          steps: [
            { kind: 'fault', text: 'inject generic/leak/tray-detector → a01/compute-tray-12', dur: 0.5, run: () => this.inject('generic/leak/tray-detector', 'a01/compute-tray-12', null, 'scenario') },
            { kind: 'expect', text: 'within 3m', dur: 7, asserts: [['redfish a01/compute-tray-12 PowerState == "Off"', () => this.devs['a01/compute-tray-12'].power === 'Off'], ['not power.off.graceful on a01/compute-tray-{01..11,13..18}', () => true]] },
            { kind: 'fault', text: 'clear tray', dur: 0.5, run: () => this.activeI.filter((a) => a.by === 'scenario').forEach((a) => this.clearInst(a, 'scenario step')) },
          ] },
        'bmc-discovery': { desc: 'Stack discovers every BMC over DHCP and reads inventory', req: 'requires dhcp/relay-option-82 · stack nico',
          steps: [
            { kind: 'act', text: 'AC cycle a05/server-01 … 04', dur: 1 },
            { kind: 'expect', text: 'within 10m', dur: 4, asserts: [['dhcp lease for every BMC MAC in a05', () => true], ['event inventory.read from stack on a05/server-*', () => true], ['accessor machine(serial).state == "Discovered"', () => this.stackOn()]] },
          ] },
      };
    }
    scenarioReport() {
      return ['devices used: 18 compute trays (2 vm, 16 model) · 9 switch trays (model) · 3 leak sensors', 'not modelled: cold-plate flow; PCIe link-state artefacts of Xid 79', 'impersonated: none · stubbed: NRAS attestation', 'twin incidents during run: 0 · projection lag max 61 ms',
        this.stackOn() ? 'stack: simulated (nvidia-dsx-os stand-in) — not a conformance pass' : 'stack: responses disabled — expect failures'];
    }
    runScenario(name) {
      const sc = this.scenarios()[name]; if (!sc || this.scenRun) return;
      const r = { name, step: 0, res: sc.steps.map(() => ({ st: 'pending', a: [] })) }; this.scenRun = r; this.scenLast = r; this.scenRes[name] = 'running';
      this.emit('twin', `scenario/${name}`, 'scenario started', 'scenario', 0);
      const sp = this.speed(); let t = 0;
      sc.steps.forEach((s, i) => {
        setTimeout(() => { r.step = i; r.res[i].st = 'running'; s.run && s.run(); this.changed(); }, (t * 1000) / sp);
        t += s.dur;
        setTimeout(() => { r.res[i].a = (s.asserts || []).map(([, fn]) => !!fn()); r.res[i].st = r.res[i].a.every(Boolean) ? 'pass' : 'fail'; this.changed(); }, (t * 1000) / sp);
      });
      setTimeout(() => { const ok = r.res.every((x) => x.st === 'pass'); this.scenRes[name] = ok ? 'passed' : 'failed'; this.emit('twin', `scenario/${name}`, `scenario ${ok ? 'passed' : 'failed'} · fidelity report attached`, 'scenario', 0, ok ? '' : 'crit'); this.scenRun = null; this.changed(); }, ((t + 0.3) * 1000) / sp);
      this.changed();
    }

    // ---------- design ----------
    designFormats() { return [['nvidia/gb300-nvl72', '72 GPU · liquid · 132 kW'], ['generic/hgx-8gpu', '4× 8-GPU servers · air'], ['generic/oam-8gpu', '4× MI355X · air'], ['generic/cpu-rack', '12× 1U CPU servers']]; }
    formatTagOf(fmt) { return FMT[FORMAT_IDS[fmt]] ? FMT[FORMAT_IDS[fmt]].tag : fmt; }
    applyDraft(draft, log, done) {
      const adds = draft.filter((d) => d.op !== 'remove'), removes = draft.filter((d) => d.op === 'remove');
      const steps = ['validate ✓ schema ✓ packs ✓ cabling', 'diff accepted · etag 7f3a2c',
        ...removes.flatMap((d) => [`${d.name}: powered off · unplugged`, `${d.name}: cables removed · de-racked`]),
        ...adds.flatMap((d) => [`${d.name}: racked (${d.fmt})`, `${d.name}: cabled · OOB + frontend leaves`, `${d.name}: powered · BMCs on standby`]), ...(adds.length ? ['stack discovers BMCs over DHCP'] : [])];
      steps.forEach((s, i) => setTimeout(() => {
        log(s);
        if (i === steps.length - 1) {
          removes.forEach((d) => this.removeRack(d.name));
          adds.forEach((d) => { this.addRack(d.name, d.row, d.pos, FORMAT_IDS[d.fmt]); this.emit('state', d.name, `racked · cabled · powered (${d.fmt})`, 'user:priya', 0); this.emit('request', d.name, 'DHCP DISCOVER from BMC 3c:ec:ef:… · lease 10.10.x.x', 'stack', 1); this.pulse(d.name); });
          done(); this.changed();
        }
      }, (i + 1) * 550));
    }

    removeRack(name) {
      const R = this.racks[name]; if (!R) return;
      for (const p of [...R.slots, ...R.sensors]) delete this.devs[p];
      delete this.racks[name];
      this.loop = this.loop.filter((n) => n !== name);
      for (const a of this.activeI.filter((x) => x.target.split('/')[0] === name)) this.clearInst(a, 'rack removed');
      this.pulled = new Set([...this.pulled].filter((id) => !id.includes(`leaf-${name}-`) && !id.endsWith(name)));
      this.emit('state', name, 'powered off · unplugged · cables removed · de-racked', 'user:priya', 0);
    }
    tick() {
      this.sec += 1;
      if (this.sec % 6 === 0) {
        const ps = Object.keys(this.devs).filter((p) => this.devs[p].dialect); const p = ps[hash('r' + this.sec) % ps.length]; const d = this.devs[p];
        if (d.bmc) { d.reqs.push({ t: this.clock(), m: 'GET', p: '/redfish/v1/Systems/1', s: d.bmcSlow ? '503' : '200' }); if (d.reqs.length > 30) d.reqs.shift(); this.emit('read', p, `Redfish GET /redfish/v1/Systems/1 · ${d.bmcSlow ? '503' : '200'} · ${8 + (hash(p) % 20)} ms`, 'stack 10.10.0.41', 0); }
      }
      if (this.chaos.on && hash('c' + this.sec) % 60 < this.chaos.rate) {
        const cands = Object.values(this.devs).filter((d) => ['compute-tray', 'gpu-server', 'cpu-server', 'switch-tray'].includes(d.cls) && d.health === 'OK');
        const d = cands[hash('d' + this.sec) % cands.length];
        const fs = FAULTS.filter((f) => f.on.includes(d.cls) && f.id !== 'generic/link/cable-pulled'); const f = fs[hash('f' + this.sec) % fs.length];
        this.inject(f.id, d.path, f.comp ? `gpu-${hash('g' + this.sec) % 4}` : null, 'chaos (seed 1729)');
      }
      this.changed();
    }

    defaultSelection() { return { rack: 'a03', dev: 'a03/compute-tray-07' }; }
    reqsOf(D) { return D.reqs; }
    reqCount(D) { return D.reqs.length; }

    // The Redfish document the stack would read, for the Interfaces tab.
    redfish(D) {
      return D.dialect ? JSON.stringify({ '@odata.id': '/redfish/v1/Systems/1', Id: '1', Manufacturer: { 'nvidia-gb': 'NVIDIA', idrac: 'Dell Inc.', supermicro: 'Supermicro', 'amd-oam': 'AMD', intel: 'Intel' }[D.dialect], SerialNumber: D.serial, PowerState: D.power, Status: { State: D.power === 'On' ? 'Enabled' : 'StandbyOffline', Health: D.health }, ProcessorSummary: { Count: D.comps.filter((c) => c.kind === 'cpu').length } }, null, 2) : '(no BMC — this device publishes over BMS / SNMP)';
    }
  }
  return Demo;
})();
