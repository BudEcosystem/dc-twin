// app.js — state, hash router, rendering and the single click/change handler that maps data-act
// attributes to engine calls. In the real console every action here is a control API call (D16).
(() => {
  const U = DCT.util, esc = U.esc, P = DCT.packs, V = DCT.views;
  const E = DCT.engine;

  const st = {
    route: { name: 'floor' }, selection: '', mode: 'operate', overlay: 'health', rackSide: 'front', tab: 'overview', rfPath: null, topoRack: null, stackFilter: '',
    ev: { sel: '', kinds: new Set(V.KINDS.filter((k) => k !== 'kernel')), minor: false, follow: true },
    fx: { sel: '', target: null, faultId: null, when: 'now', inSec: 30, everySec: 120, clearAfter: 0 },
    draft: { adds: [], removes: [], fidelity: [] }, designPick: null, modal: null, applying: false,
  };

  // ------------------------------------------------------------- boot
  const model = DCT.estate.compile(DCT.estate.file);
  E.init(model); DCT.stack.init(E);
  const theme = U.store('dct-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  document.documentElement.dataset.theme = theme;

  // ------------------------------------------------------------- routing
  function parseHash() {
    const h = location.hash.replace(/^#\/?/, ''); const [name, ...rest] = h.split('/');
    const arg = rest.length ? decodeURIComponent(rest.join('/')) : null;
    return { name: name || 'floor', arg };
  }
  window.addEventListener('hashchange', () => { st.route = parseHash(); if (st.route.name === 'device' && st.route.arg) st.selection = st.route.arg; if (st.route.name === 'rack' && st.route.arg && !(st.selection && st.selection.startsWith(st.route.arg))) st.selection = ''; closeMenus(); render(true); });
  st.route = parseHash(); if (st.route.name === 'device') st.selection = st.route.arg;
  const go = (hash) => { location.hash = hash; };

  // ------------------------------------------------------------- render
  let dirty = false, lastRender = 0, raf = null;
  function markDirty() { dirty = true; if (!raf) raf = requestAnimationFrame(() => { raf = null; const now = performance.now(); if (now - lastRender > 350) render(); else setTimeout(() => { if (dirty) render(); }, 350 - (now - lastRender)); }); }
  function typing() { const a = document.activeElement; return a && a.closest && a.closest('#app, #modal') && ((a.tagName === 'INPUT' && a.type !== 'checkbox') || a.tagName === 'TEXTAREA' || a.tagName === 'SELECT'); }
  function render(force) {
    if (!force && typing()) { dirty = true; return; }
    dirty = false; lastRender = performance.now();
    const r = st.route; const m = E.model;
    const crumbs = [`<a href="#/floor">${esc(m.hall.site)}</a>`, `<a href="#/floor">${esc(m.hall.name)}</a>`];
    if (r.name === 'rack' && r.arg) crumbs.push(`<b>${esc(U.lastSeg(r.arg))}</b>`);
    if (r.name === 'device' && r.arg) { const d = m.devices[r.arg]; if (d && d.rack) crumbs.push(`<a href="#/rack/${encodeURIComponent(d.rack)}">${esc(U.lastSeg(d.rack))}</a>`); crumbs.push(`<b>${esc(U.lastSeg(r.arg))}</b>`); }
    const active = E.activeFaults().length;
    const rail = [['floor', 'Floor', ''], ['topology', 'Topology', ''], ['facility', 'Facility', ''], ['faults', 'Faults', active ? String(active) : ''], ['events', 'Events', String(E.events.length)], ['scenarios', 'Scenarios', ''], ['stack', 'Stack', 'sim']];
    let view = '', drawer = '';
    const withDrawer = ['floor', 'rack', 'device'].includes(r.name);
    switch (r.name) {
      case 'rack': view = V.rack(st, r.arg); drawer = V.drawer(st, U.lastSeg(r.arg) + '/**'); break;
      case 'device': view = V.device(st, r.arg); drawer = V.drawer(st, U.shortPath(r.arg)); break;
      case 'topology': view = V.topology(st); break;
      case 'facility': view = V.facility(st); break;
      case 'faults': view = V.faults(st); break;
      case 'events': view = V.events(st); break;
      case 'scenarios': view = V.scenarios(st); break;
      case 'stack': view = V.stack(st); break;
      default: { const floor = V.floor(st); view = st.mode === 'design' ? `<div class="grid side">${`<div>${floor}</div>`}<div>${V.design(st)}</div></div>` : floor; drawer = V.drawer(st, st.selection && m.racks[st.selection] ? U.lastSeg(st.selection) + '/**' : ''); }
    }
    const scrollers = {}; for (const id of ['drawer-body', 'events-body']) { const el = document.getElementById(id); if (el) scrollers[id] = { top: el.scrollTop, atEnd: el.scrollTop + el.clientHeight >= el.scrollHeight - 8 }; }
    const viewEl = document.querySelector('.view'); const viewScroll = viewEl ? viewEl.scrollTop : 0;
    document.getElementById('app').innerHTML = `
      <div class="topbar"><span class="brand">dc-twin<small>console · prototype</small></span><span class="crumbs">${crumbs.join(' / ')}</span><span class="spacer"></span>
        <span class="sim" title="the software under test is a simulation (see Stack)">simulated stack</span>
        <span class="clock" title="clock: ${esc(m.estate.spec.clock)}">${esc(m.estate.spec.clock)} ${U.fmtClock(E.now)}</span>
        <div class="seg"><button class="${st.mode === 'operate' ? 'on' : ''}" data-act="mode" data-arg="operate">Operate</button><button class="${st.mode === 'design' ? 'on' : ''}" data-act="mode" data-arg="design">Design${st.draft.adds.length + st.draft.removes.length + st.draft.fidelity.length ? ' •' : ''}</button></div>
        <button class="btn sm" data-act="report" title="fidelity report for what this view shows">Report</button>
        <button class="btn sm" data-act="theme" title="light / dark">◐</button></div>
      <div class="body"><nav class="rail">${rail.map(([k, l, c]) => `<a href="#/${k}" class="${r.name === k ? 'active' : ''}">${l}<span class="count">${esc(c)}</span></a>`).join('')}
        <div class="sect">estate</div><div class="small muted" style="padding:0 10px">${esc(m.estate.metadata.name)}<br>${Object.keys(m.racks).length} racks · ${m.order.length} devices<br>${m.links.length} links · ${m.estate.spec.packs.length} packs<br>${m.order.filter((p) => m.devices[p].fidelity === 'vm').length} vm · ${m.order.filter((p) => m.devices[p].fidelity === 'model').length} model</div>
        <div class="sect">about</div><div class="small faint" style="padding:0 10px">Prototype over an in-browser estate; no backend. Durations are shortened. <a href="../docs/spec/12-ui.md">spec 12 — UI</a></div></nav>
        <div class="main ${withDrawer ? 'with-drawer' : ''}"><div class="view">${view}</div>${withDrawer ? `<div class="drawer">${drawer}</div>` : ''}</div></div>`;
    const modal = document.getElementById('modal'); modal.hidden = !st.modal; if (force || !st.modal || st.modal.type === 'apply') modal.innerHTML = st.modal ? V.modal(st) : '';
    for (const [id, s] of Object.entries(scrollers)) { const el = document.getElementById(id); if (!el) continue; if (st.ev.follow || s.atEnd) el.scrollTop = el.scrollHeight; else el.scrollTop = s.top; }
    const nv = document.querySelector('.view'); if (nv && !force) nv.scrollTop = viewScroll;
    const fresh = document.getElementById('drawer-body') || document.getElementById('events-body'); if (fresh && st.ev.follow) fresh.scrollTop = fresh.scrollHeight;
  }

  E.on((ev) => { if (ev.kind === 'tick') { DCT.scenarios.tick(); } markDirty(); });
  E.start();

  // ------------------------------------------------------------- actions
  const closeMenus = () => document.querySelectorAll('.menu.open').forEach((m) => m.classList.remove('open'));
  function openInject(target) { st.modal = { type: 'inject', target, sub: null, faultId: null, when: 'now', inSec: 30, clearAfter: 0 }; render(true); }
  function doInject(faultId, target, when, inSec, clearAfter, source = 'user:priya') {
    const opts = { source, clearAfter: clearAfter > 0 ? clearAfter : undefined };
    if (when === 'in') { E.schedule({ at: new Date(E.now.getTime() + inSec * 1000), fault: faultId, target, source, clearAfter: opts.clearAfter }); return { ok: true }; }
    if (when === 'every') { E.schedule({ at: new Date(E.now.getTime() + 1000), fault: faultId, target, source, every: inSec, clearAfter: opts.clearAfter }); return { ok: true }; }
    return E.inject(faultId, target, opts);
  }
  function toast(msg) { E.emit({ kind: 'twin', path: E.model.hall.path, msg, source: 'console' }); }
  function addRackModal(row, position, format, existing) {
    const fmt = P.formats[format || st.designPick || 'nvidia/gb300-nvl72'];
    const id = format || st.designPick || 'nvidia/gb300-nvl72';
    st.modal = existing ? { type: 'add-rack', editing: true, ...existing, params: { ...existing.params } } : { type: 'add-rack', row, position, name: `${row}${String(position).padStart(2, '0')}`, format: id, params: DCT.estate.paramsFor(fmt, {}), vmHosts: 0 };
    render(true);
  }

  document.addEventListener('click', (e) => {
    const el = e.target.closest('[data-act]'); const inMenu = e.target.closest('.menu');
    if (!inMenu) closeMenus();
    if (!el) return;
    const act = el.dataset.act, arg = el.dataset.arg;
    if (el.tagName === 'INPUT' || el.tagName === 'SELECT') return; // handled on change
    if (el.tagName === 'A' && el.getAttribute('href') === '#') e.preventDefault();
    switch (act) {
      case 'menu': el.parentElement.classList.toggle('open'); return;
      case 'select': st.selection = arg || ''; render(true); return;
      case 'goto': { e.preventDefault(); const p = arg; if (E.model.devices[p]) go(`#/device/${encodeURIComponent(p)}`); else if (E.model.racks[p]) go(`#/rack/${encodeURIComponent(p)}`); else if (E.model.devices[U.parentPath(p)]) go(`#/device/${encodeURIComponent(U.parentPath(p))}`); else if (/\/domain\//.test(p)) go(`#/rack/${encodeURIComponent(p.split('/domain/')[0])}`); return; }
      case 'open-rack': go(`#/rack/${encodeURIComponent(arg)}`); return;
      case 'overlay': st.overlay = arg; render(true); return;
      case 'mode': st.mode = arg; if (arg === 'operate') st.designPick = null; render(true); return;
      case 'theme': { const t = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark'; document.documentElement.dataset.theme = t; U.store('dct-theme', t); return; }
      case 'rack-side': st.rackSide = arg; render(true); return;
      case 'tab': st.tab = arg; render(true); return;
      case 'rf': st.rfPath = arg; render(true); return;
      case 'power': { const [path, action] = arg.split('|'); const r = E.power(path, action, { source: 'user:priya' }); if (!r.ok) toast(`power ${action} on ${U.shortPath(path)} refused: ${r.why}`); closeMenus(); render(true); return; }
      case 'inject-modal': openInject(arg); return;
      case 'inject-direct': { const [f, t] = arg.split('|'); const r = E.inject(f, t, { source: 'user:priya' }); if (!r.ok) toast(`inject refused: ${r.why}`); render(true); return; }
      case 'clear-fault': E.clearFault(parseInt(arg, 10), { source: 'user:priya' }); render(true); return;
      case 'cable': { const [id, state] = arg.split('|'); const link = E.model.linkById[id]; if (!link) return; if (state === 'down') { const r = E.inject('generic/link/cable-pulled', id, { source: 'user:priya' }); if (!r.ok) toast(r.why); } else { const i = E.activeFaults().find((f) => f.link === id); if (i) E.clearFault(i.id, { source: 'user:priya' }); else E.setLink(link, 'up'); } E.session.push({ t: new Date(E.now), type: 'cable', link: id, state }); render(true); return; }
      case 'ev-kind': { if (st.ev.kinds.has(arg)) st.ev.kinds.delete(arg); else st.ev.kinds.add(arg); render(true); return; }
      case 'ev-apply': { const inp = document.querySelector('[data-act="ev-sel"]'); if (inp) st.ev.sel = inp.value; render(true); return; }
      case 'save-scenario': st.modal = { type: 'text', title: 'Session as a scenario', note: 'The faults and technician acts of this session, in order, as a scenario file. Add expect: blocks for what the stack must have done.', text: DCT.scenarios.fromSession(), filename: 'session-scenario.yaml' }; render(true); return;
      case 'scenario-run': DCT.scenarios.run(arg); render(true); return;
      case 'stack-filter': st.stackFilter = arg; render(true); return;
      case 'report': { const paths = st.route.name === 'rack' ? E.model.racks[st.route.arg]?.devices || [] : st.route.name === 'device' ? [st.route.arg] : E.model.order; st.modal = { type: 'report', title: st.route.name === 'rack' ? U.lastSeg(st.route.arg) : st.route.name === 'device' ? U.shortPath(st.route.arg) : 'whole estate', report: E.fidelityReport(paths) }; render(true); return; }
      case 'fx-when': st.fx.when = arg; render(true); return;
      case 'fx-inject': { const r = doInject(st.fx.faultId || (V.resolveTarget(st.fx.target) && P.applicable(V.resolveTarget(st.fx.target).device, V.resolveTarget(st.fx.target).comp)[0]?.id), st.fx.target, st.fx.when, st.fx.when === 'every' ? st.fx.everySec : st.fx.inSec, st.fx.clearAfter); if (!r.ok) toast(`inject refused: ${r.why}`); render(true); return; }
      // modals
      case 'modal-close': st.modal = null; render(true); return;
      case 'm-when': st.modal.when = arg; render(true); return;
      case 'm-inject': { const M = st.modal; const target = M.sub && M.compPath ? M.compPath : (M.sub || M.target); const T = V.resolveTarget(M.compPath || M.sub || M.target); const list = T ? P.applicable(T.device, T.comp) : []; const f = list.find((x) => x.id === M.faultId) || list[0]; if (!f) return; const r = doInject(f.id, M.compPath || M.sub || M.target, M.when, M.inSec || 30, M.clearAfter || 0); if (!r.ok) toast(`inject refused: ${r.why}`); st.modal = null; render(true); return; }
      case 'copy-text': { navigator.clipboard?.writeText(st.modal.text); return; }
      case 'download-text': U.download(st.modal.filename || 'export.yaml', st.modal.text); return;
      // design
      case 'design-add': { const [row, pos] = arg.split(':'); addRackModal(row, parseInt(pos, 10)); return; }
      case 'design-pick': st.designPick = st.designPick === arg ? null : arg; render(true); return;
      case 'design-edit-add': { const a = st.draft.adds.find((x) => x.name === arg); if (a) addRackModal(a.row, a.position, a.format, a); return; }
      case 'design-remove': { const i = st.draft.removes.indexOf(arg); if (i >= 0) st.draft.removes.splice(i, 1); else st.draft.removes.push(arg); if (st.mode !== 'design') st.mode = 'design'; render(true); return; }
      case 'draft-drop': { const [kind, idx] = arg.split(':'); const list = kind === 'add' ? st.draft.adds : kind === 'remove' ? st.draft.removes : st.draft.fidelity; list.splice(parseInt(idx, 10), 1); render(true); return; }
      case 'draft-discard': st.draft = { adds: [], removes: [], fidelity: [] }; render(true); return;
      case 'draft-export': { const file = JSON.parse(JSON.stringify(E.model.estate)); const rows = file.spec.sites[0].halls[0].rows; for (const a of st.draft.adds) rows.find((r) => r.name === a.row).racks.push({ name: a.name, position: a.position, format: a.format, params: a.params }); for (const rp of st.draft.removes) for (const r of rows) r.racks = r.racks.filter((x) => x.name !== U.lastSeg(rp)); for (const f of st.draft.fidelity) file.spec.fidelity.overrides.push(f); st.modal = { type: 'text', title: 'estate file (with draft)', text: DCT.estate.toYAML(file), filename: `${file.metadata.name}.yaml` }; render(true); return; }
      case 'draft-apply': {
        const draft = st.draft; st.draft = { adds: [], removes: [], fidelity: [] }; st.applying = true; st.modal = { type: 'apply', log: [], done: false }; render(true);
        for (const a of draft.adds) if (a.vmHosts) draft.fidelity.push({ select: `${a.name}/${P.formats[a.format].archetype === 'rack-scale' ? 'compute-tray' : 'server'}-{01..${String(a.vmHosts).padStart(2, '0')}}`, level: 'vm' });
        E.applyDraft(draft, (msg) => { if (st.modal && st.modal.type === 'apply') { st.modal.log.push(msg); render(true); } }).then(() => { st.applying = false; if (st.modal && st.modal.type === 'apply') st.modal.done = true; st.selection = ''; render(true); });
        return; }
      case 'm-drop-add': { st.draft.adds = st.draft.adds.filter((a) => a.name !== st.modal.name); st.modal = null; render(true); return; }
      case 'm-add': { const M = st.modal; const name = (M.name || '').trim(); if (!name) return; const fmt = P.formats[M.format]; const params = {}; for (const [k, v] of Object.entries(fmt.params)) { let val = M.params[k] !== undefined ? M.params[k] : v.default; if (v.range) val = parseInt(val, 10); params[k] = val; } const entry = { row: M.row, position: M.position, name, format: M.format, params, vmHosts: M.vmHosts || 0 }; const idx = st.draft.adds.findIndex((a) => a.row === M.row && a.position === M.position); if (idx >= 0) st.draft.adds[idx] = entry; else st.draft.adds.push(entry); st.modal = null; st.designPick = null; render(true); return; }
    }
  });
  document.addEventListener('dblclick', (e) => { const el = e.target.closest('[data-open]'); if (!el) return; const p = el.dataset.open; if (E.model.devices[p]) go(`#/device/${encodeURIComponent(p)}`); else if (E.model.racks[p]) go(`#/rack/${encodeURIComponent(p)}`); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') { closeMenus(); if (st.modal) { st.modal = null; render(true); } } if (e.key === 'Enter' && e.target.dataset && e.target.dataset.act === 'ev-sel') { st.ev.sel = e.target.value; render(true); } if (e.key === 'Enter' && e.target.dataset && e.target.dataset.act === 'fx-sel') { st.fx.sel = e.target.value; st.fx.target = null; render(true); } });
  document.addEventListener('input', (e) => { const el = e.target; const act = el.dataset && el.dataset.act; if (!act) return; if (act === 'fx-sel') { st.fx.sel = el.value; st.fx.target = null; } if (act === 'ev-sel') st.ev.sel = el.value; if (act === 'm-name') st.modal.name = el.value; if (act === 'm-param' && el.type === 'text') st.modal.params[el.dataset.arg] = el.value; });
  document.addEventListener('change', (e) => {
    const el = e.target; const act = el.dataset && el.dataset.act; if (!act) return;
    switch (act) {
      case 'ev-follow': st.ev.follow = el.checked; break;
      case 'ev-minor': st.ev.minor = el.checked; break;
      case 'fx-sel': st.fx.sel = el.value; st.fx.target = null; break;
      case 'fx-target': st.fx.target = el.value || null; st.fx.faultId = null; break;
      case 'fx-fault': st.fx.faultId = el.value; break;
      case 'fx-in': st.fx.inSec = parseInt(el.value, 10) || 30; st.fx.when = 'in'; break;
      case 'fx-every': st.fx.everySec = parseInt(el.value, 10) || 120; st.fx.when = 'every'; break;
      case 'fx-clear': st.fx.clearAfter = parseInt(el.value, 10) || 0; break;
      case 'chaos': E.chaos.enabled = el.checked; toast(`chaos ${el.checked ? 'enabled' : 'disabled'} at ${E.chaos.ratePerDeviceHour}/device/h`); break;
      case 'chaos-rate': E.chaos.ratePerDeviceHour = parseFloat(el.value) || 1; break;
      case 'topo-rack': st.topoRack = el.value; break;
      case 'design-fid': if (el.value) { st.draft.fidelity.push({ select: el.dataset.arg, level: el.value }); st.mode = 'design'; } break;
      case 'm-sub': st.modal.sub = el.value || null; st.modal.compPath = null; st.modal.faultId = null; break;
      case 'm-comp': st.modal.compPath = el.value; st.modal.faultId = null; break;
      case 'm-fault': st.modal.faultId = el.value; break;
      case 'm-in': st.modal.inSec = parseInt(el.value, 10) || 30; st.modal.when = 'in'; break;
      case 'm-clear': st.modal.clearAfter = parseInt(el.value, 10) || 0; break;
      case 'm-name': st.modal.name = el.value; break;
      case 'm-format': st.modal.format = el.value; st.modal.params = DCT.estate.paramsFor(P.formats[el.value], {}); break;
      case 'm-param': st.modal.params[el.dataset.arg] = el.type === 'number' ? parseInt(el.value, 10) : el.value; break;
      case 'm-vm': st.modal.vmHosts = parseInt(el.value, 10); break;
      default: return;
    }
    render(true);
  });

  render(true);
})();
