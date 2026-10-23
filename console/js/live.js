// live.js — the console as a client of a running twin's control API (/v1, D16). Everything shown
// comes from the API: GET /v1/layout, /devices, /facility, /links, /domains, /faults, /catalogue,
// /report and the event stream. Every act is an API call attributed to the control API; the
// console holds a cache of what the API returned and simulates nothing.
//
// What the design draws and the twin does not model is left out rather than invented: there is
// no UPS, switchgear or remote power panel in an estate (feeds are drawn instead), no estate
// hot apply (Design applies by restarting the estate on the changed file), and no read of
// chaos state.
window.TC = window.TC || {};
TC.Live = (() => {
  const { formatKey, formatTag, fmtClock, SEVN } = TC.data;
  const STREAM_LIMIT = 3000;

  const fmtV = (v) => (typeof v === 'number' ? (Number.isInteger(v) ? String(v) : v.toFixed(Math.abs(v) < 10 ? 2 : 1)) : v === undefined || v === null ? '∅' : typeof v === 'object' ? JSON.stringify(v) : String(v));
  const isRead = (e) => !!(e.request && /\.read$/.test(e.request.canonical || ''));
  const evSrc = (e) => { const s = e.source || {}; return [s.class, s.principal || s.address].filter(Boolean).join(':'); };
  function evMsg(e) {
    if (e.request) { const q = e.request; return `${q.protocol || ''} ${q.method || ''} ${q.target || ''}${q.status ? ' → ' + q.status : ''} · ${q.canonical}`.trim(); }
    if (e.state) { const s = e.state; const m = `${s.field}: ${s.from !== undefined ? fmtV(s.from) + ' → ' : ''}${fmtV(s.to)}`; return e.message ? `${e.message} · ${m}` : m; }
    return e.message || (e.data ? JSON.stringify(e.data) : '');
  }
  function clearsText(c) { if (!c) return 'manually'; const p = []; const on = c.on || c.true; if (on && on.length) p.push('on ' + on.join(', ')); if (c.after) p.push('after ' + c.after); if (c.manual) p.push('manually'); return p.join(', or ') || '—'; }
  function targetText(t) { t = t || {}; return t.port ? 'a port' : t.component ? `component ${t.component}${t.vendor ? ' (' + t.vendor + ')' : ''}` : t.device ? `device class ${t.device}` : t.deviceWith ? `a device with ${t.deviceWith}` : 'any device'; }
  // Which catalogue faults fit a device, by the target rules the catalogue publishes. The twin
  // also applies each profile's allow-list, which the API does not serve; a refused inject says why.
  function fitsDevice(t, dv) {
    t = t || {};
    if (t.port || t.component) return false;
    if (t.device) return dv.class === t.device;
    if (t.deviceWith === 'bmc') return !!dv.dialect;
    if (t.deviceWith === 'firmware') return Object.keys(dv.firmware || {}).length > 0;
    if (t.deviceWith === 'leak-detector') return !!dv.sensors && 'leak' in dv.sensors && !['sensor', 'cdu'].includes(dv.class);
    return !t.deviceWith;
  }
  const fitsComp = (t, c) => !!t && !!t.component && c.kind === t.component && (!t.vendor || c.vendor === t.vendor);

  class Live {
    constructor(token) {
      this.kind = 'live'; this.token = token;
      this.caps = { infra: false, design: false, schedule: true, scenarios: 'live', chaosUnit: '/device/h', topology: true };
      this.onChange = () => {};
      this.status = null; this.statusAt = 0; this.rtt = 0; this.down = false; this.stream = 'connecting';
      this.layout = { halls: [] }; this.catalogue = { faults: [], formats: [] }; this.domains = []; this.faultViews = []; this.fac = { feeds: [], loops: [], racks: [] }; this.links = [];
      this.views = {}; this.rows = []; this.racks = {}; this.devs = {}; this.byRack = {}; this.rackByPath = {};
      this.events = []; this.seen = new Set(); this.depth = new Map(); this.lastSeq = 0; this.es = null;
      this.pulses = {}; this.pulled = new Set(); this.loop = []; this.cdu = null; this.cdus = []; this.feeds = {}; this.netRack = null;
      this.chaos = { on: false, rate: 2 }; this.reqs = {}; this.rf = {}; this.ports = {}; this.isNot = {}; this.reads = false;
      this.scn = { running: false, result: null, err: null }; this.example = EXAMPLE; this.scnText = EXAMPLE; this.adapters = [];
      this.estate = { name: '…', seed: null, packs: null, hall: 'HALL', hallName: 'hall', site: 'site' };
      this.dirty = { devs: new Set(), all: false, faults: false, domains: false, facility: false, links: false }; this.flushT = null;
    }

    // ---------------------------------------------------------------- the API
    async api(method, path, body, type) {
      const opt = { method, headers: { Authorization: 'Bearer ' + this.token } };
      if (body !== undefined) { opt.headers['Content-Type'] = type === 'yaml' ? 'application/yaml' : 'application/json'; opt.body = type === 'yaml' ? body : JSON.stringify(body); }
      const r = await fetch('/v1' + path, opt);
      const ct = r.headers.get('Content-Type') || '';
      const data = ct.includes('json') ? await r.json().catch(() => null) : await r.text();
      if (!r.ok) throw new Error((data && data.error) || (typeof data === 'string' && data.trim()) || `${r.status} ${r.statusText}`);
      return data;
    }
    toast(msg, crit) {
      let el = document.getElementById('tc-toast');
      if (!el) { el = document.createElement('div'); el.id = 'tc-toast'; document.body.appendChild(el); }
      el.className = 'tc-toast' + (crit ? ' crit' : ''); el.hidden = false; el.textContent = msg;
      clearTimeout(this.toastT); this.toastT = setTimeout(() => { el.hidden = true; }, crit ? 9000 : 4000);
    }
    // An act: call the API, report failures, refresh what it may have touched.
    call(method, path, body, okMsg, type) {
      return this.api(method, path, body, type).then((r) => {
        const errs = r && Array.isArray(r.errors) ? r.errors : [];
        if (errs.length) this.toast(errs.join('; '), true); else if (okMsg) this.toast(typeof okMsg === 'function' ? okMsg(r) : okMsg);
        Object.assign(this.dirty, { faults: true, domains: true, facility: true, all: true }); this.schedule();
        return r;
      }).catch((err) => { this.toast(`${method} /v1${path} — ${err.message}`, true); return null; });
    }

    // ---------------------------------------------------------------- connect and index
    async connect() {
      try { await this.pollStatus(true); } catch (e) { console.warn('live mode: GET /v1/status —', e.message); return false; }
      if (!this.status || !this.status.estate) return false;
      await this.load();
      return true;
    }
    // load reads everything the screens draw; reload, after an apply restarted the estate,
    // starts the cache and the event log afresh (the new estate's event numbers start again).
    async load() {
      this.caps.design = !!this.status.canApply;
      const get = (p) => this.api('GET', p);
      const [layout, cat, doms, faults, devs, fac, links, report, full, host, tree] = await Promise.allSettled([get('/layout'), get('/catalogue'), get('/domains'), get('/faults'), get('/devices?select=**'), get('/facility'), get('/links'), get('/report'),
        get('/catalogue?detail=full'), get('/host'), get('/estate?format=json')]);
      for (const r of [layout, cat, doms, faults, devs, fac, links]) if (r.status === 'rejected') this.toast('live mode: ' + r.reason.message, true);
      // the designer's: every format, profile and template of the packs, this host, and the
      // estate file as a tree with the etag a draft is planned against
      this.catalogueFull = full.value || null; this.host = host.value || null;
      this.estateTree = tree.value ? tree.value.tree : null; this.etag = tree.value ? tree.value.etag : this.status.etag || null;
      this.layout = layout.value || { halls: [] }; this.catalogue = cat.value || { faults: [], formats: [] }; this.domains = doms.value || []; this.faultViews = faults.value || [];
      this.fac = fac.value || { feeds: [], loops: [], racks: [] }; this.links = links.value || [];
      for (const n of (report.value && report.value.notModelled) || []) this.isNot[n.subject] = n.isNot || [];
      for (const v of devs.value || []) this.views[v.path] = v;
      this.index();
      await this.backfill(0);
    }
    async reload() {
      Object.assign(this, { views: {}, events: [], seen: new Set(), depth: new Map(), lastSeq: 0, reqs: {}, rf: {}, ports: {}, isNot: {}, plan: null, planKey: null });
      await this.pollStatus(true);
      await this.load();
      this.openStream();
      this.changed();
    }
    start() {
      this.openStream();
      setInterval(() => this.pollStatus().catch(() => {}), 3000);
      setInterval(() => { if (!document.hidden) { this.dirty.all = true; this.dirty.facility = true; this.schedule(); } }, 5000);
      setInterval(() => { if (!document.hidden) { this.dirty.links = true; this.schedule(); } }, 15000);
      setInterval(() => this.changed(), 1000); // the clock and the event flashes move without data
    }
    changed() { this.onChange(); }
    async pollStatus(first) {
      const t0 = performance.now();
      try {
        const st = await this.api('GET', '/status');
        if (st.restarting) { this.stream = 'restarting'; if (!first) this.changed(); return; } // keep the last status until the estate is back
        this.status = st; this.statusAt = Date.now(); this.rtt = Math.round(performance.now() - t0); this.down = false; this.adapters = this.status.adapters || [];
      }
      catch (e) { this.down = true; if (first) throw e; }
      if (!first) this.changed();
    }
    short(p) { const pre = this.prefix; return pre && p && p.startsWith(pre) ? p.slice(pre.length) : p; }
    index() {
      const hall = (this.layout.halls || [])[0] || { rows: [] };
      this.prefix = hall.path ? hall.path + '/' : '';
      this.estate = { name: this.status.estate, seed: null, packs: null, hall: String(hall.name || 'hall').toUpperCase(), hallName: hall.name, site: this.layout.site };
      const facRack = {}; for (const r of this.fac.racks || []) facRack[r.path] = r;
      // every cooling loop with its CDU; cdu and loop are the first, for what draws only one
      this.cdus = (this.fac.loops || []).map((lp) => {
        const s = lp.sensors || {};
        return { name: lp.name, path: lp.device || null, racks: lp.racks || [], capKW: lp.capacityKW, loadKW: lp.loadKW, state: !lp.device || (lp.health === 'OK' && lp.flow) ? 'OK' : 'Critical',
          flow: s.flow ?? 0, supplyC: s['supply-temp'] ?? lp.supplyC, returnC: s['return-temp'] ?? lp.supplyC, flowUnit: 'L/min' };
      });
      this.cdu = this.cdus.find((c) => c.path) || null;
      this.loop = this.cdu ? this.cdu.racks : [];
      const onLoop = new Set(this.cdus.flatMap((c) => c.racks));
      this.rows = (hall.rows || []).map((r) => ({ name: r.name, positions: r.positions }));
      this.racks = {}; this.rackByPath = {};
      for (const row of hall.rows || []) for (const r of row.racks || []) {
        const f = formatKey(r.format, r.archetype), fr = facRack[r.path] || {};
        const H = r.height || 42;
        const devs = r.devices || [];
        const slotted = devs.filter((d) => d.units > 0 && d.side !== 'zero-u' && d.class !== 'sensor').sort((a, b) => b.u - a.u);
        const sensors = devs.filter((d) => !slotted.includes(d)).sort((a, b) => (b.u || 0) - (a.u || 0));
        const R = { name: r.name, path: r.path, row: row.name, pos: r.position, f, tag: formatTag(r.format, f), title: r.format, cooling: onLoop.has(r.name) || f === 'nvl' ? 'liquid' : 'air',
          cap: fr.capacityW || 0, feeds: fr.feeds || [], feedW: fr.feedW || {}, powerW: fr.powerW || 0, height: H, flags: {}, slots: slotted.map((d) => d.path), sensors: sensors.map((d) => d.path), at: {} };
        for (const d of sensors) R.at[d.path] = d.u ? H - d.u : 0;
        this.racks[r.name] = R; this.rackByPath[r.path] = R;
      }
      this.netRack = (Object.values(this.racks).find((r) => r.f === 'net') || {}).name || null;
      this.feeds = {}; for (const f of this.fac.feeds || []) this.feeds[f.name] = { up: f.up, capW: f.capacityW, loadW: f.loadW };
      this.indexChain();
      this.indexDevices();
      this.indexLinks();
    }
    indexDevices() {
      this.devs = {}; this.byRack = {};
      for (const v of Object.values(this.views)) {
        const R = this.racks[v.rack];
        const fw = Object.values(v.firmware || {}).some((f) => f.version !== f.baseline) ? 'drifted' : Object.keys(v.firmware || {}).length ? 'in baseline' : '—';
        const faults = v.faults || [];
        const D = { path: v.path, short: this.short(v.path), name: v.name, rack: v.rack, cls: v.class, units: v.units, health: v.health || 'OK', power: v.power, fid: v.fidelity,
          bmc: v.dialect ? !!v.bmc : true, bmcSlow: faults.some((f) => /bmc\/slow|bmc\/errors/.test(f.fault)), leak: !!(v.sensors && v.sensors.leak > 0), hasLeak: !!(v.sensors && 'leak' in v.sensors),
          fw, serial: (v.identity || {}).serial, ip: v.lease || (v.endpoint || {}).siteIP || '', mac: (v.identity || {}).bmcMAC || '', profile: v.profile, dialect: v.dialect, parent: v.parent,
          w: v.drawW || 0, temp: v.sensors ? (v.sensors['coolant-in'] ?? v.sensors.inlet ?? null) : null, faults,
          comps: (v.components || []).map((c) => ({ name: c.name, path: c.path, kind: c.kind, model: c.model, vendor: c.vendor, health: c.health || 'OK', state: /offline|absent|disabled/i.test(c.state || '') ? 'offline' : c.state })),
          isNot: this.isNot[v.profile] || ['The control API does not serve this profile\'s "is not" list; see the fidelity report (GET /v1/report).'], at: R && R.at[v.path] };
        this.devs[v.path] = D;
        if (R) (this.byRack[R.name] = this.byRack[R.name] || []).push(D);
      }
    }
    // The cabled fabrics from the links: which racks have a port on each, the hub (the rack whose
    // switches end the most cables from other racks: where the fabric is switched), and the racks
    // with a pulled cable on it (the far end of a pulled uplink, not the hub).
    indexLinks() {
      this.pulled = new Set(this.links.filter((l) => !l.up).map((l) => l.id));
      const F = new Map();
      for (const l of this.links) {
        if (l.kind !== 'cable' || !l.fabric) continue;
        if (!F.has(l.fabric)) F.set(l.fabric, { name: l.fabric, racks: new Set(), sw: new Map(), pulled: [] });
        const f = F.get(l.fabric), ends = [l.a, l.b].filter((e) => e && e.rack);
        for (const e of ends) f.racks.add(e.rack);
        if (ends.length === 2 && ends[0].rack !== ends[1].rack) for (const e of ends) if (e.class === 'switch') f.sw.set(e.rack, (f.sw.get(e.rack) || 0) + 1);
        if (!l.up) f.pulled.push(ends);
      }
      this.fabricList = TC.data.styleFabrics([...F.values()].map((f) => {
        const hub = [...f.sw.entries()].sort((a, b) => b[1] - a[1])[0]?.[0] || null, down = new Set();
        for (const ends of f.pulled) { const far = ends.filter((e) => e.rack !== hub); for (const e of far.length ? far : ends) down.add(e.rack); }
        return { name: f.name, racks: f.racks, hub, down };
      }));
    }
    fabrics() { return this.fabricList || []; }

    // ---------------------------------------------------------------- refresh
    schedule() { if (!this.flushT) this.flushT = setTimeout(() => this.flush(), 300); }
    async flush() {
      this.flushT = null;
      const d = this.dirty; const devs = [...d.devs]; this.dirty = { devs: new Set(), all: false, faults: false, domains: false, facility: false, links: false };
      const jobs = [];
      const put = (list) => { for (const v of list || []) this.views[v.path] = v; };
      if (d.all || devs.length > 40) jobs.push(this.api('GET', '/devices?select=**').then((l) => { this.views = {}; put(l); }));
      else if (devs.length) jobs.push(this.api('GET', '/devices?select=' + encodeURIComponent(devs.join(' '))).then(put));
      if (d.faults) jobs.push(this.api('GET', '/faults').then((f) => { this.faultViews = f || []; }));
      if (d.domains) jobs.push(this.api('GET', '/domains').then((x) => { this.domains = x || []; }));
      if (d.facility) jobs.push(this.api('GET', '/facility').then((x) => { this.fac = x || this.fac; }));
      if (d.links) jobs.push(this.api('GET', '/links').then((x) => { this.links = x || []; }));
      await Promise.allSettled(jobs);
      if (d.facility) this.index(); else { this.indexDevices(); if (d.links) this.indexLinks(); }
      this.changed();
    }
    devicesOf(path) {
      const out = [];
      for (const part of String(path || '').split('~')) {
        let p = part.split(':')[0];
        while (p) { if (this.views[p]) { out.push(p); break; } const i = p.lastIndexOf('/'); if (i < 0) break; p = p.slice(0, i); }
      }
      return out;
    }
    norm(e, fresh) {
      const cause = e.cause && this.depth.has(e.cause) ? Math.min(3, this.depth.get(e.cause) + 1) : 0;
      this.depth.set(e.seq, cause);
      const kind = isRead(e) ? 'read' : e.kind;
      return { seq: e.seq, t: fmtClock(new Date(e.t)), at: fresh ? Date.now() : 0, kind, path: this.short(e.path || ''), full: e.path || '', msg: evMsg(e), src: evSrc(e), depth: cause,
        sev: e.severity === 'Critical' ? 'crit' : e.severity === 'Warning' ? 'warn' : '', title: `seq ${e.seq}${e.cause ? ' · caused by ' + e.cause : ''}`, raw: e };
    }
    add(e, fresh) {
      if (!e || this.seen.has(e.seq)) return false;
      this.seen.add(e.seq);
      const n = this.norm(e, fresh); this.events.push(n);
      if (this.events.length > STREAM_LIMIT) for (const x of this.events.splice(0, 500)) { this.seen.delete(x.seq); this.depth.delete(x.seq); }
      if (e.seq > this.lastSeq) this.lastSeq = e.seq;
      if (fresh) {
        for (const p of this.devicesOf(e.path)) { this.dirty.devs.add(p); const d = this.devs[p]; if (d && e.kind === 'fault') { this.pulses[p] = Date.now(); if (d.rack) this.pulses[d.rack] = Date.now(); } }
        if (e.kind === 'fault') this.dirty.faults = true;
        if (e.state && e.state.field === 'domain') this.dirty.domains = true;
        if (e.state && e.state.field === 'link' || /~/.test(e.path || '')) this.dirty.links = true;
        if (/facility/.test(e.path || '') || (e.state && /^(up|isolation\.)/.test(e.state.field))) this.dirty.facility = true;
        if (e.kind === 'request') { const p = this.devicesOf(e.path)[0]; if (p && this.reqs[p]) this.reqs[p].push(this.reqRow(e)); }
        this.schedule();
      }
      return true;
    }
    async backfill(since) {
      const q = new URLSearchParams({ since: String(since), limit: '1500' }); if (!this.reads) q.set('reads', 'false');
      const evs = await this.api('GET', '/events?' + q).catch(() => null);
      for (const e of evs || []) this.add(e, false);
      this.changed();
    }
    // The event stream (server-sent events) is read with fetch, so the token goes in a header and
    // never in a URL a proxy in front would log. It reconnects when it drops, a second later and
    // then twice as long each time it fails, up to 15 s, so a twin that is down is not hammered.
    openStream() {
      this.closeStream();
      const ctl = new AbortController(); this.es = ctl; this.stream = 'connecting';
      const q = new URLSearchParams(); if (!this.reads) q.set('reads', 'false');
      let wait = 1000;
      const run = async () => {
        try {
          const r = await fetch('/v1/events/stream?' + q, { headers: { Authorization: 'Bearer ' + this.token }, cache: 'no-store', signal: ctl.signal });
          if (!r.ok || !r.body) throw new Error(`${r.status}`);
          this.stream = 'open'; wait = 1000; this.backfill(this.lastSeq);
          const rd = r.body.pipeThrough(new TextDecoderStream()).getReader();
          let buf = '';
          for (;;) {
            const { value, done } = await rd.read();
            if (done) break;
            buf += value;
            let i;
            while ((i = buf.indexOf('\n\n')) >= 0) {
              const data = buf.slice(0, i).split('\n').filter((l) => l.startsWith('data:')).map((l) => l.slice(5).trimStart()).join('\n');
              buf = buf.slice(i + 2);
              if (!data) continue; // a keep-alive
              let e; try { e = JSON.parse(data); } catch (x) { continue; }
              if (this.add(e, true)) this.changed();
            }
          }
        } catch (x) { /* dropped or refused: reconnect below */ }
        if (ctl.signal.aborted) return;
        this.stream = 'reconnecting'; this.changed();
        setTimeout(() => { if (!ctl.signal.aborted) run(); }, wait);
        wait = Math.min(wait * 2, 15000);
      };
      run();
    }
    closeStream() { if (this.es) this.es.abort(); this.es = null; }
    onKinds(k) { const want = !!k.read; if (want !== this.reads) { this.reads = want; this.openStream(); if (want) this.backfill(0); } }

    // ---------------------------------------------------------------- what the views read
    defaultSelection() {
      const R = Object.values(this.racks).find((r) => r.f === 'nvl') || Object.values(this.racks)[0];
      if (!R) return { rack: null, dev: null };
      return { rack: R.name, dev: R.slots.find((p) => /compute-tray-07$/.test(p)) || R.slots.find((p) => /server-01$/.test(p)) || R.slots[0] };
    }
    clock() {
      const s = this.status; if (!s) return '—';
      const base = new Date(s.now).getTime();
      return fmtClock(new Date(s.clock === 'wall' ? base + (Date.now() - this.statusAt) * (s.timeScale || 1) : base));
    }
    clockLabel() { return this.status ? this.status.clock : 'clock'; }
    canAdvance() { return !!this.status && this.status.clock === 'virtual'; }
    advance(d) { this.call('POST', '/clock/advance', { duration: d }).then(() => this.pollStatus()); }
    lag() { return this.rtt; }
    // The power chain (facility.power.topology, in GET /v1/facility): a feed with a UPS at its
    // head is drawn as switchgear and UPS, with a panel per row and feed, as the design does, from
    // the twin's nodes; a feed as the racks see it is then its UPS's output. A feed with no UPS is
    // drawn as the feed itself.
    indexChain() {
      this.chain = this.fac.power || [];
      this.utility = {}; this.ups = {}; this.rpp = {}; this.rppNode = {};
      for (const f of this.fac.feeds || []) {
        this.utility[f.name] = { up: f.up };
        const u = this.chain.find((n) => n.kind === 'ups' && n.from === f.name);
        if (u) this.ups[f.name] = { mode: u.mode, batt: Math.round(u.charge ?? 100), name: u.name, path: u.path, node: u };
      }
      this.caps.infra = Object.keys(this.ups).length > 0;
      if (!this.caps.infra) return;
      for (const n of Object.keys(this.ups)) this.feeds[n].up = this.ups[n].mode === 'online' || this.ups[n].mode === 'battery';
      for (const row of this.rows) {
        const rk = Object.values(this.racks).filter((r) => r.row === row.name).map((r) => r.name);
        for (const n of Object.keys(this.feeds)) {
          const p = this.chain.find((x) => x.kind !== 'ups' && x.feed === n && (x.racks || []).some((r) => rk.includes(r)));
          if (p) { this.rppNode[`${row.name}-${n}`] = p; this.rpp[`${row.name}-${n}`] = p.mode === 'open'; }
        }
      }
    }
    nodeFault(path, id) { return this.faultViews.find((f) => f.Fault === id && f.Target === path); }
    toggleNodeFault(node, id) {
      if (!node) return;
      const ex = this.nodeFault(node.path, id);
      if (ex) this.call('POST', '/faults/clear', { target: ex.ID }, `cleared ${id} on ${node.name}`); else this.call('POST', '/faults', { select: node.path, fault: id }, `injected ${id} on ${node.name}`);
    }
    incidents() { return (this.status && this.status.incidents) || 0; }
    stackLabel() { return { text: `live · ${this.adapters.join(', ') || 'no adapters'}`, title: `a client of ${location.host}/v1 · adapters: ${this.adapters.join(', ') || 'none'} · the software under test is seen only through the requests and events it causes` }; }
    devWatts(d) { return d.w || 0; }
    devTemp(d) { return typeof d.temp === 'number' ? d.temp : null; }
    rackCap(R) { return R.cap || Math.max(1, R.powerW * 1.25); }
    rackAgg(R) {
      const ds = this.byRack[R.name] || [];
      let h = 0, flagged = 0, unreach = 0, w = 0; const temps = [];
      for (const d of ds) { h = Math.max(h, SEVN[d.health] || 0); if (d.health !== 'OK' && !d.parent) flagged++; if (!d.bmc && !d.parent) unreach++; if (!d.parent) w += d.w; const t = this.devTemp(d); if (t != null && !d.parent) temps.push(t); }
      const temp = temps.length ? temps.reduce((a, b) => a + b, 0) / temps.length : null;
      let dom = null;
      if (R.f === 'nvl') {
        const own = this.domains.filter((d) => d.kind === 'scale-up' && d.path.startsWith(R.path + '/domain/'));
        const st = own.map((d) => d.state);
        dom = !st.length ? null : st.every((s) => s === 'formed') ? 'Ready' : st.some((s) => s === 'unformed') ? 'Unformed' : 'Degraded';
      }
      return { w: R.powerW || w, health: ['OK', 'Warning', 'Critical'][h], temp, flagged, unreach, dom };
    }
    feedBroken(R, n) { const f = this.feeds[n]; return !f || !f.up || ((R.feeds || []).includes(n) && R.powerW > 0 && !(R.feedW[n] > 0)); }
    feedLoad(n) { return this.caps.infra && this.ups[n] ? this.ups[n].node.loadW || 0 : (this.feeds[n] || {}).loadW || 0; }
    feedShare() { return 1; }
    rackFeedW(R, a, n) { return R.feedW[n] || 0; }
    rowFeedW(row, n) { return Object.values(this.racks).filter((r) => r.row === row).reduce((a, r) => a + (r.feedW[n] || 0), 0); }
    upsLabel(n) {
      const U = this.ups[n];
      if (this.caps.infra && U) {
        const left = U.node.runtimeLeftS || 0, mins = left >= 90 ? `${Math.round(left / 60)} min` : `${Math.round(left)} s`;
        return U.mode === 'battery' ? `on battery · ${U.batt} % · ${mins} left` : U.mode === 'online' ? `online${U.batt < 100 ? ` · charging ${U.batt} %` : ''}` : U.mode === 'fault' ? 'fault · 0 kW' : 'depleted · 0 kW';
      }
      const k = Object.values(this.racks).filter((r) => (r.feeds || []).includes(n)).length; return this.feeds[n] && this.feeds[n].up ? `${k} racks drawing` : `lost · ${k} racks on the other feed`;
    }
    // the infra popover's state, by the demo's act names
    has(id) {
      let m;
      if ((m = /utility-loss · feed (\S+)/.exec(id))) return !!this.utility[m[1]] && !this.utility[m[1]].up;
      if ((m = /ups-failure · ups-(\S+)/.exec(id))) return !!this.ups[m[1]] && !!this.nodeFault(this.ups[m[1]].path, 'generic/power/ups-failure');
      if ((m = /breaker-trip · rpp-(\S+) (\S+)/.exec(id))) { const p = this.rppNode[`${m[1]}-${m[2]}`]; return !!p && !!this.nodeFault(p.path, 'generic/power/breaker-trip'); }
      return false;
    }
    activeOn(D) { return (D.faults || []).length; }
    get active() {
      return this.faultViews.map((f) => ({ key: f.ID, title: f.Title || f.Fault, target: this.short(f.Target), by: f.Parent ? 'from ' + f.Parent : 'control-api', t: f.Since ? fmtClock(new Date(f.Since)) : '', raw: f }));
    }
    eventTarget(e) {
      const p = this.devicesOf(e.full)[0];
      if (p) { let d = this.devs[p]; while (d && d.parent && this.devs[d.parent]) d = this.devs[d.parent]; return { dev: d.path }; }
      const R = Object.values(this.racks).find((r) => (e.full || '').startsWith(r.path + '/') || e.full === r.path);
      return R ? { rack: R.name } : null;
    }

    // ---------------------------------------------------------------- faults
    mkFault(f, kindOf, chips) {
      const params = f.params && Object.keys(f.params).length ? f.params : null;
      return { id: f.id, title: f.title, kindOf, comps: chips, clears: clearsText(f.clears), params,
        notes: [`target: ${targetText(f.target)}`, ...(params ? [] : ['no params'])],
        paramValues: (vals) => { const out = {}; for (const [k, p] of Object.entries(params || {})) { const v = (vals || {})[k]; if (v === undefined || v === '') continue; out[k] = p.type === 'int' || p.type === 'number' ? Number(v) : v; } return out; } };
    }
    faultsFor(D) {
      const v = this.views[D.path]; if (!v) return [];
      const cat = (this.catalogue.faults || []).slice().sort((a, b) => a.id.localeCompare(b.id));
      const comp = [], dev = [], port = [];
      for (const f of cat) {
        const t = f.target || {};
        if (t.component) { const cs = (v.components || []).filter((c) => fitsComp(t, c)).map((c) => c.name); if (cs.length) comp.push(this.mkFault(f, 'component', cs)); }
        else if (t.port) { const ps = this.ports[D.path]; if (ps && ps.length) port.push(this.mkFault(f, 'port', ps)); }
        else if (fitsDevice(t, v)) dev.push(this.mkFault(f, 'device', null));
      }
      return [...comp, ...dev, ...port];
    }
    rackFaults(R) {
      const by = new Map();
      for (const p of [...R.sensors, ...R.slots]) {
        const v = this.views[p]; if (!v || !['sensor', 'pdu', 'power-shelf'].includes(v.class)) continue;
        for (const f of this.catalogue.faults || []) if (fitsDevice(f.target, v)) { if (!by.has(f.id)) by.set(f.id, { f, devs: [] }); by.get(f.id).devs.push(v.name); }
      }
      return [...by.values()].sort((a, b) => a.f.id.localeCompare(b.f.id)).map(({ f, devs }) => ({ ...this.mkFault(f, 'rack', devs), targetDev: R.path }));
    }
    selectFor(faultId, target, comp) {
      const f = (this.catalogue.faults || []).find((x) => x.id === faultId); const t = (f && f.target) || {};
      if (!comp) return target;
      if (t.port) return `${target}:${comp}`;
      return `${target}/${comp}`; // a component, or a device in a rack
    }
    inject(faultId, target, comp, by, when, params) {
      const select = this.selectFor(faultId, target, comp);
      const go = () => this.call('POST', '/faults', { select, fault: faultId, params: params || {} }, (r) => `injected ${faultId} on ${(r || []).map((x) => this.short(x.Target)).join(', ') || this.short(select)}`);
      if (when && when !== 'now') { const n = Number(when); setTimeout(go, n * 1000); return `scheduled in this browser for ${n} s from now`; }
      go(); return '';
    }
    clear(a) { this.call('POST', '/faults/clear', { target: a.key }, (r) => `cleared ${r.cleared} fault(s)`); }
    act(kind, path) {
      const short = this.short(path);
      if (kind === 'reseat') return this.call('POST', '/reseat', { select: path }, `reseat ${short}`);
      if (kind === 'bmc') return this.call('POST', '/bmc/reset', { select: path }, (r) => (r.devices ? `BMC reset on ${short}` : `${short} has no BMC`));
      const action = { off: 'off', on: 'on', ac: 'ac-cycle' }[kind];
      return this.call('POST', '/power', { select: path, action }, (r) => `power ${action} → ${r.canonical} on ${short}`);
    }
    chaosFaults() { return (this.catalogue.faults || []).filter((f) => (f.target && f.target.component === 'accelerator') || ['generic/bmc/unreachable', 'generic/bmc/slow', 'generic/thermal/fan-failure', 'generic/power/psu-failure'].includes(f.id)).map((f) => f.id); }
    setChaos(on, rate) {
      this.chaos = { on, rate }; this.changed();
      this.call('POST', '/chaos', { rate: on ? rate : 0, faults: on ? this.chaosFaults() : [] }, on ? `chaos at ${rate}/device/h` : 'chaos stopped');
    }

    // ---------------------------------------------------------------- facility
    cduOf(name) { return name ? this.cdus.find((c) => c.name === name) : this.cdu; }
    cduFault(id, name) { const c = this.cduOf(name); return c && c.path && this.faultViews.find((f) => f.Fault === id && f.Target === c.path); }
    facility(kind) {
      if (/^(feed|utility)-/.test(kind)) { const n = kind.replace(/^(feed|utility)-/, ''), f = this.utility[n] || this.feeds[n]; if (!f) return; this.call('POST', '/feeds', { feed: n, up: !f.up }, `feed ${n} ${f.up ? 'down' : 'up'}`); return; }
      let m;
      if ((m = /^ups-(.+)$/.exec(kind))) { this.toggleNodeFault(this.ups[m[1]] && this.ups[m[1]].node, 'generic/power/ups-failure'); return; }
      if ((m = /^rpp-(.+)-([^-]+)$/.exec(kind))) { this.toggleNodeFault(this.rppNode[`${m[1]}-${m[2]}`], 'generic/power/breaker-trip'); return; }
      if ((m = /^node-(.+)$/.exec(kind))) { const n = this.chain.find((x) => x.name === m[1]); if (n) this.toggleNodeFault(n, n.kind === 'ups' ? 'generic/power/ups-failure' : 'generic/power/breaker-trip'); return; }
      if ((m = /^(cdu|cdu-temp)(?::(.+))?$/.exec(kind))) {
        const c = this.cduOf(m[2]); if (!c || !c.path) return;
        const id = m[1] === 'cdu' ? 'generic/thermal/cdu-flow-loss' : 'generic/thermal/coolant-excursion';
        const ex = this.cduFault(id, c.name);
        if (ex) this.call('POST', '/faults/clear', { target: ex.ID }, `cleared ${id}`); else this.call('POST', '/faults', { select: c.path, fault: id }, `injected ${id} on ${this.short(c.path)}`);
        return;
      }
      if (kind === 'rack-leak') {
        const s = this.leakSensor(); if (!s) return;
        const ex = this.faultViews.find((f) => f.Fault === 'generic/leak/rack-point' && f.Target === s.path);
        if (ex) this.call('POST', '/faults/clear', { target: ex.ID }, 'cleared the rack leak point'); else this.call('POST', '/faults', { select: s.path, fault: 'generic/leak/rack-point' }, `rack leak point on ${s.short}`);
      }
    }
    leakSensor() { for (const n of [...this.cdus.flatMap((c) => c.racks), ...Object.keys(this.racks)]) { const R = this.racks[n]; if (!R) continue; const p = R.sensors.find((x) => this.devs[x] && this.devs[x].cls === 'sensor'); if (p) return this.devs[p]; } return null; }
    facilityActs() {
      const out = [];
      const many = this.cdus.filter((c) => c.path).length > 1;
      for (const c of this.cdus) {
        if (!c.path) continue;
        const of = many ? ` · ${c.name}` : '';
        out.push({ label: (this.cduFault('generic/thermal/cdu-flow-loss', c.name) ? 'Restore CDU flow' : 'CDU flow loss') + of, kind: `cdu:${c.name}` });
        out.push({ label: (this.cduFault('generic/thermal/coolant-excursion', c.name) ? 'Clear coolant excursion' : 'Coolant excursion') + of, kind: `cdu-temp:${c.name}` });
      }
      const s = this.leakSensor();
      if (s) out.push({ label: `${this.faultViews.some((f) => f.Fault === 'generic/leak/rack-point' && f.Target === s.path) ? 'Clear' : 'Rack'} leak point ${s.rack}`, kind: 'rack-leak' });
      return out;
    }
    bmsPoints() {
      const out = [];
      for (const c of this.cdus) { const cv = c.path && this.views[c.path]; if (cv) for (const [k, v] of Object.entries(cv.sensors || {})) out.push([`${this.short(cv.path)} · ${k}`, fmtV(v)]); }
      for (const [n, f] of Object.entries(this.utility || this.feeds)) out.push([`feed ${n} · state`, f.up ? 'up' : 'down']);
      for (const n of this.chain || []) out.push([`${n.name} · ${n.kind === 'ups' ? 'mode · battery' : 'breaker'}`, n.kind === 'ups' ? `${n.mode} · ${Math.round(n.charge ?? 0)} %` : n.mode === 'open' ? 'open (tripped)' : n.mode === 'dead' ? 'no supply' : 'closed']);
      for (const R of Object.values(this.racks)) for (const p of R.sensors) { const d = this.devs[p]; if (d && d.cls === 'sensor' && d.hasLeak) out.push([`${d.short} · leak`, d.leak ? 'true' : 'false']); }
      return out;
    }

    // ---------------------------------------------------------------- device details, loaded on select
    reqRow(e) { const q = e.request || {}; return { t: fmtClock(new Date(e.t)), m: q.method || q.canonical, p: q.target || q.canonical, s: q.status ? String(q.status) : '—' }; }
    onSelect(path) {
      const d = this.devs[path]; if (!d) return;
      if (!this.reqs[path]) { this.reqs[path] = []; this.api('GET', `/events?select=${encodeURIComponent(path)}&kind=request&limit=200`).then((l) => { this.reqs[path] = (l || []).map((e) => this.reqRow(e)); this.changed(); }, () => {}); }
      if (!this.ports[path]) {
        this.ports[path] = [];
        this.api('GET', '/links?select=' + encodeURIComponent(path)).then((ls) => {
          const names = []; for (const l of ls || []) for (const end of [l.a, l.b]) if (end && end.device === path) names.push(end.port);
          this.ports[path] = [...new Set(names)].sort(); this.changed();
        }, () => {});
      }
      if (d.dialect && !this.rf[path]) {
        this.rf[path] = 'reading…';
        const get = (p) => fetch(`/v1/inspect/redfish?device=${encodeURIComponent(path)}&path=${encodeURIComponent(p)}`, { headers: { Authorization: 'Bearer ' + this.token } }).then((r) => r.json());
        get('/redfish/v1/Systems').then((col) => {
          const m = col && col.Members && col.Members[0] && col.Members[0]['@odata.id'];
          return m ? get(m) : col;
        }).then((doc) => {
          if (!doc || doc.error) { this.rf[path] = doc && doc.error ? doc.error : '(no Redfish system)'; this.changed(); return; }
          const keep = ['@odata.id', 'Id', 'Manufacturer', 'Model', 'SerialNumber', 'PowerState', 'Status', 'ProcessorSummary', 'MemorySummary', 'BiosVersion'];
          const o = {}; for (const k of keep) if (k in doc) o[k] = doc[k];
          this.rf[path] = JSON.stringify(o, null, 2); this.changed();
        }).catch((e) => { this.rf[path] = e.message; this.changed(); });
      }
    }
    redfish(D) { if (!D.dialect) return '(no BMC — this device publishes over BMS / SNMP)'; if (!this.rf[D.path]) this.onSelect(D.path); return this.rf[D.path] || 'reading…'; }
    reqsOf(D) { return this.reqs[D.path] || []; }
    reqCount(D) { return (this.reqs[D.path] || []).length; }

    // ---------------------------------------------------------------- topology
    topoModel(want) {
      const cables = this.links.filter((l) => l.kind === 'cable');
      const fabrics = [...new Set(cables.map((l) => l.fabric))].sort();
      const fabric = fabrics.includes(want) ? want : fabrics.includes('frontend') ? 'frontend' : fabrics[0];
      const ls = cables.filter((l) => l.fabric === fabric);
      const pulled = this.links.filter((l) => !l.up).map((l) => ({ id: l.id, label: l.id.split('~').map((p) => this.short(p)).join(' ↔ ') }));
      if (!ls.length) return { fabric, fabrics, nodes: [], links: [], pulled, title: 'no cabled fabric in this estate', empty: 'GET /v1/links returned no cables.' };
      const isSw = (e) => e && e.class === 'switch';
      const peers = new Map(); const add = (a, b) => { if (!peers.has(a.device)) peers.set(a.device, []); peers.get(a.device).push(b); };
      for (const l of ls) { if (isSw(l.a)) add(l.a, l.b); if (isSw(l.b)) add(l.b, l.a); }
      const leaves = [...peers.keys()].filter((s) => peers.get(s).some((p) => !isSw(p)));
      const spines = [...peers.keys()].filter((s) => !leaves.includes(s)).sort();
      const rackNames = [...new Set(ls.flatMap((l) => [l.a, l.b]).filter((e) => e && !isSw(e) && e.rack).map((e) => e.rack))].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
      const W = 960, x0 = 80, span = W - x0 - 30;
      const rx = {}; rackNames.forEach((r, i) => { rx[r] = x0 + (span / Math.max(1, rackNames.length)) * (i + 0.5); });
      const nodes = [], links = [];
      spines.forEach((s, i) => nodes.push({ id: s, label: this.devs[s] ? this.devs[s].name : s.split('/').pop(), x: x0 + span * ((i + 1) / (spines.length + 1)), y: 40, w: 84, health: this.devs[s] && this.devs[s].health }));
      const groups = new Map();
      for (const s of leaves) { const rs = [...new Set(peers.get(s).filter((p) => !isSw(p)).map((p) => p.rack))].sort(); const k = rs.join(','); if (!groups.has(k)) groups.set(k, []); groups.get(k).push({ s, rs }); }
      const lx = {};
      for (const g of groups.values()) g.sort((a, b) => a.s.localeCompare(b.s)).forEach((x, i) => { const c = x.rs.reduce((a, r) => a + (rx[r] || 0), 0) / Math.max(1, x.rs.length); lx[x.s] = c + (i - (g.length - 1) / 2) * 40; });
      for (const s of leaves) { const nm = this.devs[s] ? this.devs[s].name : s.split('/').pop(); const m = /leaf-(.+)-(\d+)$/.exec(nm); nodes.push({ id: s, label: m ? `${m[1]}·${m[2]}` : nm.replace(/^.*leaf-?/, '') || nm, tip: this.short(s), x: lx[s], y: 170, w: 38, health: this.devs[s] && this.devs[s].health }); }
      for (const r of rackNames) { const R = this.racks[r]; nodes.push({ id: 'rack:' + r, label: r, x: rx[r], y: 300, w: 46, health: R ? this.rackAgg(R).health : null }); }
      const y = (d) => (spines.includes(d) ? 40 : 170);
      const agg = new Map();
      for (const l of ls) {
        if (isSw(l.a) && isSw(l.b)) {
          const [a, b] = y(l.a.device) <= y(l.b.device) ? [l.a, l.b] : [l.b, l.a];
          const xa = spines.includes(a.device) ? nodes.find((n) => n.id === a.device).x : lx[a.device];
          links.push({ id: l.id, x1: xa, y1: y(a.device) + 11, x2: lx[b.device] ?? xa, y2: y(b.device) - 11, down: !l.up, tip: l.id.split('~').map((p) => this.short(p)).join(' ↔ ') });
        } else {
          const sw = isSw(l.a) ? l.a : l.b, host = isSw(l.a) ? l.b : l.a;
          const k = `agg:${sw.device}|${host.rack}`;
          if (!agg.has(k)) agg.set(k, { ids: [], down: 0, sw: sw.device, rack: host.rack });
          const g = agg.get(k); g.ids.push(l.id); if (!l.up) g.down++;
        }
      }
      this.aggLinks = agg;
      for (const [k, g] of agg) links.push({ id: k, x1: lx[g.sw], y1: 181, x2: rx[g.rack], y2: 289, down: g.down === g.ids.length, part: g.down > 0 && g.down < g.ids.length, tip: `${this.short(g.sw)} → ${g.rack}: ${g.ids.length} links, ${g.down} down` });
      return { fabric, fabrics, nodes, links, pulled, title: `${fabric} · ${spines.length} spines · ${leaves.length} leaves · ${ls.length} cables · click a link to pull its cable` };
    }
    pullLink(id, replace) {
      const g = this.aggLinks && this.aggLinks.get(id);
      const set = (ids, state) => Promise.all(ids.map((link) => this.api('POST', '/cable', { link, state }))).then(() => { this.toast(`${ids.length} cable${ids.length === 1 ? '' : 's'} ${state === 'pull' ? 'pulled' : 'replaced'}`); }, (e) => this.toast('POST /v1/cable — ' + e.message, true)).finally(() => { this.dirty.links = this.dirty.all = this.dirty.faults = true; this.schedule(); });
      if (g) { const down = g.ids.filter((x) => this.pulled.has(x)); return down.length ? set(down, 'replace') : set(g.ids, 'pull'); }
      return set([id], replace || this.pulled.has(id) ? 'replace' : 'pull');
    }

    // ---------------------------------------------------------------- scenarios and design
    runScenarioText() {
      this.scn = { running: true, result: null, err: null }; this.changed();
      this.api('POST', '/scenarios/run', this.scnText, 'yaml').then((r) => { this.scn = { running: false, result: r, err: null }; }, (e) => { this.scn = { running: false, result: null, err: e.message }; }).finally(() => this.changed());
    }
    // A dry-run apply of the draft, fetched once per draft and cached: the twin compiles the
    // changed estate and says what it would refuse.
    planFor(draft) {
      if (!draft.length) return null;
      const key = JSON.stringify(draft.map((d) => [d.op || 'add', d.row, d.name, d.pos, d.fmt]));
      if (this.planKey !== key) {
        this.planKey = key; this.plan = { loading: true };
        clearTimeout(this.planT);
        this.planT = setTimeout(() => this.applyCall(draft, true).then((r) => { if (this.planKey === key) { this.plan = r; this.changed(); } }), 250);
      }
      return this.plan;
    }
    async applyCall(draft, dryRun) {
      const racks = draft.filter((d) => d.op !== 'remove').map((d) => ({ hall: this.estate.hallName, row: d.row, name: d.name, position: d.pos, format: d.fmt }));
      const remove = draft.filter((d) => d.op === 'remove').map((d) => d.name);
      try {
        const r = await fetch('/v1/estate/apply', { method: 'POST', headers: { Authorization: 'Bearer ' + this.token, 'Content-Type': 'application/json' }, body: JSON.stringify({ racks, remove, dryRun }) });
        const j = await r.json().catch(() => null);
        if (j && 'ok' in j) return { ...j, status: r.status };
        return { ok: false, problems: [(j && j.error) || `${r.status} ${r.statusText}`], status: r.status };
      } catch (e) { return { ok: false, problems: [e.message] }; }
    }
    // The designer's draft: ops on the estate file, planned against the etag it was read at.
    async estateCall(path, body) {
      try {
        const r = await fetch('/v1' + path, { method: 'POST', headers: { Authorization: 'Bearer ' + this.token, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        const j = await r.json().catch(() => null);
        if (j && 'ok' in j) return { ...j, problems: j.problems || (j.error ? [j.error] : []), status: r.status };
        return { ok: false, problems: [(j && j.error) || `${r.status} ${r.statusText}`], status: r.status };
      } catch (e) { return { ok: false, problems: [e.message] }; }
    }
    planOps(ops) { return this.estateCall('/estate/plan', { etag: this.etag, ops }); }
    async applyOps(ops, log, done) { return this.follow(await this.estateCall('/estate/apply', { etag: this.etag, ops }), log, done); }
    async applyDraft(draft, log, done) { return this.follow(await this.applyCall(draft, false), log, done); }
    // Apply: the twin writes the changed estate file and restarts the estate on it; the console
    // follows the restart through /v1/status and reconnects to the new estate.
    async follow(r, log, done) {
      if (!r.ok) { for (const p of r.problems || ['refused']) log('✕ ' + p); done(false); return; }
      log(`validated · ${r.devices[0]} → ${r.devices[1]} devices, ${r.links[0]} → ${r.links[1]} links`);
      for (const n of r.notes || []) log(n);
      log(`estate file written · ${r.file} (etag ${r.etag})`);
      log('… estate going down to restart');
      this.closeStream();
      this.stream = 'restarting'; this.changed();
      const t0 = Date.now();
      let st = null;
      while (Date.now() - t0 < 180000) {
        await new Promise((ok) => setTimeout(ok, 700));
        try { st = await this.api('GET', '/status'); } catch (e) { continue; }
        if (!st.restarting) break;
      }
      const a = st && st.apply;
      if (!a || a.state !== 'up') {
        log('✕ ' + ((a && a.error) || 'the estate did not come back in time'));
        if (a && a.state === 'restored') log('… the previous estate is back up; the draft is kept');
        await this.reload().catch(() => {});
        done(false); return;
      }
      log(`estate up again · took ${a.took}`);
      await this.reload();
      for (const n of r.racksRemoved || []) log(`${n}: ${this.racks[n] ? '✕ still in the layout' : 'removed · its devices and cables are gone'}`);
      for (const n of r.racksAdded || []) { const ds = this.byRack[n] || []; const on = ds.filter((d) => d.power === 'On').length; log(`${n}: in the estate · ${ds.length} devices, ${on} powered on`); }
      log(`${Object.keys(this.devs).length} devices · ${Object.keys(this.racks).length} racks in the layout`);
      for (const n of r.racksAdded || []) if (this.racks[n]) this.pulses[n] = Date.now();
      done(true);
    }
    designFormats() { const f = (this.catalogue.formats || []).map((x) => [x.name, `${x.archetype}${x.params ? ' · ' + Object.keys(x.params).slice(0, 3).join(', ') : ''}`]).sort((a, b) => a[0].localeCompare(b[0])); return f.length ? f : [['generic/cpu-rack', 'cpu']]; }
    formatTagOf(fmt) { return formatTag(fmt, formatKey(fmt)); }
  }

  const EXAMPLE = `# An example to edit: Xid 79 on one GPU of an NVL72 tray, then an AC cycle clears it.
# Paste any scenario file instead (examples/scenarios/ in the repository).
apiVersion: dc-twin/v1alpha1
kind: Scenario
metadata: { name: console-xid-79 }
spec:
  estate: { requires: { formats: [nvidia/gb200-nvl72] } }
  stack: {}
  timeout: 2m
  steps:
    - fault: { id: x79, select: "a01/compute-tray-07/gpu-2", inject: nvidia/xid/xid-79 }
    - expect:
        within: 5s
        all:
          - model:  { expr: 'model.device("a01/compute-tray-07").health == "Critical"' }
          - domain: { path: "a01/domain/scale-up-nvlink", state: degraded }
    - act: { power: ACCycle, select: "a01/compute-tray-07" }
    - expect:
        within: 2m          # AC returns, the BMC boots (about 30 s), then the fault clears
        all:
          - faults: { select: "a01/compute-tray-07/**", count: 0 }
  report: fidelity
`;
  return Live;
})();
