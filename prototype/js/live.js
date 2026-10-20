// live.js — LIVE mode: the console as a client of a running twin's control API (/v1, D16).
// Selected when the page carries ?token= and /v1/status answers on the same origin (`dctwin up
// --console prototype`, then `dctwin ui`). Everything shown here comes from the API: the layout,
// device views, the catalogue, domains, faults and the event log. Every act is an API call; the
// console holds no model of its own. Without a token this module stays idle and the in-memory
// demo runs unchanged.
window.DCT = window.DCT || {};
DCT.live = (() => {
  const U = DCT.util, esc = U.esc;
  const token = new URLSearchParams(location.search).get('token');
  const L = {
    on: false, hooks: null, st: null, status: null, statusAt: 0, down: false, layout: { halls: [] }, catalogue: { faults: [] },
    domains: [], faults: [], devices: {}, racks: {}, rackOf: {}, byRack: {}, children: {},
    events: [], bySeq: new Map(), lastSeq: 0, es: null, stream: 'connecting',
    rf: { device: null, path: null, body: null, status: 0, err: null, loading: false, hist: [] },
    reqs: { device: null, list: [], loading: false, err: null },
    report: { text: null, loading: false, at: null, err: null },
    scn: { running: false, result: null, err: null },
    ui: { reads: false, hideKinds: new Set(), fxSel: '', fxFault: '', fxParams: {}, fxId: '', chaosRate: 2, chaosFaults: [], chaosSet: null, cableRef: '', port: '', util: '', scnText: null },
  };
  const MAX_EVENTS = 4000;

  // ------------------------------------------------------------- the API
  async function api(method, path, body, type) {
    const opt = { method, headers: { Authorization: 'Bearer ' + token } };
    if (body !== undefined) {
      opt.headers['Content-Type'] = type === 'yaml' ? 'application/yaml' : 'application/json';
      opt.body = type === 'yaml' ? body : JSON.stringify(body);
    }
    const r = await fetch('/v1' + path, opt);
    const ct = r.headers.get('Content-Type') || '';
    const data = ct.includes('json') ? await r.json().catch(() => null) : await r.text();
    if (!r.ok) throw new Error((data && data.error) || (typeof data === 'string' && data.trim()) || `${r.status} ${r.statusText}`);
    return data;
  }
  const changed = () => L.hooks && L.hooks.changed();
  const render = () => L.hooks && L.hooks.render();

  function toast(msg, sev) {
    let el = document.getElementById('live-toast');
    if (!el) { el = document.createElement('div'); el.id = 'live-toast'; document.body.appendChild(el); }
    el.className = `toast ${sev === 'crit' ? 'crit' : ''}`; el.hidden = false;
    el.innerHTML = `${sev === 'crit' ? U.sevHTML('Critical') + ' ' : ''}${esc(msg)}`;
    clearTimeout(toast.t); toast.t = setTimeout(() => { el.hidden = true; }, sev === 'crit' ? 9000 : 4500);
  }
  // An act: call the API, say what happened, refresh what it touched.
  function call(method, path, body, okMsg, type) {
    return api(method, path, body, type).then((r) => {
      const errs = r && Array.isArray(r.errors) ? r.errors : [];
      if (errs.length) toast(errs.join('; '), 'crit'); else if (okMsg) toast(typeof okMsg === 'function' ? okMsg(r) : okMsg);
      dirty.faults = dirty.status = dirty.domains = true; schedule();
      return r;
    }).catch((err) => { toast(`${method} /v1${path} — ${err.message}`, 'crit'); return null; });
  }

  // ------------------------------------------------------------- the store
  function indexLayout(layout) {
    L.layout = layout || { halls: [] }; L.racks = {}; L.rackOf = {};
    for (const h of L.layout.halls || []) for (const row of h.rows || []) for (const r of row.racks || []) {
      L.racks[r.path] = { ...r, row: row.name, hall: h.path, devices: r.devices || [] };
      for (const d of r.devices || []) L.rackOf[d.path] = r.path;
    }
  }
  // Returns whether any view differs from what the store held, so a quiet poll does not re-render.
  function putDevices(list, full) {
    if (!Array.isArray(list)) return false;
    let diff = !!full && list.length !== Object.keys(L.devices).length;
    const prev = L.devices; if (full) L.devices = {};
    for (const v of list) { if (!diff && JSON.stringify(prev[v.path]) !== JSON.stringify(v)) diff = true; L.devices[v.path] = v; }
    L.children = {}; L.byRack = {};
    for (const v of Object.values(L.devices)) {
      if (v.parent) { (L.children[v.parent] = L.children[v.parent] || []).push(v.path); if (!L.rackOf[v.path] && L.rackOf[v.parent]) L.rackOf[v.path] = L.rackOf[v.parent]; }
      const rp = L.rackOf[v.path]; if (rp) (L.byRack[rp] = L.byRack[rp] || []).push(v.path);
    }
    return diff;
  }
  const loadDevices = (sel, full) => api('GET', '/devices?select=' + encodeURIComponent(sel)).then((l) => putDevices(l, full));
  async function pollStatus() {
    try { L.status = await api('GET', '/status'); L.statusAt = Date.now(); L.down = false; } catch (e) { L.down = true; }
    changed();
  }

  // The device a path belongs to: a device, a component, a port ref (path:port) or a link (ref~ref).
  function devicesOf(path) {
    const out = [];
    for (const part of String(path || '').split('~')) {
      let p = part.split(':')[0];
      while (p) { if (L.devices[p]) { out.push(p); break; } const i = p.lastIndexOf('/'); if (i < 0) break; p = p.slice(0, i); }
    }
    return out;
  }
  function inScope(path, scope) {
    return String(path || '').split('~').some((part) => { const b = part.split(':')[0]; return b === scope || b.startsWith(scope + '/'); });
  }

  // Events change devices; the views they touch are re-read, batched.
  const dirty = { devs: new Set(), faults: false, domains: false, status: false, reqs: false };
  let flushT = null;
  function schedule() { if (!flushT) flushT = setTimeout(flush, 300); }
  async function flush() {
    flushT = null;
    const devs = [...dirty.devs]; const d = { ...dirty }; dirty.devs.clear(); dirty.faults = dirty.domains = dirty.status = dirty.reqs = false;
    const jobs = [];
    if (devs.length > 40) jobs.push(loadDevices('**', true)); else if (devs.length) jobs.push(loadDevices(devs.join(' ')));
    if (d.faults) jobs.push(api('GET', '/faults').then((f) => { L.faults = f || []; }));
    if (d.domains) jobs.push(api('GET', '/domains').then((x) => { L.domains = x || []; }));
    if (d.status) jobs.push(pollStatus());
    if (d.reqs && L.reqs.device) jobs.push(loadReqs(L.reqs.device));
    await Promise.allSettled(jobs); changed();
  }
  function onEvent(ev) {
    for (const p of devicesOf(ev.path)) dirty.devs.add(p);
    if (ev.kind === 'fault') dirty.faults = true;
    if (/\/domain\//.test(ev.path || '') || (ev.state && ev.state.field === 'domain')) dirty.domains = true;
    if (ev.kind === 'request' && L.reqs.device && devicesOf(ev.path)[0] === L.reqs.device) dirty.reqs = true;
    dirty.status = true; schedule();
  }
  function addEvent(ev) {
    if (!ev || L.bySeq.has(ev.seq)) return false;
    ev._t = new Date(ev.t); L.bySeq.set(ev.seq, ev);
    const last = L.events[L.events.length - 1];
    L.events.push(ev); if (last && ev.seq < last.seq) L.events.sort((a, b) => a.seq - b.seq);
    if (L.events.length > MAX_EVENTS) for (const x of L.events.splice(0, 500)) L.bySeq.delete(x.seq);
    if (ev.seq > L.lastSeq) L.lastSeq = ev.seq;
    return true;
  }
  async function backfill(since) {
    const q = new URLSearchParams({ since: String(since), limit: '1500' }); if (!L.ui.reads) q.set('reads', 'false');
    const evs = await api('GET', '/events?' + q).catch(() => null);
    for (const ev of evs || []) addEvent(ev);
    changed();
  }
  function openStream() {
    if (L.es) L.es.close();
    const q = new URLSearchParams({ token }); if (!L.ui.reads) q.set('reads', 'false');
    const es = new EventSource('/v1/events/stream?' + q); L.es = es; L.stream = 'connecting';
    es.onopen = () => { L.stream = 'open'; backfill(L.lastSeq); };
    es.onerror = () => { L.stream = es.readyState === 2 ? 'closed' : 'reconnecting'; changed(); };
    es.onmessage = (m) => { let ev; try { ev = JSON.parse(m.data); } catch (e) { return; } if (addEvent(ev)) { onEvent(ev); changed(); } };
  }

  // ------------------------------------------------------------- connect
  async function connect(st, hooks) {
    if (!token) return false;
    try { L.status = await api('GET', '/status'); L.statusAt = Date.now(); } catch (e) { console.warn('live mode: /v1/status did not answer —', e.message); return false; }
    if (!L.status || !L.status.estate) return false;
    L.on = true; L.st = st; L.hooks = hooks;
    const [layout, cat, doms, faults, devs] = await Promise.allSettled([api('GET', '/layout'), api('GET', '/catalogue'), api('GET', '/domains'), api('GET', '/faults'), api('GET', '/devices?select=**')]);
    indexLayout(layout.value); L.catalogue = cat.value || { faults: [] }; L.domains = doms.value || []; L.faults = faults.value || [];
    putDevices(devs.value, true);
    for (const r of [layout, cat, doms, faults, devs]) if (r.status === 'rejected') toast('live mode: ' + r.reason.message, 'crit');
    await backfill(0);
    openStream();
    setInterval(pollStatus, 5000);
    let tick = 0;
    setInterval(() => { // sensors move between events: re-read what is on screen
      if (document.hidden) return; tick++;
      const r = st.route;
      const then = (diff) => { if (diff) changed(); };
      if (r.name === 'device' && r.arg) loadDevices(`${r.arg} ${r.arg}/**`).then(then, () => {});
      else if (r.name === 'rack' && r.arg) loadDevices(`${r.arg}/**`).then(then, () => {});
      else if (['floor', 'facility'].includes(r.name) && tick % 4 === 0) loadDevices('**', true).then(then, () => {});
    }, 4000);
    setInterval(() => { const el = document.getElementById('live-clock'); if (el) el.textContent = clockText(); }, 1000);
    return true;
  }

  // ------------------------------------------------------------- helpers
  const fmtV = (v) => (typeof v === 'number' ? (Number.isInteger(v) ? String(v) : v.toFixed(Math.abs(v) < 10 ? 2 : 1)) : v === undefined || v === null ? '—' : String(v));
  const fmtT = (d) => (d && !isNaN(d) ? U.fmtClock(d) : '—');
  const domSev = (s) => ({ formed: 'OK', degraded: 'Warning', unformed: 'Critical' }[s] || 'Unknown');
  const pillSev = (s) => ({ Critical: 'crit', Warning: 'warn', Unknown: 'unknown' }[s] || '');
  const devLink = (p, label) => `<a href="#/device/${encodeURIComponent(p)}" class="mono">${esc(label || U.lastSeg(p))}</a>`;
  const rackDevs = (rp) => (L.byRack[rp] || []).map((p) => L.devices[p]).filter(Boolean);
  const faultSpec = (id) => (L.catalogue.faults || []).find((f) => f.id === id);
  const faultTitle = (id) => (faultSpec(id) || {}).title || id;
  function twinNow() { const s = L.status; if (!s) return null; const base = new Date(s.now).getTime(); return new Date(s.clock === 'wall' ? base + (Date.now() - L.statusAt) : base); }
  function clockText() { const s = L.status || {}; return `${s.clock || '?'} clock ${fmtT(twinNow())} · time scale ${s.timeScale ?? '?'}`; }
  function clearsText(c) { if (!c) return 'manual'; const parts = []; const on = c.on || c.true; if (on && on.length) parts.push('on ' + on.join(', ')); if (c.after) parts.push('after ' + c.after); if (c.manual) parts.push('manually'); return parts.join('; ') || '—'; }
  function targetText(t) { t = t || {}; return t.port ? 'a port' : t.component ? `component ${t.component}${t.vendor ? ' (' + t.vendor + ')' : ''}` : t.device ? `device ${t.device}` : t.deviceWith ? `device with ${t.deviceWith}` : 'any device'; }

  // Which catalogue faults fit a target, by the target rules the catalogue publishes. The twin
  // also enforces each profile's catalogue allow-list, which the API does not expose: an inject
  // the profile does not allow comes back refused with the twin's reason.
  function fits(t, dv, comp, port) {
    t = t || {};
    if (port) return !!t.port;
    if (t.port) return false;
    if (comp) return !!t.component && comp.kind === t.component && (!t.vendor || comp.vendor === t.vendor);
    if (t.component) return false;
    if (t.device) return dv.class === t.device;
    if (t.deviceWith === 'bmc') return !!dv.dialect;
    if (t.deviceWith === 'firmware') return Object.keys(dv.firmware || {}).length > 0;
    if (t.deviceWith === 'leak-detector') return !!dv.sensors && 'leak' in dv.sensors && !['sensor', 'cdu'].includes(dv.class);
    return !t.deviceWith;
  }
  const applicable = (dv, comp, port) => (L.catalogue.faults || []).filter((f) => fits(f.target, dv, comp, port)).sort((a, b) => a.id.localeCompare(b.id));
  function targetsOf(devPath) {
    const d = L.devices[devPath]; if (!d) return [];
    const out = [{ key: 'd:' + d.path, path: d.path, label: `${d.name} — the device (${d.class})`, faults: applicable(d) }];
    for (const c of d.components || []) out.push({ key: 'c:' + c.path, path: c.path, label: `${c.name} — ${c.kind}${c.model ? ' · ' + c.model : ''}`, faults: applicable(d, c) });
    for (const cp of L.children[devPath] || []) { const ch = L.devices[cp]; if (ch) out.push({ key: 'd:' + cp, path: cp, label: `${ch.name} — child device (${ch.class})`, faults: applicable(ch) }); }
    out.push({ key: 'p:' + devPath, path: devPath, port: true, label: 'a port of this device (name it below)', faults: applicable(d, null, true) });
    return out.filter((t) => t.faults.length);
  }
  function paramInputs(spec, values, act) {
    const ps = spec && spec.params ? Object.entries(spec.params) : [];
    if (!ps.length) return '';
    return `<div class="row"><label>params</label><div>${ps.map(([k, p]) => `<div style="margin-bottom:4px"><span class="mono small" style="display:inline-block;min-width:90px">${esc(k)}</span> <input type="${p.type === 'int' || p.type === 'number' ? 'number' : 'text'}" ${p.range ? `min="${p.range[0]}" max="${p.range[1]}"` : ''} style="width:140px" data-act="${act}" data-arg="${esc(k)}" value="${esc(values[k] ?? p.default ?? '')}"> <span class="faint small">${p.range ? `range ${p.range[0]}–${p.range[1]}` : esc(p.type || '')}${p.default !== undefined ? ` · default ${esc(p.default)}` : ''}</span></div>`).join('')}</div></div>`;
  }
  function paramValues(spec, values) {
    const out = {};
    for (const [k, p] of Object.entries((spec && spec.params) || {})) { const v = values[k]; if (v === undefined || v === '') continue; out[k] = p.type === 'int' || p.type === 'number' ? Number(v) : v; }
    return out;
  }

  // ------------------------------------------------------------- frame (top bar, rail, view, drawer)
  function frame(st) {
    const r = st.route, s = L.status || {};
    const hall = (L.layout.halls || [])[0] || {};
    const crumbs = [`<a href="#/floor">${esc(L.layout.site || 'site')}</a>`, `<a href="#/floor">${esc(hall.name || '')}</a>`];
    if (r.name === 'rack' && r.arg) crumbs.push(`<b>${esc(U.lastSeg(r.arg))}</b>`);
    if (r.name === 'device' && r.arg) { const rp = L.rackOf[r.arg]; if (rp) crumbs.push(`<a href="#/rack/${encodeURIComponent(rp)}">${esc(U.lastSeg(rp))}</a>`); const d = L.devices[r.arg]; if (d && d.parent) crumbs.push(`<a href="#/device/${encodeURIComponent(d.parent)}">${esc(U.lastSeg(d.parent))}</a>`); crumbs.push(`<b>${esc(U.lastSeg(r.arg))}</b>`); }
    const inc = s.incidents || 0;
    const health = L.down ? `<span class="sev crit" title="GET /v1/status is failing">${U.SEV_MARK.Critical} twin not answering</span>`
      : `<span class="small ${inc ? '' : 'faint'}" title="twin incidents (16): a result shown while the twin has an incident is not a result">${inc ? U.sevHTML('Critical', `${inc} twin incident${inc === 1 ? '' : 's'}`) : 'no twin incidents'}</span>${L.stream === 'open' ? '' : ` <span class="sev warn" title="the event stream is ${esc(L.stream)}">${U.SEV_MARK.Warning} stream ${esc(L.stream)}</span>`}`;
    const top = `<span class="brand">dc-twin<small>console · live</small></span><span class="crumbs">${crumbs.join(' / ')}</span><span class="spacer"></span>
      <span class="live" title="This console reads a running twin through its control API (${esc(location.host)}/v1). Every act below is an API call, attributed to the control API.">live · ${esc(s.estate || '?')}</span>
      ${health}
      <span class="clock" id="live-clock" title="the twin's clock (${esc(s.clock)}); behaviour durations are scaled by the time scale">${esc(clockText())}</span>
      <button class="btn sm" data-act="report" title="the twin's fidelity report">Report</button>
      <button class="btn sm" data-act="theme" title="light / dark">◐</button>`;
    const nFaults = (L.faults || []).length;
    const items = [['floor', 'Floor', ''], ['facility', 'Facility', ''], ['faults', 'Faults', nFaults ? String(nFaults) : ''], ['events', 'Events', String(L.events.length)], ['scenarios', 'Scenarios', ''], ['report', 'Report', '']];
    const fid = Object.entries(s.fidelity || {}).map(([k, v]) => `${v} ${esc(k)}`).join(' · ');
    const rail = `${items.map(([k, l, c]) => `<a href="#/${k}" class="${r.name === k || (k === 'floor' && ['rack', 'device'].includes(r.name)) ? 'active' : ''}">${l}<span class="count">${esc(c)}</span></a>`).join('')}
      <div class="sect">estate</div><div class="small muted" style="padding:0 10px">${esc(s.estate)}<br>${Object.keys(L.racks).length} racks · ${s.devices ?? '?'} devices<br>${s.links ?? '?'} links<br>${fid}<br>adapters: ${esc((s.adapters || []).join(', ') || 'none')}</div>
      <div class="sect">about</div><div class="small faint" style="padding:0 10px">Live: a client of the running twin's control API (D16). Nothing here is simulated in the browser; drop <code>?token=</code> for the in-memory demo.</div>`;
    let view = '', drawer = '', withDrawer = false;
    switch (r.name) {
      case 'rack': view = rackView(st, r.arg); drawer = drawerView(st, r.arg); withDrawer = true; break;
      case 'device': view = deviceView(st, r.arg); drawer = drawerView(st, r.arg); withDrawer = true; break;
      case 'facility': view = facilityView(st); break;
      case 'faults': view = faultsView(st); break;
      case 'events': view = eventsView(st); break;
      case 'scenarios': view = scenariosView(st); break;
      case 'report': view = reportView(st); break;
      case 'floor': case '': view = floorView(st); drawer = drawerView(st, st.selection && L.racks[st.selection] ? st.selection : ''); withDrawer = true; break;
      default: view = `<div class="card">${esc(r.name)} is not available in live mode: the control API does not serve it yet. <a href="#/floor">Floor</a></div>`;
    }
    return { top, rail, view, drawer, withDrawer };
  }

  // ------------------------------------------------------------- floor
  const TILE = 76, GAP = 10, ROWH = 118, LABELW = 44;
  function tempRamp(t) {
    if (t === null || t === undefined) return 'var(--bg1)';
    const x = Math.max(0, Math.min(1, (t - 24) / 30));
    return `color-mix(in srgb, var(--bg1) ${Math.round(100 - x * 100)}%, var(--warn) ${Math.round(x * 100)}%)`;
  }
  const rackPower = () => { const m = {}; for (const r of (L.status && L.status.racks) || []) m[r.path] = r.powerW; return m; };
  const tempOf = (d) => (d.sensors ? d.sensors['coolant-in'] ?? d.sensors.inlet : undefined);
  // A rack's domains: its own (an NVL72's scale-up domain) and its machines' (an HGX server's).
  const rackDomains = (rp) => (L.domains || []).filter((d) => d.path.startsWith(rp + '/') && d.path.includes('/domain/'));
  const ownDomain = (rp, d) => d.path.startsWith(rp + '/domain/');
  function floorView(st) {
    const pw = rackPower(); const maxW = Math.max(1, ...Object.values(pw));
    let halls = '';
    for (const hall of L.layout.halls || []) {
      const rows = hall.rows || [];
      const maxPos = Math.max(1, ...rows.map((r) => r.positions || 0));
      const W = LABELW + maxPos * (TILE + GAP) + 20, H = rows.length * ROWH + 16;
      let svg = '';
      rows.forEach((row, ri) => {
        const y = ri * ROWH + 14;
        svg += `<text class="rowlabel" x="10" y="${y + TILE / 2 + 4}">row ${esc(row.name)}</text>`;
        for (let i = 1; i <= (row.positions || 0); i++) {
          const x = LABELW + (i - 1) * (TILE + GAP);
          const rack = (row.racks || []).find((k) => k.position === i);
          if (!rack) { svg += `<g class="empty" transform="translate(${x},${y})"><rect width="${TILE}" height="${TILE}"/></g>`; continue; }
          const devs = rackDevs(rack.path);
          const health = devs.length ? U.maxSeverity(devs.map((d) => d.health)) : 'Unknown';
          const flagged = devs.filter((d) => d.health !== 'OK').length;
          const unreach = devs.filter((d) => d.dialect && !d.bmc).length;
          let fill = 'var(--bg1)', val = '';
          if (st.overlay === 'power') { const w = pw[rack.path] || 0; const pct = w / maxW; fill = `color-mix(in srgb, var(--bg1) ${Math.round(100 - pct * 55)}%, var(--info) ${Math.round(pct * 55)}%)`; val = U.fmtKW(w); }
          if (st.overlay === 'temperature') { const t = devs.map(tempOf).filter((v) => typeof v === 'number'); const avg = t.length ? t.reduce((a, b) => a + b, 0) / t.length : null; fill = tempRamp(avg); val = avg === null ? 'no reading' : `${avg.toFixed(1)} avg`; }
          if (st.overlay === 'health') val = flagged ? `${flagged} flagged` : 'nominal';
          if (st.overlay === 'domains') { const su = rackDomains(rack.path).filter((d) => d.kind === 'scale-up'); const ok = su.filter((d) => d.state === 'formed').length; val = !su.length ? 'no domain' : su.length === 1 ? su[0].state : `${ok}/${su.length} formed`; }
          const hcls = health === 'Critical' ? 'h-crit' : health === 'Warning' ? 'h-warn' : '';
          svg += `<g class="tile ${hcls} ${st.selection === rack.path ? 'sel' : ''}" data-act="select" data-arg="${esc(rack.path)}" data-open="${esc(rack.path)}" transform="translate(${x},${y})" tabindex="0">
            <rect class="body" width="${TILE}" height="${TILE}" style="fill:${fill}"/>
            <text x="8" y="18" font-weight="600">${esc(rack.name)}</text>
            <text class="tag" x="8" y="33">${esc(U.lastSeg(rack.format || ''))}</text>
            ${health !== 'OK' ? `<text class="mark ${U.sevClass(health)}" x="${TILE - 16}" y="18">${U.SEV_MARK[health] || '?'}</text>` : ''}
            ${unreach ? `<text class="mark warn" x="${TILE - 30}" y="18">⊘</text>` : ''}
            <text class="val" x="8" y="${TILE - 10}">${esc(val.length > 13 ? val.slice(0, 12) + '…' : val)}</text><title>${esc(rack.name)} · ${esc(rack.format)} · ${devs.length} devices · ${esc(val)}${unreach ? ` · ${unreach} BMC unavailable` : ''}</title>
          </g>`;
          if (st.overlay === 'domains' && rackDomains(rack.path).some((d) => d.kind === 'scale-up' && ownDomain(rack.path, d))) svg += `<rect class="dom" x="${x - 4}" y="${y - 4}" width="${TILE + 8}" height="${TILE + 8}"/>`;
        }
      });
      halls += `<div class="card" style="padding:10px 12px;overflow:auto"><div class="small faint">${esc(hall.path)}</div><svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" role="img" aria-label="hall floor plan">${svg}</svg></div>`;
    }
    const s = L.status || {};
    const feeds = Object.entries(s.feeds || {}).map(([k, up]) => `<span>feed ${esc(k)} ${up ? '<b>up</b>' : U.sevHTML('Critical', 'down')}</span>`).join(' · ');
    const cdus = Object.values(L.devices).filter((d) => d.class === 'cdu').map((d) => `<span>${devLink(d.path, d.name)} ${U.sevHTML(d.health)} supply <b>${fmtV(d.sensors['supply-temp'])}</b> return <b>${fmtV(d.sensors['return-temp'])}</b> flow <b>${fmtV(d.sensors.flow)}</b></span>`).join(' · ');
    const sel = st.selection && L.racks[st.selection];
    const selHealth = sel ? U.maxSeverity(rackDevs(sel.path).map((d) => d.health)) : 'OK';
    return `
      <div class="toolbar">
        <span class="muted">overlay</span>
        <div class="seg">${['health', 'power', 'temperature', 'domains'].map((o) => `<button class="${st.overlay === o ? 'on' : ''}" data-act="overlay" data-arg="${o}">${o}</button>`).join('')}</div>
        <span class="grow"></span>
        ${sel ? `<span class="path">${esc(sel.name)} · ${esc(sel.format)} · ${U.sevHTML(selHealth)}</span> <button class="btn sm primary" data-act="open-rack" data-arg="${esc(sel.path)}">Open rack</button> <button class="btn sm" data-act="inject-modal" data-arg="${esc(sel.path)}">Fault ▾</button> <button class="btn sm" data-act="select" data-arg="">clear</button>` : '<span class="faint">click a rack to select it, double-click to open</span>'}
      </div>
      ${halls || '<div class="card">the layout is empty</div>'}
      <div class="toolbar" style="margin-top:10px;font-size:12px">${feeds} ${cdus ? `<span class="faint">·</span> ${cdus}` : ''}</div>
      <div class="legend">${st.overlay === 'health' ? `<span>${U.sevHTML('OK', 'nominal (grey, not green)')}</span><span>${U.sevHTML('Warning', 'warning')}</span><span>${U.sevHTML('Critical', 'critical')}</span><span class="sev warn"><span class="mark">⊘</span> BMC unavailable</span>` : st.overlay === 'power' ? '<span>fill = the rack\'s draw (GET /v1/status) relative to the busiest rack; the control API does not publish rack capacity</span>' : st.overlay === 'temperature' ? '<span>fill = mean coolant-in (liquid) or inlet (air) reading of the rack\'s devices</span>' : '<span><i style="border:1.5px dashed var(--unknown)"></i>rack-scale scale-up domain (NVLink); the value is the state of the rack\'s scale-up domains, its own or its servers\' (GET /v1/domains)</span>'}</div>`;
  }

  // ------------------------------------------------------------- rack
  const UH = 13, RW = 320, LX = 70;
  function elevation(st, rack) {
    const Hh = rack.height || 42, H = Hh * UH + 24;
    const yTop = (u, units) => 12 + (Hh - (u + Math.max(1, units) - 1)) * UH; // u counts from the bottom (U1)
    let svg = `<rect x="${LX}" y="12" width="${RW}" height="${Hh * UH}" fill="none" stroke="var(--line-strong)"/>`;
    for (let k = 0; k < Hh; k += 6) svg += `<text class="axis" x="${LX - 6}" y="${12 + k * UH + 8}" text-anchor="end">U${Hh - k}</text>`;
    const slotted = rack.devices.filter((d) => d.units > 0 && d.side !== 'zero-u' && d.u > 0).sort((a, b) => b.u - a.u);
    const bands = []; let cur = null;
    for (const d of slotted) { if (cur && cur.slot === d.slot && cur.bottom === d.u + d.units) { cur.bottom = d.u; cur.n++; } else { cur = { slot: d.slot, top: d.u + d.units - 1, bottom: d.u, n: 1 }; bands.push(cur); } }
    for (const b of bands.filter((x) => x.n > 1)) { const y = 12 + (Hh - b.top) * UH, h = (b.top - b.bottom + 1) * UH; svg += `<g class="band"><rect x="${LX - 2}" y="${y - 1}" width="${RW + 4}" height="${h + 2}"/><text x="${LX + RW + 8}" y="${y + h / 2 + 3}">${esc(b.slot)} ×${b.n}</text></g>`; }
    for (const d of slotted) {
      const v = L.devices[d.path] || {}; const y = yTop(d.u, d.units), h = d.units * UH;
      const health = v.health || 'Unknown', power = v.power || '?';
      const ov = st.overlay === 'power' ? (v.drawW !== undefined ? U.fmtKW(v.drawW) : '') : st.overlay === 'temperature' ? (typeof tempOf(v) === 'number' ? `${fmtV(tempOf(v))}` : '') : '';
      svg += `<g class="slot ${st.selection === d.path ? 'sel' : ''} ${power === 'Off' ? 'off' : ''}" data-act="select" data-arg="${esc(d.path)}" data-open="${esc(d.path)}"><rect x="${LX}" y="${y}" width="${RW}" height="${h}"/>
        <text class="hm ${U.sevClass(health)}" x="${LX + 4}" y="${y + h / 2 + 3}">${health !== 'OK' ? U.SEV_MARK[health] || '?' : '·'}</text>
        <text x="${LX + 16}" y="${y + h / 2 + 3.5}" style="font-size:10px">${esc(d.name)}</text>
        ${ov ? `<text x="${LX + RW - 110}" y="${y + h / 2 + 3.5}">${esc(ov)}</text>` : ''}
        ${power !== 'On' ? `<text class="hm" x="${LX + RW - 56}" y="${y + h / 2 + 3}">${esc(String(power).toLowerCase())}</text>` : ''}
        ${d.fidelity && d.fidelity !== 'model' ? `<text class="fb" x="${LX + RW - 22}" y="${y + h / 2 + 3}">${esc(d.fidelity)}</text>` : ''}<title>${esc(d.name)} · U${d.u}${d.units > 1 ? '–' + (d.u + d.units - 1) : ''} · ${esc(d.profile)} · ${esc(health)} · ${esc(power)}</title></g>`;
    }
    for (const d of rack.devices.filter((x) => x.class === 'sensor' && x.u > 0)) { const v = L.devices[d.path] || {}; const leak = v.sensors && v.sensors.leak > 0; const y = 12 + (Hh - d.u) * UH; svg += `<polygon class="leak ${leak ? 'on' : ''}" data-act="select" data-arg="${esc(d.path)}" style="cursor:pointer" points="${LX - 14},${y} ${LX - 4},${y + 4.5} ${LX - 14},${y + 9}"><title>${esc(d.name)} at U${d.u}${leak ? ' — LEAK' : ''}</title></polygon>`; }
    rack.devices.filter((x) => x.side === 'zero-u').forEach((d, i) => { svg += `<g class="zero ${st.selection === d.path ? 'sel' : ''}" data-act="select" data-arg="${esc(d.path)}" data-open="${esc(d.path)}" style="cursor:pointer"><rect x="${LX + RW + 110 + i * 16}" y="12" width="12" height="${Hh * UH}"/><text x="${LX + RW + 119 + i * 16}" y="20">${esc(d.name)}</text></g>`; });
    return `<svg viewBox="0 0 ${LX + RW + 160} ${H}" width="${LX + RW + 160}" height="${H}" role="img" aria-label="rack elevation">${svg}</svg>`;
  }
  function rackView(st, rp) {
    const rack = L.racks[rp];
    if (!rack) return `<div class="card">rack not found: <code>${esc(rp)}</code></div>`;
    const devs = rackDevs(rp); const health = devs.length ? U.maxSeverity(devs.map((d) => d.health)) : 'Unknown';
    const doms = rackDomains(rp); const w = rackPower()[rp];
    const fid = {}; for (const d of devs) fid[d.fidelity] = (fid[d.fidelity] || 0) + 1;
    const sel = st.selection && L.devices[st.selection] && L.rackOf[st.selection] === rp ? st.selection : null;
    return `
      <div class="toolbar"><h2>${esc(rack.name)} <span class="muted">· <code>${esc(rack.format)}</code> · ${esc(rack.archetype)} · ${rack.height}U</span></h2>
        ${doms.filter((d) => ownDomain(rp, d)).map((d) => `<span class="pill ${pillSev(domSev(d.state))}" title="${esc(d.path)}">${esc(d.kind)}${d.technology ? ' ' + esc(d.technology) : ''}: ${esc(d.state)}</span>`).join(' ')}
        <span class="grow"></span>
        <span class="muted">overlay</span><div class="seg">${['health', 'power', 'temperature'].map((o) => `<button class="${st.overlay === o ? 'on' : ''}" data-act="overlay" data-arg="${o}">${o}</button>`).join('')}</div>
        <button class="btn sm" data-act="inject-modal" data-arg="${esc(rp)}">Fault ▾</button>
      </div>
      <div class="grid side">
        <div class="card" style="overflow:auto">${elevation(st, rack)}</div>
        <div>
          <div class="card"><h3>rack</h3><dl class="kv">
            <dt>power</dt><dd class="mono">${w !== undefined ? U.fmtKW(w) : '—'} <span class="faint small">drawn now</span></dd>
            <dt>devices</dt><dd>${rack.devices.length} in the layout${devs.length > rack.devices.length ? ` + ${devs.length - rack.devices.length} child` : ''} · ${Object.entries(fid).map(([k, n]) => `${n} at ${U.fidBadge(k)}`).join(', ')}</dd>
            <dt>health</dt><dd>${U.sevHTML(health, health)} ${devs.filter((d) => d.health !== 'OK').map((d) => devLink(d.path, d.path.slice(rp.length + 1))).join(', ')}</dd>
            <dt>domains</dt><dd>${doms.length ? doms.map((d) => `${U.sevHTML(domSev(d.state), `${ownDomain(rp, d) ? '' : d.path.slice(rp.length + 1).split('/domain/')[0] + ' '}${d.kind}: ${d.state}`)} <span class="faint small">${d.members} members${d.manager ? ' · manager ' + esc(U.lastSeg(d.manager)) : ''}</span>`).join('<br>') : '—'}</dd>
          </dl></div>
          ${sel ? deviceCard(st, sel) : '<div class="card"><span class="faint">select a slot to see the device; double-click to open it</span></div>'}
        </div>
      </div>`;
  }
  function powerMenu(d) {
    const items = d.power === 'Off' ? [['on', 'Power on'], ['ac-cycle', 'AC cycle (technician)']] : [['graceful', 'Graceful shutdown'], ['off', 'Force off'], ['restart', 'Force restart'], ['cycle', 'Power cycle'], ['ac-cycle', 'AC cycle (technician)']];
    return `<span class="menu"><button class="btn sm" data-act="menu">Power ▾</button><div class="dd"><div class="hint">POST /v1/power — as a technician at the BMC, shelf or PDU would</div>${items.map(([a, l]) => `<button data-act="l-power" data-arg="${esc(d.path)}|${a}">${l}</button>`).join('')}</div></span>`;
  }
  function faultPills(d) { return (d.faults || []).map((f) => `<span class="pill ${pillSev(d.health)}" title="${esc(f.id)} on ${esc(f.target)}">${esc(faultTitle(f.fault))}</span>`).join(' '); }
  function deviceCard(st, path) {
    const d = L.devices[path]; if (!d) return '';
    const sensors = Object.entries(d.sensors || {}).slice(0, 4).map(([k, v]) => `${esc(k)} <b class="mono">${fmtV(v)}</b>`).join(' · ');
    return `<div class="card"><h3>${esc(d.name)} <span class="faint">· ${esc(d.profile)}</span></h3><dl class="kv">
      <dt>health</dt><dd>${U.sevHTML(d.health, d.health)} ${faultPills(d)}</dd>
      <dt>power</dt><dd>${esc(d.power)} · ${U.fmtKW(d.drawW || 0)}${d.ac === false ? ` · ${U.sevHTML('Warning', 'no AC')}` : ''}</dd>
      <dt>fidelity</dt><dd>${U.fidBadge(d.fidelity)}</dd>
      ${d.dialect ? `<dt>bmc</dt><dd>${d.bmc ? 'available' : U.sevHTML('Warning', 'unavailable')} · lease <span class="mono">${esc(d.lease || '—')}</span> · <span class="mono small">${esc(d.dialect)}</span></dd>` : ''}
      <dt>sensors</dt><dd class="small">${sensors || '—'}</dd></dl>
      <div class="toolbar" style="margin:10px 0 0"><a class="btn sm primary" href="#/device/${encodeURIComponent(path)}">Open</a>${d.class !== 'sensor' ? powerMenu(d) : ''}<button class="btn sm" data-act="inject-modal" data-arg="${esc(path)}">Fault ▾</button></div></div>`;
  }

  // ------------------------------------------------------------- device
  const TABS = ['overview', 'components', 'sensors', 'firmware', 'faults', 'interfaces', 'cables'];
  function deviceView(st, path) {
    const d = L.devices[path];
    if (!d) return `<div class="card">device not found: <code>${esc(path)}</code></div>`;
    const tab = TABS.includes(st.tab) ? st.tab : 'overview';
    const rp = L.rackOf[path];
    return `
      <div class="toolbar">
        <div><div class="path">${rp ? `<a href="#/rack/${encodeURIComponent(rp)}">${esc(U.lastSeg(rp))}</a> / ` : ''}${d.parent ? `${devLink(d.parent)} / ` : ''}<b>${esc(d.name)}</b> <span class="faint">${esc(path)}</span></div>
          <h2>${esc(d.name)} <span class="muted">· <code>${esc(d.profile)}</code> · ${esc(d.class)}</span></h2></div>
        <span class="grow"></span>
        ${U.fidBadge(d.fidelity)} ${U.sevHTML(d.health, d.health)} <span class="pill">${esc(d.power)} · ${U.fmtKW(d.drawW || 0)}</span>
        ${d.class !== 'sensor' ? powerMenu(d) : ''}
        <button class="btn sm" data-act="inject-modal" data-arg="${esc(path)}">Fault ▾</button>
      </div>
      <div class="tabs">${TABS.map((t) => `<button class="${t === tab ? 'on' : ''}" data-act="tab" data-arg="${t}">${t === 'cables' ? 'ports & cables' : t}</button>`).join('')}</div>
      ${TAB[tab](st, d)}`;
  }
  const TAB = {
    overview(st, d) {
      const rp = L.rackOf[d.path]; const kids = L.children[d.path] || [];
      const active = (d.faults || []).map((f) => { const fv = (L.faults || []).find((x) => x.ID === f.id) || {}; return `<div><span class="pill ${pillSev(d.health)}">${esc(faultTitle(f.fault))}</span> <span class="faint mono small">${esc(f.fault)}</span> on <span class="mono">${esc(f.target.slice(d.path.length + 1) || 'device')}</span>${fv.Since ? ` since ${fmtT(new Date(fv.Since))}` : ''} <span class="faint small">${esc(f.id)}</span> <button class="btn sm" data-act="l-clear" data-arg="${esc(f.id)}">clear</button></div>`; }).join('');
      const ep = d.endpoint || {}; const boot = d.boot || {};
      return `<div class="grid c2">
        <div class="card"><h3>identity & place</h3><dl class="kv">
          <dt>serial</dt><dd class="mono">${esc(d.identity && d.identity.serial)}</dd><dt>uuid</dt><dd class="mono">${esc(d.identity && d.identity.uuid)}</dd>
          ${d.identity && d.identity.bmcMAC ? `<dt>bmc mac</dt><dd class="mono">${esc(d.identity.bmcMAC)}</dd>` : ''}
          <dt>rack / slot</dt><dd>${rp ? `<a href="#/rack/${encodeURIComponent(rp)}">${esc(U.lastSeg(rp))}</a> · ` : ''}${esc(d.slot || '—')} · ${d.units > 0 ? `U${d.u}${d.units > 1 ? '–' + (d.u + d.units - 1) : ''}` : d.class === 'sensor' && d.u ? `at the level of U${d.u}` : rp ? 'zero-U' : 'facility'}</dd>
          ${d.parent ? `<dt>inside</dt><dd>${devLink(d.parent, U.shortPath(d.parent))}</dd>` : ''}
          ${kids.length ? `<dt>child devices</dt><dd>${kids.map((k) => devLink(k)).join(', ')}</dd>` : ''}
          ${d.generated ? `<dt>generated</dt><dd>by fabric <span class="mono">${esc(d.generated)}</span></dd>` : ''}
          <dt>fidelity</dt><dd>${U.fidBadge(d.fidelity)}</dd>
        </dl></div>
        <div class="card"><h3>state</h3><dl class="kv">
          <dt>health</dt><dd>${U.sevHTML(d.health, d.health)} <span class="small muted">the twin's roll-up of components, sensors and faults</span></dd>
          <dt>power</dt><dd>${esc(d.power)} · ${U.fmtKW(d.drawW || 0)} · AC ${d.ac ? 'present' : U.sevHTML('Warning', 'absent')}${d.booted ? ` · booted <span class="mono">${esc(d.booted)}</span>` : ''}${d.os ? ` · os <span class="mono">${esc(d.os)}</span>` : ''}</dd>
          <dt>utilisation</dt><dd><input type="number" min="0" max="1" step="0.05" style="width:80px" data-act="l-util" value="${esc(L.ui.util)}" placeholder="0–1"> <button class="btn sm" data-act="l-util-set" data-arg="${esc(d.path)}">set</button> <span class="faint small">POST /v1/utilisation</span></dd>
          <dt>active faults</dt><dd>${active || '<span class="faint">none</span>'}</dd>
          ${d.dialect ? `<dt>bmc</dt><dd>${d.bmc ? 'available' : U.sevHTML('Warning', 'unavailable')} · <span class="mono">${esc(d.dialect)}</span></dd>
          <dt>lease</dt><dd class="mono">${esc(d.lease || '— (no lease)')}</dd>
          <dt>endpoints</dt><dd class="mono small">${ep.siteIP ? `site ${esc(ep.siteIP)}` : ''}${ep.redfish ? ` · redfish ${esc(ep.redfish)}` : ''}${ep.ipmi ? ` · ipmi ${esc(ep.ipmi)}` : ''}${ep.snmp ? ` · snmp ${esc(ep.snmp)}` : ''}</dd>` : '<dt>bmc</dt><dd>none</dd>'}
          ${boot.order && boot.order.length ? `<dt>boot</dt><dd class="small">override ${esc(boot.overrideTarget || 'None')} (${esc(boot.overrideEnabled || 'Disabled')}) · order <span class="mono">${esc(boot.order.slice(0, 4).join(', '))}${boot.order.length > 4 ? ', …' : ''}</span></dd>` : ''}
          <dt>security</dt><dd class="small">secure boot ${d.secureBoot ? 'on' : 'off'} · lockdown ${d.lockdown ? 'on' : 'off'}</dd>
        </dl></div></div>`;
    },
    components(st, d) {
      return `<div class="card"><table><thead><tr><th>component</th><th>kind</th><th>model</th><th>vendor</th><th>health</th><th>state</th><th>counters</th><th></th></tr></thead><tbody>
        ${(d.components || []).map((c) => `<tr><td class="mono">${esc(c.name)}</td><td>${esc(c.kind)}</td><td>${esc(c.model)}</td><td>${esc(c.vendor)}</td><td>${U.sevHTML(c.health, c.health)}</td><td>${esc(c.state)}</td><td class="mono small">${c.counters && Object.keys(c.counters).length ? esc(JSON.stringify(c.counters)) : ''}</td><td>${applicable(d, c).length ? `<button class="btn sm" data-act="inject-modal" data-arg="${esc(c.path)}">Fault ▾</button>` : ''}</td></tr>`).join('') || '<tr><td colspan="8" class="faint">no components</td></tr>'}
      </tbody></table></div>`;
    },
    sensors(st, d) {
      const s = Object.entries(d.sensors || {});
      return `<div class="card"><table><thead><tr><th>sensor</th><th class="num">reading</th></tr></thead><tbody>
        ${s.map(([k, v]) => `<tr><td class="mono">${esc(k)}</td><td class="num">${fmtV(v)}</td></tr>`).join('') || '<tr><td colspan="2" class="faint">no sensors</td></tr>'}
      </tbody></table><p class="note">Readings as the twin derives them now. The device view carries values only: units and thresholds live in the profile, which the control API does not serve; health above already accounts for them.</p></div>`;
    },
    firmware(st, d) {
      const fw = Object.entries(d.firmware || {});
      return `<div class="card"><table><thead><tr><th>component</th><th>version</th><th>baseline</th><th>status</th></tr></thead><tbody>
        ${fw.map(([k, f]) => `<tr><td class="mono">${esc(k)}</td><td class="mono">${esc(f.version)}${f.staged ? ` <span class="pill info">staged → ${esc(f.staged)}</span>` : ''}</td><td class="mono">${esc(f.baseline)}</td><td>${f.version === f.baseline ? 'in baseline' : U.sevHTML('Warning', 'out of baseline')}</td></tr>`).join('') || '<tr><td colspan="4" class="faint">no firmware inventory</td></tr>'}
      </tbody></table></div>`;
    },
    faults(st, d) {
      const active = d.faults || [];
      const targets = targetsOf(d.path);
      return `<div class="grid c2">
        <div class="card"><h3>active on this device</h3>${active.length ? `<table><tbody>${active.map((f) => `<tr><td><span class="pill ${pillSev(d.health)}">${esc(faultTitle(f.fault))}</span></td><td class="mono small">${esc(f.target.slice(d.path.length + 1) || 'device')}</td><td class="small faint">${esc(f.id)}</td><td><button class="btn sm" data-act="l-clear" data-arg="${esc(f.id)}">clear</button></td></tr>`).join('')}</tbody></table>` : '<span class="faint">none</span>'}</div>
        <div class="card"><h3>catalogue, filtered to this device</h3>${targets.map((t) => `<div class="small muted" style="margin-top:6px">${esc(t.label)}</div>${t.faults.map((f) => `<div>${t.port ? '' : `<button class="btn sm" data-act="l-inject" data-arg="${esc(f.id)}|${esc(t.path)}" title="inject with default params">inject</button> `}<button class="btn sm" data-act="inject-modal" data-arg="${esc(t.port ? d.path : t.path)}" data-key="${esc(t.key)}" data-fault="${esc(f.id)}">…</button> ${esc(f.title)} <span class="faint mono small">${esc(f.id)}</span></div>`).join('')}`).join('') || '<span class="faint">no applicable faults</span>'}
          <p class="note">Filtered by each fault's published target. The twin also applies the profile's catalogue allow-list, which the API does not publish; a refused inject says why.</p></div></div>`;
    },
    interfaces(st, d) {
      if (!d.dialect) return `<div class="card"><h3>interfaces</h3><p>This device has no BMC, so there is no Redfish service to inspect.${d.class === 'sensor' || d.class === 'cdu' ? ' Its readings reach the stack as BMS points (the bms adapter).' : ''}</p></div>`;
      ensureRF(d.path); ensureReqs(d.path);
      const rf = L.rf, q = L.reqs;
      const body = rf.loading ? '<span class="faint">reading…</span>' : rf.err ? `<span class="bad">${esc(rf.err)}</span>` : rf.body !== null ? `<pre class="rfjson">${rfJSON(rf.body, '')}</pre>` : '';
      return `<div class="grid c2">
        <div class="card"><h3>Redfish — as the stack sees it <span class="faint">(${esc(d.dialect)})</span></h3>
          <div class="toolbar" style="margin-bottom:8px"><button class="btn sm" data-act="l-rf-back" ${rf.hist.length ? '' : 'disabled'}>← back</button><button class="btn sm" data-act="rf" data-arg="/redfish/v1">root</button>
            <input type="text" class="mono" style="flex:1;min-width:180px" data-act="l-rf-path" value="${esc(rf.path || '/redfish/v1')}"> <button class="btn sm" data-act="l-rf-go">GET</button>
            ${rf.status ? `<span class="small ${rf.status >= 400 ? 'warnc' : 'faint'}">HTTP ${rf.status}</span>` : ''}</div>
          ${body}
          <p class="note">Read through the redfish adapter by <code>GET /v1/inspect/redfish</code>, with the device's own credentials. Click an <code>@odata.id</code> to follow it.</p></div>
        <div class="card"><h3>requests made to this device <span class="faint">(${q.list.length})</span> <button class="btn sm" data-act="l-reqs">refresh</button></h3>
          ${q.err ? `<span class="bad">${esc(q.err)}</span>` : q.list.length ? `<table><thead><tr><th>time</th><th>method</th><th>target</th><th class="num">status</th><th>canonical</th><th>from</th></tr></thead><tbody>${q.list.slice().reverse().slice(0, 120).map((e) => { const r = e.request || {}; return `<tr><td class="mono small">${fmtT(new Date(e.t))}</td><td class="mono">${esc(r.method)}</td><td class="mono small">${esc(r.target)}</td><td class="num ${r.status >= 400 || !r.status ? 'warnc' : ''}">${esc(r.status || '—')}</td><td class="mono small">${esc(r.canonical)}</td><td class="small">${esc(evSrc(e))}</td></tr>`; }).join('')}</tbody></table>` : `<span class="faint">${q.loading ? 'reading…' : 'none recorded'}</span>`}
          <p class="note">From <code>GET /v1/events?select=…&amp;kind=request</code>. The inspector's own reads pass through the same adapter, so they are listed here too.</p></div></div>`;
    },
    cables(st, d) {
      const acts = L.events.filter((e) => e.kind === 'act' && e.state && e.state.field === 'link' && inScope(e.path, d.path));
      const latest = new Map(); for (const e of acts) latest.set(e.path, e);
      return `<div class="grid c2"><div class="card"><h3>pull or replace a cable</h3>
        <div class="toolbar"><span class="mono small">${esc(U.shortPath(d.path))}:</span><input type="text" class="mono" style="width:140px" data-act="l-port" value="${esc(L.ui.port)}" placeholder="port, e.g. bmc0">
          <button class="btn sm danger" data-act="l-cable-port" data-arg="${esc(d.path)}|pull">Pull</button><button class="btn sm" data-act="l-cable-port" data-arg="${esc(d.path)}|replace">Replace</button></div>
        <p class="note">POST /v1/cable with the port ref <code>device:port</code>. The control API does not list a device's ports or links yet, so name the port as its profile does.</p></div>
        <div class="card"><h3>cable acts on this device</h3>${latest.size ? `<table><tbody>${[...latest.values()].map((e) => `<tr><td class="mono small" title="${esc(e.path)}">${esc(e.path.split('~').map((p) => U.shortPath(p)).join(' ↔ '))}</td><td>${e.state.to ? 'in' : U.sevHTML('Warning', 'pulled')}</td><td><button class="btn sm" data-act="l-cable" data-arg="${esc(e.path)}|${e.state.to ? 'pull' : 'replace'}">${e.state.to ? 'pull' : 'replace'}</button></td></tr>`).join('')}</tbody></table>` : '<span class="faint">none in this console\'s event buffer</span>'}</div></div>`;
    },
  };

  // Redfish JSON with each @odata.id a link the inspector can follow.
  function rfJSON(v, ind) {
    const pad = ind + '  ';
    if (Array.isArray(v)) return v.length ? '[\n' + v.map((x) => pad + rfJSON(x, pad)).join(',\n') + '\n' + ind + ']' : '[]';
    if (v && typeof v === 'object') {
      const ks = Object.keys(v);
      return ks.length ? '{\n' + ks.map((k) => `${pad}${esc(JSON.stringify(k))}: ${(k === '@odata.id' || /odata\.nextLink$/.test(k)) && typeof v[k] === 'string' ? `<a href="#" data-act="rf" data-arg="${esc(v[k])}">${esc(JSON.stringify(v[k]))}</a>` : rfJSON(v[k], pad)}`).join(',\n') + '\n' + ind + '}' : '{}';
    }
    return esc(JSON.stringify(v));
  }
  async function loadRF(device, path, push) {
    if (push && L.rf.device === device && L.rf.path && L.rf.path !== path) L.rf.hist.push(L.rf.path);
    Object.assign(L.rf, { device, path, loading: true, err: null, status: 0 });
    render();
    try {
      const r = await fetch(`/v1/inspect/redfish?device=${encodeURIComponent(device)}&path=${encodeURIComponent(path)}`, { headers: { Authorization: 'Bearer ' + token } });
      const text = await r.text(); let body; try { body = JSON.parse(text); } catch (e) { body = text; }
      if (r.status === 404 && body && body.error && !body['@odata.id']) Object.assign(L.rf, { err: body.error, body: null });
      else Object.assign(L.rf, { body, status: r.status });
    } catch (e) { L.rf.err = e.message; }
    L.rf.loading = false; changed();
    loadReqs(device).then(changed);
  }
  function ensureRF(device) { if (L.rf.device !== device) { L.rf.hist = []; L.rf.body = null; setTimeout(() => loadRF(device, '/redfish/v1', false), 0); L.rf.device = device; L.rf.path = '/redfish/v1'; L.rf.loading = true; } }
  async function loadReqs(device) {
    L.reqs.device = device; L.reqs.loading = true;
    try { L.reqs.list = (await api('GET', `/events?select=${encodeURIComponent(device)}&kind=request&limit=400`)) || []; L.reqs.err = null; } catch (e) { L.reqs.err = e.message; }
    L.reqs.loading = false;
  }
  function ensureReqs(device) { if (L.reqs.device !== device) { L.reqs.list = []; L.reqs.device = device; loadReqs(device).then(changed); } }

  // ------------------------------------------------------------- events
  const isRead = (e) => !!(e.request && /\.read$/.test(e.request.canonical || ''));
  const evSrc = (e) => { const s = e.source || {}; return [s.class, s.principal || s.address].filter(Boolean).join(':'); };
  function show(x) { return x === undefined || x === null || x === '' ? '∅' : typeof x === 'object' ? JSON.stringify(x) : String(x); }
  function evMsg(e) {
    if (e.request) { const q = e.request; return `${q.protocol} ${q.method || ''} ${q.target || ''}${q.status ? ' → ' + q.status : ''} · ${q.canonical}`; }
    if (e.state) { const s = e.state; const m = `${s.field}: ${s.from !== undefined ? show(s.from) + ' → ' : ''}${show(s.to)}`; return e.message ? `${e.message} · ${m}` : m; }
    return e.message || (e.data ? JSON.stringify(e.data) : '');
  }
  function selMatch(sel, path) {
    return String(path || '').split('~').some((part) => { const b = part.split(':')[0]; return U.matchSelector(sel, b) || U.matchSelector(sel + '/**', b); });
  }
  // Cause chains: an event whose cause is visible is listed under it, indented.
  function tree(evs) {
    const vis = new Set(evs.map((e) => e.seq)); const kids = new Map(); const roots = [];
    for (const e of evs) { if (e.cause && e.cause < e.seq && vis.has(e.cause)) { if (!kids.has(e.cause)) kids.set(e.cause, []); kids.get(e.cause).push(e); } else roots.push(e); }
    const out = [];
    const walk = (e, d) => { out.push([e, d]); for (const k of kids.get(e.seq) || []) walk(k, d + 1); };
    for (const r of roots) walk(r, 0);
    return out;
  }
  function evRow(e, depth) {
    const sev = e.severity === 'Critical' ? 'sev-crit' : e.severity === 'Warning' ? 'sev-warn' : '';
    return `<div class="evrow ${sev}" data-seq="${e.seq}" title="seq ${e.seq}${e.cause ? ' · caused by ' + e.cause : ''}${e.severity ? ' · ' + esc(e.severity) : ''}"><span class="t">${fmtT(e._t)}</span><span class="kind ${esc(e.kind)}">${esc(e.kind)}</span><span class="p"><a href="#" data-act="goto" data-arg="${esc(e.path)}" title="${esc(e.path)}">${esc(U.shortPath(e.path || ''))}</a></span><span class="m">${depth ? `<span class="ind">${'   '.repeat(depth - 1)}↳ </span>` : ''}${esc(evMsg(e))}</span><span class="src" title="${esc(evSrc(e))}">${esc(evSrc(e))}</span></div>`;
  }
  function eventRows(st, { scope, sel, limit }) {
    const hide = L.ui.hideKinds;
    const evs = L.events.filter((e) => !hide.has(e.kind) && (L.ui.reads || !isRead(e)) && (!scope || inScope(e.path, scope)) && (!sel || selMatch(sel, e.path)));
    const rows = tree(evs.slice(-limit)).map(([e, d]) => evRow(e, d));
    return { html: rows.join('') || '<div class="evrow"><span></span><span></span><span></span><span class="faint">no events match</span></div>', total: evs.length };
  }
  function drawerView(st, scope) {
    const { html, total } = eventRows(st, { scope, limit: 200 });
    return `<div class="dhead"><b>events</b> <span class="faint">${total} · ${scope ? esc(U.shortPath(scope)) + '/**' : 'whole estate'} · GET /v1/events/stream${L.ui.reads ? '' : ', reads hidden'}</span>
      <span class="grow"></span>
      <label class="chk"><input type="checkbox" data-act="ev-follow" ${st.ev.follow ? 'checked' : ''}> follow</label>
      <label class="chk" title="show requests whose canonical name ends in .read"><input type="checkbox" data-act="l-ev-reads" ${L.ui.reads ? 'checked' : ''}> reads</label>
      <a href="#/events">open events →</a></div>
      <div class="dbody" id="drawer-body">${html}</div>`;
  }
  function eventsView(st) {
    const kinds = [...new Set(L.events.map((e) => e.kind))].sort();
    const sel = (st.ev.sel || '').trim();
    const { html, total } = eventRows(st, { sel, limit: 800 });
    return `<div class="toolbar"><h2>Events</h2><span class="muted">${L.events.length} in this console's buffer · last seq ${L.lastSeq} · stream ${esc(L.stream)}</span></div>
      <div class="toolbar"><input type="text" class="mono" style="width:260px" data-act="ev-sel" value="${esc(st.ev.sel)}" placeholder="selector filter, e.g. a01/**"> <button class="btn sm" data-act="ev-apply">apply</button>
        <span class="faint">kinds</span> ${kinds.map((k) => `<button class="btn sm ${L.ui.hideKinds.has(k) ? '' : 'primary'}" data-act="l-ev-kind" data-arg="${esc(k)}">${esc(k)}</button>`).join('')}
        <label class="chk"><input type="checkbox" data-act="l-ev-reads" ${L.ui.reads ? 'checked' : ''}> reads</label><label class="chk"><input type="checkbox" data-act="ev-follow" ${st.ev.follow ? 'checked' : ''}> follow</label></div>
      <div class="card" style="padding:0"><div class="dbody" id="events-body" style="max-height:calc(100vh - 230px);overflow:auto">${html}</div><div class="small faint" style="padding:6px 12px">${total} matching; showing the last ${Math.min(800, total)}. Indented rows are effects of the row above (the event's <code>cause</code>).</div></div>`;
  }

  // ------------------------------------------------------------- facility, faults
  function facilityView(st) {
    const s = L.status || {};
    const cdus = Object.values(L.devices).filter((d) => d.class === 'cdu');
    const links = new Map(); for (const e of L.events) if (e.kind === 'act' && e.state && e.state.field === 'link') links.set(e.path, e);
    return `<div class="toolbar"><h2>Facility</h2><span class="muted">feeds, cooling, domains and cabling acts on the running twin</span></div>
      <div class="grid c2"><div>
        <div class="card"><h3>power feeds</h3><table><tbody>${Object.entries(s.feeds || {}).map(([k, up]) => `<tr><td class="mono">${esc(k)}</td><td>${up ? 'up' : U.sevHTML('Critical', 'down')}</td><td><button class="btn sm ${up ? 'danger' : ''}" data-act="l-feed" data-arg="${esc(k)}|${up ? 'down' : 'up'}">${up ? 'take down' : 'bring up'}</button></td></tr>`).join('') || '<tr><td class="faint">no feeds</td></tr>'}</tbody></table><p class="note">POST /v1/feeds. A feed going down takes every PSU on it with it; redundancy decides what stays up.</p></div>
        <div class="card"><h3>cooling</h3>${cdus.length ? `<table><tbody>${cdus.map((d) => `<tr><td>${devLink(d.path, U.shortPath(d.path))}</td><td>${U.sevHTML(d.health, d.health)}</td><td class="small mono">${Object.entries(d.sensors || {}).map(([k, v]) => `${esc(k)} ${fmtV(v)}`).join(' · ')}</td><td><button class="btn sm" data-act="inject-modal" data-arg="${esc(d.path)}">Fault ▾</button></td></tr>`).join('')}</tbody></table>` : '<span class="faint">no CDU in this estate</span>'}</div>
        <div class="card"><h3>cables</h3><div class="toolbar"><input type="text" class="mono" style="flex:1;min-width:200px" data-act="l-cable-ref" value="${esc(L.ui.cableRef)}" placeholder="port ref (device:port) or link id"><button class="btn sm danger" data-act="l-cable" data-arg="|pull">Pull</button><button class="btn sm" data-act="l-cable" data-arg="|replace">Replace</button></div>
          ${links.size ? `<table><tbody>${[...links.values()].map((e) => `<tr><td class="mono small" title="${esc(e.path)}">${esc(e.path.split('~').map((p) => U.shortPath(p)).join(' ↔ '))}</td><td>${e.state.to ? 'in' : U.sevHTML('Warning', 'pulled')}</td><td><button class="btn sm" data-act="l-cable" data-arg="${esc(e.path)}|${e.state.to ? 'pull' : 'replace'}">${e.state.to ? 'pull' : 'replace'}</button></td></tr>`).join('')}</tbody></table>` : ''}
          <p class="note">POST /v1/cable. Cable acts already in this console's event buffer are listed so a pulled cable can be put back.</p></div>
      </div><div>
        <div class="card"><h3>domains (${(L.domains || []).length})</h3><table><thead><tr><th>domain</th><th>kind</th><th class="num">members</th><th>state</th></tr></thead><tbody>${(L.domains || []).map((d) => `<tr><td class="path"><a href="#" data-act="goto" data-arg="${esc(d.path)}">${esc(U.shortPath(d.path))}</a></td><td>${esc(d.kind)}${d.technology ? ' · ' + esc(d.technology) : ''}</td><td class="num">${esc(d.members)}</td><td>${U.sevHTML(domSev(d.state), d.state)}</td></tr>`).join('')}</tbody></table></div>
      </div></div>`;
  }
  function faultsView(st) {
    const all = (L.catalogue.faults || []).slice().sort((a, b) => a.id.localeCompare(b.id));
    const f = all.find((x) => x.id === L.ui.fxFault) || null;
    const active = L.faults || [];
    return `<div class="toolbar"><h2>Faults</h2><span class="muted">faults go into the twin's model; every interface that would reveal an effect shows it</span></div>
      <div class="grid c2"><div>
        <div class="card"><h3>inject by selector</h3><div class="dialog" style="padding:0;border:0;width:auto;max-height:none">
          <div class="row"><label>select</label><input type="text" class="mono" data-act="l-fx-sel" value="${esc(L.ui.fxSel)}" placeholder="e.g. a01/compute-tray-07/gpu-2 or a01/switch-tray-0*"></div>
          <div class="row"><label>fault</label><select data-act="l-fx-fault" style="width:100%"><option value="">pick a fault…</option>${all.map((x) => `<option value="${esc(x.id)}" ${x.id === L.ui.fxFault ? 'selected' : ''}>${esc(x.title)} — ${esc(x.id)} (${esc(targetText(x.target))})</option>`).join('')}</select></div>
          ${f ? `${paramInputs(f, L.ui.fxParams, 'l-fx-param')}<div class="row"><label>clears</label><div class="small">${esc(clearsText(f.clears))}</div></div>` : ''}
          <div class="row"><label>id</label><input type="text" class="mono" data-act="l-fx-id" value="${esc(L.ui.fxId)}" placeholder="optional; the twin numbers it otherwise"></div>
          <div class="actions"><button class="btn primary" data-act="l-fx-inject" ${f ? '' : 'disabled'}>Inject</button></div></div>
          <p class="note">POST /v1/faults. The twin resolves the selector: every matching ${f ? esc(targetText(f.target)) : 'target'} gets an instance.</p></div>
        <div class="card"><h3>chaos</h3><div class="toolbar">rate <input type="number" min="0" step="0.5" style="width:70px" data-act="l-chaos-rate" value="${esc(L.ui.chaosRate)}"> per device per hour</div>
          <select multiple size="6" style="width:100%" data-act="l-chaos-faults">${all.map((x) => `<option value="${esc(x.id)}" ${L.ui.chaosFaults.includes(x.id) ? 'selected' : ''}>${esc(x.id)}</option>`).join('')}</select>
          <div class="toolbar" style="margin-top:6px"><button class="btn sm primary" data-act="l-chaos-set" ${L.ui.chaosFaults.length ? '' : 'disabled'}>Start</button><button class="btn sm" data-act="l-chaos-stop">Stop</button><span class="small faint">${L.ui.chaosSet ? `last set here: ${esc(L.ui.chaosSet)}` : 'the API does not report chaos state'}</span></div></div>
      </div><div>
        <div class="card"><h3>active (${active.length})</h3>${active.length ? `<table><thead><tr><th>since</th><th>fault</th><th>target</th><th></th></tr></thead><tbody>${active.slice().reverse().map((i) => `<tr><td class="mono small">${fmtT(new Date(i.Since))}</td><td>${esc(i.Title)} <span class="faint mono small">${esc(i.Fault)} · ${esc(i.ID)}${i.Parent ? ' · from ' + esc(i.Parent) : ''}</span>${i.NotApplied && i.NotApplied.length ? `<div class="small faint">not applied: ${esc(i.NotApplied.join('; '))}</div>` : ''}</td><td class="path"><a href="#" data-act="goto" data-arg="${esc(i.Target)}">${esc(U.shortPath(i.Target))}</a></td><td><button class="btn sm" data-act="l-clear" data-arg="${esc(i.ID)}">clear</button></td></tr>`).join('')}</tbody></table>` : '<span class="faint">none — no fault is active</span>'}</div>
      </div></div>`;
  }

  // ------------------------------------------------------------- report, scenarios
  function loadReport() {
    L.report.loading = true;
    api('GET', '/report?format=text').then((t) => { L.report.text = t; L.report.err = null; L.report.at = new Date(); }, (e) => { L.report.err = e.message; }).finally(() => { L.report.loading = false; changed(); });
  }
  function reportView(st) {
    if (L.report.text === null && !L.report.loading && !L.report.err) setTimeout(loadReport, 0);
    return `<div class="toolbar"><h2>Fidelity report</h2><span class="muted">GET /v1/report — what this twin is and is not, for the whole estate</span><span class="grow"></span>${L.report.at ? `<span class="small faint">read ${fmtT(L.report.at)}</span>` : ''}<button class="btn sm" data-act="l-report">refresh</button></div>
      <div class="card">${L.report.err ? `<span class="bad">${esc(L.report.err)}</span>` : L.report.text !== null ? `<pre style="white-space:pre-wrap">${esc(L.report.text)}</pre>` : '<span class="faint">reading…</span>'}</div>`;
  }
  const EXAMPLE = `# An example to edit: Xid 79 on one GPU of an NVL72 tray, then an AC cycle clears it.
# Paste any scenario file instead (examples/scenarios/twin/ in the repository).
apiVersion: dc-twin/v1alpha1
kind: Scenario
metadata: { name: console-xid-79 }
spec:
  estate: { requires: { formats: [nvidia/gb300-nvl72] } }
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
        within: 20s
        all:
          - faults: { select: "a01/compute-tray-07/**", count: 0 }
  report: fidelity
`;
  function reportBlock(rep) {
    if (!rep) return '';
    const list = (a) => (a && a.length ? a.join('; ') : 'none');
    const kv = (o) => Object.entries(o || {}).map(([k, v]) => `${v} ${k}`).join(' · ') || 'none';
    return `<pre style="white-space:pre-wrap;margin-top:8px">fidelity report — ${esc(rep.estate)}${rep.scenario ? ' · scenario ' + esc(rep.scenario) : ''}
  devices used: ${esc(kv(rep.devices))} · ${esc(rep.racks)} racks · fabric devices: ${esc(kv(rep.fabricDevices))}
  clock: ${esc(rep.clock)} (time scale ${esc(rep.timeScale)})
  impersonated: ${esc(list(rep.impersonated))}
  stubs: ${esc(list(rep.stubs))}
  unsynchronised adapters: ${esc(list(rep.unsynchronised))}
  substrate-owned state in use: ${esc(list(rep.substrateOwnedState))}
  twin incidents: ${esc(list(rep.twinIncidents))}
  fault effects not applied: ${esc(list(rep.faultEffectsNotApplied))}
  unverified profiles: ${esc(list(rep.unverifiedProfiles))}
  not modelled, and used by this run:
${(rep.notModelled || []).map((n) => `    ${esc(n.subject)}: ${esc((n.isNot || []).join('; '))}`).join('\n') || '    none'}${(rep.notes || []).length ? '\n  notes:\n' + rep.notes.map((n) => '    ' + esc(n)).join('\n') : ''}</pre>`;
  }
  function scenariosView(st) {
    if (L.ui.scnText === null) L.ui.scnText = EXAMPLE;
    const R = L.scn.result;
    const vSev = R ? ({ passed: 'OK', failed: 'Critical', impersonated: 'Warning', 'not-run': 'Warning' }[R.verdict] || 'Unknown') : null;
    return `<div class="toolbar"><h2>Scenarios</h2><span class="muted">runs on this twin through POST /v1/scenarios/run; a result is never shown without its fidelity report (D11)</span></div>
      <div class="grid c2">
        <div class="card"><h3>scenario file</h3>
          <textarea rows="24" data-act="l-scn-text" spellcheck="false">${esc(L.ui.scnText)}</textarea>
          <div class="toolbar" style="margin-top:8px"><button class="btn primary" data-act="l-scn-run" ${L.scn.running ? 'disabled' : ''}>${L.scn.running ? 'Running…' : 'Run'}</button><button class="btn sm" data-act="l-scn-example">example</button><label class="btn sm">open file…<input type="file" accept=".yaml,.yml,.json" data-act="l-scn-file" hidden></label></div>
          <p class="note">A run acts on this estate: its faults and acts are real events here, and other clients see them.</p></div>
        <div class="card"><h3>result</h3>${L.scn.err ? `<span class="bad">${esc(L.scn.err)}</span>` : L.scn.running ? '<span class="faint">running — the API answers when the scenario ends</span>' : R ? `
          <div class="toolbar"><b>${esc(R.scenario)}</b> <span class="pill ${pillSev(vSev)}">${U.SEV_MARK[vSev] || ''} ${esc(R.verdict)}</span> <span class="small muted">${esc(R.reason || '')}</span><span class="grow"></span><span class="small faint">took ${esc(R.took)}</span></div>
          <ol class="steps">${(R.steps || []).map((s) => `<li><span class="${s.status === 'passed' ? 'ok' : 'bad'}">${s.status === 'passed' ? '✓' : '✗'}</span> <span class="mono small">${esc(s.kind)}</span>${s.name ? ' ' + esc(s.name) : ''} <span class="faint small">${esc(s.took)}</span>${(s.detail || []).length ? `<ul class="small" style="margin:2px 0 0 14px;padding:0">${s.detail.map((x) => `<li class="${s.status === 'passed' ? 'muted' : 'bad'}">${esc(x)}</li>`).join('')}</ul>` : ''}</li>`).join('')}</ol>
          ${reportBlock(R.report)}
          <details><summary class="small">raw result</summary><pre>${esc(JSON.stringify(R, null, 2))}</pre></details>` : '<span class="faint">run a scenario to see its verdict, steps and fidelity report</span>'}</div>
      </div>`;
  }

  // ------------------------------------------------------------- the inject modal
  function openInject(st, path, key, fault) {
    const M = { type: 'l-inject', rack: null, dev: null, key: null, port: '', fault: fault || null, params: {}, id: '' };
    if (L.racks[path]) M.rack = path;
    else if (L.devices[path]) { M.dev = path; M.key = key || 'd:' + path; }
    else { const p = devicesOf(path)[0]; if (p) { M.dev = p; M.key = key || 'c:' + path; } }
    st.modal = M;
  }
  function modal(st) {
    const M = st.modal; if (!M || M.type !== 'l-inject') return '';
    const rack = M.rack && L.racks[M.rack];
    const devOpts = rack ? rack.devices.filter((d) => targetsOf(d.path).length) : [];
    const targets = M.dev ? targetsOf(M.dev) : [];
    const T = targets.find((t) => t.key === M.key) || targets[0];
    const list = T ? T.faults : [];
    const f = list.find((x) => x.id === M.fault) || list[0];
    return `<div class="dialog"><h2>Inject fault <span class="faint small">POST /v1/faults</span></h2>
      ${rack ? `<div class="row"><label>device in ${esc(rack.name)}</label><select data-act="l-m-dev" style="width:100%"><option value="">pick a device…</option>${devOpts.map((d) => `<option value="${esc(d.path)}" ${M.dev === d.path ? 'selected' : ''}>${esc(d.name)} (${esc(d.class)})</option>`).join('')}</select></div>` : ''}
      ${M.dev ? `<div class="row"><label>target</label><div><div class="mono small">${esc(U.shortPath(M.dev))}</div><select data-act="l-m-tgt" style="width:100%;margin-top:4px">${targets.map((t) => `<option value="${esc(t.key)}" ${T && T.key === t.key ? 'selected' : ''}>${esc(t.label)}</option>`).join('')}</select>
        ${T && T.port ? `<div style="margin-top:4px"><span class="mono small">${esc(U.lastSeg(M.dev))}:</span><input type="text" class="mono" data-act="l-m-port" value="${esc(M.port)}" placeholder="port, e.g. bmc0" style="width:160px"></div>` : ''}</div></div>
      ${T ? `<div class="row"><label>fault</label><select data-act="l-m-fault" style="width:100%">${list.map((x) => `<option value="${esc(x.id)}" ${f && f.id === x.id ? 'selected' : ''}>${esc(x.title)} — ${esc(x.id)}</option>`).join('')}</select></div>
        ${f ? `${paramInputs(f, M.params, 'l-m-param')}<div class="row"><label>clears</label><div class="small">${esc(clearsText(f.clears))}</div></div>` : ''}
        <div class="row"><label>id</label><input type="text" class="mono" data-act="l-m-id" value="${esc(M.id)}" placeholder="optional"></div>` : '<p class="faint">no catalogue fault fits this device</p>'}` : '<p class="faint">pick a device</p>'}
      <div class="actions"><button class="btn" data-act="modal-close">Cancel</button><button class="btn primary" data-act="l-m-inject" ${f ? '' : 'disabled'}>Inject</button></div></div>`;
  }
  function injectFromModal(st) {
    const M = st.modal; const targets = targetsOf(M.dev); const T = targets.find((t) => t.key === M.key) || targets[0]; if (!T) return;
    const f = T.faults.find((x) => x.id === M.fault) || T.faults[0]; if (!f) return;
    if (T.port && !M.port.trim()) { toast('name the port to inject on', 'crit'); return; }
    const select = T.port ? `${M.dev}:${M.port.trim()}` : T.path;
    st.modal = null;
    call('POST', '/faults', { select, fault: f.id, params: paramValues(f, M.params), id: M.id.trim() }, (r) => `injected ${f.id} on ${(r || []).map((x) => U.shortPath(x.Target)).join(', ') || U.shortPath(select)}`);
  }

  // ------------------------------------------------------------- acts (from app.js's delegated handlers)
  function goto(p) {
    const go = L.hooks.go;
    if (L.devices[p]) return go(`#/device/${encodeURIComponent(p)}`);
    if (L.racks[p]) return go(`#/rack/${encodeURIComponent(p)}`);
    const d = devicesOf(p)[0]; if (d) return go(`#/device/${encodeURIComponent(d)}`);
    const rp = Object.keys(L.racks).find((r) => p.startsWith(r + '/')); if (rp) return go(`#/rack/${encodeURIComponent(rp)}`);
  }
  function act(st, a, arg, el, e) {
    switch (a) {
      case 'goto': e.preventDefault(); goto(arg); return true;
      case 'report': L.hooks.go('#/report'); return true;
      case 'inject-modal': openInject(st, arg, el.dataset.key, el.dataset.fault); render(); return true;
      case 'l-m-inject': injectFromModal(st); render(); return true;
      case 'l-power': { document.querySelectorAll('.menu.open').forEach((m) => m.classList.remove('open')); const [p, action] = arg.split('|'); call('POST', '/power', { select: p, action }, (r) => `power ${action} → ${r.canonical} on ${U.shortPath(p)}`); render(); return true; }
      case 'l-inject': { const [f, t] = arg.split('|'); call('POST', '/faults', { select: t, fault: f }, `injected ${f} on ${U.shortPath(t)}`); return true; }
      case 'l-clear': call('POST', '/faults/clear', { target: arg }, (r) => `cleared ${r.cleared} fault(s)`); return true;
      case 'l-feed': { const [feed, to] = arg.split('|'); call('POST', '/feeds', { feed, up: to === 'up' }, `feed ${feed} ${to}`); return true; }
      case 'l-cable': { const [ref, state] = arg.split('|'); const link = ref || L.ui.cableRef.trim(); if (!link) return true; call('POST', '/cable', { link, state }, `cable ${state === 'pull' ? 'pulled' : 'replaced'}: ${link}`); return true; }
      case 'l-cable-port': { const [dev, state] = arg.split('|'); const port = L.ui.port.trim(); if (!port) { toast('name a port first', 'crit'); return true; } call('POST', '/cable', { link: `${dev}:${port}`, state }, `cable ${state === 'pull' ? 'pulled' : 'replaced'} at ${U.lastSeg(dev)}:${port}`); return true; }
      case 'l-util-set': { const v = parseFloat(L.ui.util); if (isNaN(v)) return true; call('POST', '/utilisation', { select: arg, value: v }, `utilisation ${v} on ${U.shortPath(arg)}`); return true; }
      case 'rf': e.preventDefault(); if (L.st.route.name === 'device') loadRF(L.st.route.arg, arg, true); return true;
      case 'l-rf-back': { const p = L.rf.hist.pop(); if (p) loadRF(L.rf.device, p, false); return true; }
      case 'l-rf-go': { const inp = document.querySelector('[data-act="l-rf-path"]'); if (inp && L.st.route.name === 'device') loadRF(L.st.route.arg, inp.value.trim() || '/redfish/v1', true); return true; }
      case 'l-reqs': if (L.reqs.device) loadReqs(L.reqs.device).then(changed); return true;
      case 'l-report': loadReport(); render(); return true;
      case 'l-ev-kind': if (L.ui.hideKinds.has(arg)) L.ui.hideKinds.delete(arg); else L.ui.hideKinds.add(arg); render(); return true;
      case 'l-fx-inject': { const f = faultSpec(L.ui.fxFault); if (!f) return true; if (!L.ui.fxSel.trim()) { toast('enter a selector', 'crit'); return true; } call('POST', '/faults', { select: L.ui.fxSel.trim(), fault: f.id, params: paramValues(f, L.ui.fxParams), id: L.ui.fxId.trim() }, (r) => `injected ${f.id} on ${(r || []).length} target(s)`); return true; }
      case 'l-chaos-set': { const rate = parseFloat(L.ui.chaosRate) || 0; call('POST', '/chaos', { rate, faults: L.ui.chaosFaults }, `chaos at ${rate}/device/h over ${L.ui.chaosFaults.length} fault(s)`).then((r) => { if (r) { L.ui.chaosSet = `${rate}/device/h · ${L.ui.chaosFaults.join(', ')}`; render(); } }); return true; }
      case 'l-chaos-stop': call('POST', '/chaos', { rate: 0, faults: [] }, 'chaos stopped').then((r) => { if (r) { L.ui.chaosSet = 'stopped'; render(); } }); return true;
      case 'l-scn-example': L.ui.scnText = EXAMPLE; render(); return true;
      case 'l-scn-run': {
        L.scn = { running: true, result: null, err: null }; render();
        api('POST', '/scenarios/run', L.ui.scnText, 'yaml').then((r) => { L.scn = { running: false, result: r, err: null }; }, (err) => { L.scn = { running: false, result: null, err: err.message }; }).finally(changed);
        return true;
      }
    }
    return false;
  }
  function change(st, a, el) {
    const M = st.modal;
    if (input(st, a, el)) return true; // text and number fields: keep the value, no re-render under the next click
    switch (a) {
      case 'l-ev-reads': L.ui.reads = el.checked; openStream(); if (L.ui.reads) backfill(0); break;
      case 'l-m-dev': M.dev = el.value || null; M.key = null; M.fault = null; M.params = {}; break;
      case 'l-m-tgt': M.key = el.value; M.fault = null; M.params = {}; break;
      case 'l-m-fault': M.fault = el.value; M.params = {}; break;
      case 'l-fx-fault': L.ui.fxFault = el.value; L.ui.fxParams = {}; break;
      case 'l-chaos-faults': L.ui.chaosFaults = [...el.selectedOptions].map((o) => o.value); break;
      case 'l-scn-file': { const file = el.files && el.files[0]; if (!file) return true; file.text().then((t) => { L.ui.scnText = t; render(); }); return true; }
      default: return false;
    }
    render(); return true;
  }
  // Typing: keep the value, do not re-render under the cursor.
  function input(st, a, el) {
    const M = st.modal;
    switch (a) {
      case 'l-scn-text': L.ui.scnText = el.value; return true;
      case 'l-port': L.ui.port = el.value; return true;
      case 'l-cable-ref': L.ui.cableRef = el.value; return true;
      case 'l-util': L.ui.util = el.value; return true;
      case 'l-fx-sel': L.ui.fxSel = el.value; return true;
      case 'l-fx-id': L.ui.fxId = el.value; return true;
      case 'l-fx-param': L.ui.fxParams[el.dataset.arg] = el.value; return true;
      case 'l-chaos-rate': L.ui.chaosRate = el.value; return true;
      case 'l-m-port': M.port = el.value; return true;
      case 'l-m-id': M.id = el.value; return true;
      case 'l-m-param': M.params[el.dataset.arg] = el.value; return true;
      case 'l-rf-path': return true;
    }
    return false;
  }
  function key(st, e) {
    const a = e.target.dataset && e.target.dataset.act;
    if (e.key === 'Enter' && a === 'l-rf-path' && st.route.name === 'device') { loadRF(st.route.arg, e.target.value.trim() || '/redfish/v1', true); return true; }
    return false;
  }

  return { requested: () => !!token, connect, frame, modal, act, change, input, key, index: () => ({ devices: L.devices, racks: L.racks }), store: L };
})();
