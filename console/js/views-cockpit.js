// views-cockpit.js — the top bar and the Operate screen: the hall in ISO (CSS 3D) or 2D with
// power, network and cooling flows, and the floating rack, device, break-it, events and
// facility panels. Values come from the source (demo or live); nothing here holds state.
window.TC = window.TC || {};
TC.views = TC.views || {};
(() => {
  const { h } = TC.h;
  const { SEVN, SEVC, SEVM, KIND_C, kw } = TC.data;
  const MONO = 'ui-monospace,Menlo,monospace';
  const GLASS = 'background:rgba(12,12,14,0.74);backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);border:1px solid rgba(255,255,255,0.07);border-radius:10px;box-shadow:0 12px 40px rgba(0,0,0,0.55)';
  const LABEL = 'font-size:11px;color:#757575;text-transform:uppercase;letter-spacing:.06em';
  const SUB = 'font-size:10.5px;color:#757575;text-transform:uppercase;letter-spacing:.06em';

  // A segmented control: [value, label] pairs.
  function seg(items, cur, set, style) {
    return items.map(([v, l]) => h('button', { key: v, onClick: () => set(v), style: `background:${cur === v ? '#965cde26' : 'transparent'};color:${cur === v ? '#edeef0' : '#757575'};border:0;${style}` }, l ?? v));
  }
  const segBox = (kids) => h('div', { style: 'display:flex;border:1px solid #2a2a2a;border-radius:6px;overflow:hidden' }, kids);
  const primary = (text, onClick, disabled) => h('button', { className: 'ds-btn ds-primary', onClick, disabled: !!disabled }, text);
  const secondary = (text, onClick, disabled) => h('button', { className: 'ds-btn ds-secondary', onClick, disabled: !!disabled }, text);

  function fxEl(src, path, round) {
    const t = src.pulses[path]; if (!t || Date.now() - t > 2600) return null;
    return h('span', { key: 'fx' + t, style: `position:absolute;inset:-3px;border:2px solid #ec7575;border-radius:${round ? 8 : 3}px;animation:dcPulse 1.1s ease-out 2;pointer-events:none` });
  }

  // ------------------------------------------------------------------ the model the views use
  function model(A) {
    const S = A.S, src = A.src;
    const aggs = {}; for (const r of Object.values(src.racks)) aggs[r.name] = src.rackAgg(r);
    const R = src.racks[S.selRack] || Object.values(src.racks)[0];
    const D = (R && src.devs[S.selDev]) || (R && src.devs[R.slots[0]]) || null;
    const tot = Object.values(aggs).reduce((a, b) => a + b.w, 0);
    return { aggs, R, D, tot };
  }

  // ------------------------------------------------------------------ top bar
  function topBar(A, M) {
    const S = A.S, src = A.src;
    const nAct = src.active.length;
    const rail = [['cockpit', 'Operate', ''], ['topology', 'Topology', src.pulled.size ? String(src.pulled.size) : ''], ['facility', 'Facility', ''], ['scenarios', 'Scenarios', src.kind === 'demo' ? '3' : ''], ['design', 'Design', S.draft.length ? String(S.draft.length) : '']];
    const lag = src.lag(); const inc = src.incidents(); const st = src.stackLabel();
    const devs = Object.keys(src.devs).length, vm = Object.values(src.devs).filter((d) => d.fid === 'vm').length;
    return h('div', { style: 'display:flex;align-items:center;gap:14px;padding:0 12px 0 14px;border-bottom:1px solid #1f1f1f;background:#101010;min-width:0' },
      h('div', { style: 'display:flex;align-items:center;gap:10px;flex:none' },
        h('img', { className: 'tc-logo-full', src: 'img/bud-gpu-foundry-white.png', alt: 'Bud GPU Foundry', title: 'Bud GPU Foundry', style: 'height:16px;width:auto;display:block' }),
        h('img', { className: 'tc-logo-mark', src: 'img/bud-mark.png', alt: 'Bud GPU Foundry', title: 'Bud GPU Foundry', style: 'height:18px;width:18px;display:none' }),
        h('span', { style: 'width:1px;height:16px;background:#2a2a2a' }),
        h('div', { style: 'display:flex;align-items:baseline;gap:8px' },
          h('span', { style: 'font-weight:700;letter-spacing:.02em;color:#edeef0' }, 'dc-twin'),
          h('span', { className: 'tc-tb-sub', style: 'font-size:11px;color:#757575' }, src.kind === 'live' ? 'twin view · live' : 'twin view'))),
      h('nav', { style: 'display:flex;align-items:stretch;align-self:stretch;gap:2px;flex:none' },
        rail.map(([k, l, c]) => {
          const count = k === 'cockpit' && nAct ? `${nAct} ▲` : c;
          return h('button', { key: k, className: 'hv-rail', onClick: () => A.set({ screen: k }), style: `display:flex;align-items:center;gap:6px;padding:0 11px;border:0;border-bottom:2px solid ${S.screen === k ? '#965cde' : 'transparent'};background:transparent;color:${S.screen === k ? '#edeef0' : '#b3b3b3'};font-size:13px;cursor:pointer` },
            h('span', null, l), h('span', { style: `font:10.5px ${MONO};color:${k === 'cockpit' && nAct ? '#ec7575' : '#4d4d4d'}` }, count));
        })),
      h('div', { style: 'flex:1' }),
      h('div', { className: 'tc-tb-estate', title: `${src.estate.name} · seed ${src.estate.seed ?? '—'} · ${src.estate.packs ?? '—'} packs · ${vm} vm / ${devs - vm} model`, style: `display:flex;align-items:center;gap:6px;font:11px ${MONO};color:#757575;flex:none;white-space:nowrap` },
        h('span', { style: 'color:#b3b3b3' }, src.estate.name), h('span', null, `${Object.keys(src.racks).length} racks · ${devs} dev`)),
      h('div', { style: `display:flex;align-items:center;gap:10px;font:11px ${MONO};color:#757575;flex:none;white-space:nowrap` },
        h('span', { title: src.kind === 'live' ? 'round trip of the last GET /v1/status' : 'projection lag', style: 'display:flex;align-items:center;gap:6px' }, h('span', { style: `width:6px;height:6px;border-radius:50%;background:${src.kind === 'live' && src.down ? '#ec7575' : lag > 80 ? '#d1b854' : '#479d5f'}` }), src.kind === 'live' ? (src.down ? 'twin not answering' : `api ${lag} ms`) : `lag ${lag} ms`),
        h('span', { style: `color:${inc ? '#ec7575' : '#757575'}`, title: 'twin incidents: a result shown while the twin has an incident is not a result' }, `${inc} incidents`),
        src.kind === 'live' && src.stream !== 'open' ? h('span', { style: 'color:#d1b854', title: 'the event stream' }, `stream ${src.stream}`) : null),
      h('span', { title: st.title, style: `font-size:11px;color:${src.kind === 'live' ? '#5b9dff' : '#d4a853'};border:1px solid ${src.kind === 'live' ? '#5b9dff66' : '#d4a85366'};border-radius:999px;padding:1px 9px;flex:none;white-space:nowrap` }, st.text),
      h('div', { style: `display:flex;align-items:center;gap:8px;font:12px ${MONO};color:#b3b3b3;flex:none` },
        h('span', { style: 'color:#757575' }, src.clockLabel ? src.clockLabel() : 'wall'), h('span', { style: 'color:#edeef0' }, src.clock()),
        src.canAdvance && src.canAdvance() ? [['10s', '+10s'], ['1m', '+1m']].map(([d, l]) => h('button', { key: d, onClick: () => src.advance(d), title: 'advance the virtual clock (POST /v1/clock/advance)', style: `background:none;border:1px solid #2a2a2a;border-radius:4px;color:#b3b3b3;font:10.5px ${MONO};padding:1px 6px;cursor:pointer`, className: 'hv-act' }, l)) : null));
  }

  // ------------------------------------------------------------------ floor cells
  function cell(A, M, r) {
    const S = A.S, src = A.src, a = M.aggs[r.name], sel = M.R && r.name === M.R.name;
    let val = '', bg = '#18191b', barPct = 0, barColor = 'transparent', barTrack = 'transparent', valColor = '#757575';
    let border = `1px solid ${SEVN[a.health] ? SEVC[a.health] : '#2a2a2a'}`;
    const cap = src.rackCap(r);
    if (S.overlay === 'health') { val = a.flagged ? `${a.flagged} flagged` : 'nominal'; valColor = a.flagged ? SEVC[a.health] : '#4d4d4d'; }
    if (S.overlay === 'power') { const p = a.w / cap; val = `${(a.w / 1000).toFixed(0)} kW · ${Math.round(p * 100)}%`; barPct = Math.min(100, Math.round(p * 100)); barColor = p > 0.9 ? '#d1b854' : '#965cde'; barTrack = '#0a0a0a'; valColor = '#b3b3b3'; }
    if (S.overlay === 'temp') { const t = a.temp || 0; const x = Math.max(0, Math.min(1, (t - 22) / 26)); bg = `color-mix(in srgb, #18191b ${Math.round(100 - x * 55)}%, #d1b854)`; val = a.temp ? `${t.toFixed(1)} °C` : ''; valColor = '#cfcfcf'; }
    if (S.overlay === 'domains') { val = r.f === 'nvl' ? `NVLink ${a.dom || '—'}` : ((src.cdus || []).find((c) => c.racks.includes(r.name)) || {}).name || r.cooling; if (r.f === 'nvl') border = `1.5px dashed ${a.dom === 'Ready' ? '#965cde' : '#d1b854'}`; valColor = '#b3b3b3'; }
    const mark = (a.unreach ? '⊘ ' : '') + SEVM[a.health];
    return { isRack: true, r, name: r.name, tag: r.tag + (r.slots.some((p) => src.devs[p] && src.devs[p].fid === 'vm') ? ' · vm' : ''), val, valColor, bg, border: sel ? '1px solid #965cde' : border, outline: sel ? '1px solid #965cde55' : 'none',
      mark, markColor: a.unreach && !SEVN[a.health] ? '#d1b854' : SEVC[a.health], barPct, barColor, barTrack, onClick: () => A.selectRack(r.name), fx: fxEl(src, r.name, true) };
  }
  function floorRows(A, M) {
    const S = A.S, src = A.src, design = S.screen === 'design';
    return src.rows.map((row) => ({ name: row.name, cells: Array.from({ length: row.positions }, (_, i) => {
      const pos = i + 1; const r = Object.values(src.racks).find((x) => x.row === row.name && x.pos === pos);
      if (r) {
        const c = cell(A, M, r);
        if (!design) return c;
        // in Design a click selects the rack; the Draft panel offers to remove it (and the 2D
        // tile's ✕ marks it at once). A rack marked for removal is drawn red and dashed.
        const rm = S.draft.find((x) => x.op === 'remove' && x.name === r.name);
        c.removing = !!rm; c.designSel = S.designSel === r.name;
        c.onClick = () => { if (!A.dragMoved()) A.set({ designSel: S.designSel === r.name ? null : r.name }); };
        c.toggleRemove = () => A.set({ designSel: r.name, draft: rm ? S.draft.filter((x) => x !== rm) : [...S.draft, { op: 'remove', name: r.name, row: r.row, pos: r.pos, fmt: r.title }] });
        if (rm) Object.assign(c, { val: '− remove', valColor: '#ec7575', border: '1.5px dashed #ec7575', bg: '#861a1a22', mark: '−', markColor: '#ec7575', outline: 'none' });
        return c;
      }
      const dr = S.draft.find((x) => x.op !== 'remove' && x.row === row.name && x.pos === pos);
      return { isRack: false, isEmpty: true, dash: dr ? '#965cde' : design ? '#965cde66' : '#232323', bg: dr ? '#965cde14' : 'transparent', cursor: design ? 'pointer' : 'default', plus: dr ? dr.name : design ? '+' : '', plusSize: dr ? 12 : 18, draft: dr ? src.formatTagOf(dr.fmt) + ' · draft' : '',
        onClick: () => { if (!design || A.dragMoved()) return; if (dr) A.set({ draft: S.draft.filter((x) => x !== dr) }); else A.set({ draft: [...S.draft, { name: `${row.name}${String(pos).padStart(2, '0')}`, row: row.name, pos, fmt: S.designFmt }] }); } };
    }) }));
  }

  // ------------------------------------------------------------------ ISO scene
  function iso3d(A, M, rows) {
    const S = A.S, src = A.src, aggs = M.aggs, R = M.R, D = M.D;
    const PX = 48, RD = 78, PY = 210, anim = S.animate, L = S.layers, now = Date.now();
    const nRows = Math.max(1, src.rows.length), maxPos = Math.max(1, ...src.rows.map((r) => r.positions));
    const W = Math.max(10, maxPos) * PX, H = (nRows - 1) * PY + RD, PZ = 1, NZ = 1.2, NET0 = 6, NETGAP = 7;
    const infra = src.caps.infra;
    const ledC = (d) => {
      if (D && d.path === D.path) return ['#b48ae8', '0 0 6px #965cde', 'none'];
      if (d.power !== 'On') return ['#1c1c1e', 'none', 'none'];
      if (d.health === 'Critical') return ['#ec7575', '0 0 8px #ec7575', 'dcBlink 0.9s infinite'];
      if (d.health === 'Warning' || !d.bmc) return ['#d1b854', '0 0 6px #d1b85499', 'none'];
      const sw = d.cls === 'switch-tray' || d.cls === 'switch';
      return [sw ? '#6a7386' : '#4a5263', sw ? '0 0 4px rgba(150,170,210,0.35)' : 'none', 'none'];
    };
    const cells = [];
    rows.forEach((row, ri) => row.cells.forEach((c, ci) => {
      const x = ci * PX, y = ri * PY;
      if (!c.isRack) { cells.push({ ...c, x, y }); return; }
      const r = c.r, a = aggs[r.name], sel = S.screen === 'design' ? S.designSel === r.name : R && r.name === R.name;
      const cap = src.rackCap(r);
      const hh = S.overlay === 'power' ? Math.round(36 + Math.min(1, a.w / cap) * 110) : r.f === 'net' ? 120 : 132;
      const leds = r.slots.map((p) => src.devs[p]).filter(Boolean).map((d) => { const [col, g, an] = ledC(d); return { f: Math.max(1, d.units), c: col, g, anim: an }; });
      const crit = a.health === 'Critical', warn = a.health === 'Warning';
      let top = sel ? '#2a2140' : '#222329';
      if (S.overlay === 'temp' && a.temp) { const t = Math.max(0, Math.min(1, (a.temp - 22) / 26)); top = `color-mix(in srgb, #18191c ${Math.round(100 - t * 60)}%, #d1b854)`; }
      if (S.overlay === 'domains' && r.f === 'nvl') top = a.dom === 'Ready' ? '#1e1830' : '#2d2716';
      cells.push({ ...c, x, y, h: hh, leds, top,
        edge: sel ? '#965cde' : crit ? '#ec7575aa' : warn ? '#d1b85488' : '#ffffff1c',
        topGlow: sel ? '0 0 18px rgba(150,92,222,0.55)' : crit ? '0 0 18px rgba(236,117,117,0.45)' : 'none',
        tint: sel ? 'rgba(150,92,222,0.14)' : crit ? 'rgba(236,117,117,0.16)' : warn ? 'rgba(209,184,84,0.08)' : 'transparent',
        glow: sel ? '0 0 34px 6px rgba(150,92,222,0.35)' : crit ? '0 0 40px 10px rgba(236,117,117,0.32)' : warn ? '0 0 24px 4px rgba(209,184,84,0.18)' : 'none',
        onClick: () => { if (!A.dragMoved()) c.onClick(); } });
      if (c.removing) Object.assign(cells[cells.length - 1], { edge: '#ec7575', top: '#3a1d1d', tint: 'rgba(236,117,117,0.18)', glow: '0 0 30px 6px rgba(236,117,117,0.28)', topGlow: '0 0 14px rgba(236,117,117,0.4)',
        leds: leds.map((l) => ({ ...l, c: '#3a1d1d', g: 'none', anim: 'none' })) });
    }));
    // every cooling loop's CDU, in-row at the head of the first row its racks are in (side by
    // side when two loops share a row); a loop with no racks after the last row, so it is seen
    const cdus = src.cdus || (src.cdu ? [{ ...src.cdu, racks: src.loop }] : []);
    const loopRows = (c) => [...new Set(c.racks.map((n) => src.racks[n]).filter(Boolean).map((r) => src.rows.findIndex((x) => x.name === r.row)))].filter((ri) => ri >= 0).sort((a, b) => a - b);
    const slot = {}; let idleLoops = 0;
    const cduAt = cdus.map((c) => {
      const rows = loopRows(c), lanes = {};
      for (const ri of rows) lanes[ri] = slot[ri] = (slot[ri] ?? -1) + 1;
      if (!rows.length) { const k = idleLoops++; return { c, rows, lanes, x: -PX - 14 - k * (PX + 6), y: (nRows - 1) * PY + RD + 120 }; }
      return { c, rows, lanes, x: -PX - 14 - lanes[rows[0]] * (PX + 6), y: rows[0] * PY };
    });
    for (const { c, rows, x, y } of cduAt) {
      const bad = c.state !== 'OK', idle = !rows.length;
      cells.push({ isRack: true, isCdu: true, name: c.name, x, y, h: 132, mark: idle ? 'no racks' : bad ? '▲ flow' : '', markColor: idle ? '#d1b854' : '#ec7575', val: c.path ? c.supplyC.toFixed(1) + '°C' : 'no CDU',
        leds: Array.from({ length: 8 }, (_, i) => ({ f: 1, c: bad ? (i % 2 ? '#ec7575' : '#3a1d1d') : !idle && i < 2 ? '#5b9dff' : '#23324a', g: bad ? '0 0 6px #ec7575' : !idle && i < 2 ? '0 0 6px #5b9dff' : 'none', anim: bad ? 'dcBlink 0.9s infinite' : 'none' })),
        top: '#141a24', edge: bad ? '#ec757588' : idle ? '#d1b85466' : '#5b9dff44', topGlow: 'none', tint: bad ? 'rgba(236,117,117,0.14)' : idle ? 'rgba(209,184,84,0.06)' : 'rgba(91,157,255,0.06)', glow: 'none', fx: null, onClick: () => { if (!A.dragMoved()) A.set({ screen: 'facility' }); } });
    }

    // the electrical room
    const sel = S.infraSel; const pick = (id) => () => { if (!A.dragMoved()) A.set({ infraSel: sel === id ? null : id }); };
    const box = (id, name, x, y, w, d, hgt, state, sev, meter) => {
      const bad = sev === 'crit', warn = sev === 'warn', on = sel === id;
      return { key: id, name, x, y, w, d, h: hgt, state, stateC: bad ? '#ec7575' : warn ? '#d1b854' : '#757575',
        led: bad ? '#ec7575' : warn ? '#d1b854' : '#965cde', ledGlow: bad ? '0 0 8px #ec7575' : warn ? '0 0 8px #d1b854' : '0 0 6px #965cde99', anim: bad || warn ? 'dcBlink 0.9s infinite' : 'none',
        meter, meterC: bad ? '#ec7575' : warn ? '#d1b854' : '#965cde',
        top: on ? '#2a2140' : '#1d1e24', edge: on ? '#965cde' : bad ? '#ec7575aa' : warn ? '#d1b85488' : '#ffffff1c', topGlow: on ? '0 0 18px rgba(150,92,222,0.55)' : bad ? '0 0 16px rgba(236,117,117,0.45)' : 'none',
        tint: on ? 'rgba(150,92,222,0.16)' : bad ? 'rgba(236,117,117,0.16)' : warn ? 'rgba(209,184,84,0.1)' : 'transparent', glow: on ? '0 0 34px 6px rgba(150,92,222,0.35)' : bad ? '0 0 40px 10px rgba(236,117,117,0.3)' : warn ? '0 0 24px 4px rgba(209,184,84,0.2)' : 'none', onClick: pick(id) };
    };
    // every feed of the estate, top to bottom in the electrical room: its switchgear and UPS where
    // the power chain has one (or the feed itself), and a panel per row
    const feedNames = Object.keys(src.feeds), nF = feedNames.length;
    const feedK = Object.fromEntries(feedNames.map((n, k) => [n, k]));
    // the chain's nodes the floor has no place for (not the UPS at a feed's head, not a row's
    // panel): a panel that serves no rack, a second UPS, a busway; small, under their feed
    const placed = new Set([...Object.values(src.ups || {}).map((u) => u.name), ...Object.values(src.rppNode || {}).map((p) => p.name)]);
    const loose = infra ? (src.chain || []).filter((x) => !placed.has(x.name) && feedK[x.feed] !== undefined) : [];
    const looseOf = (n) => loose.filter((x) => x.feed === n);
    const gapF = nF > 1 ? Math.max(RD + 24 + (loose.length ? 36 : 0), ((nRows - 1) * PY) / (nF - 1)) : 0;
    const feedY = (n) => feedK[n] * gapF;
    const hasUps = (n) => infra && src.ups && src.ups[n];
    const rowFeeds = (rr) => { const used = new Set(rr.flatMap((r) => rackFeeds(r))); return feedNames.filter((n) => used.has(n)); };
    const rackFeeds = (r) => (r.feeds && r.feeds.length ? r.feeds : feedNames.slice(0, 2));
    const fedRacks = (n) => Object.values(src.racks).filter((r) => rackFeeds(r).includes(n));
    const boxes = [];
    feedNames.forEach((n) => {
      if (hasUps(n)) {
        const ut = src.utility[n].up, U = src.ups[n];
        boxes.push(box(`swgr-${n}`, `SWGR-${n}`, -392, feedY(n), 62, RD, 118, ut ? 'UTIL OK' : 'UTIL LOST', ut ? '' : 'crit', ut ? 70 : 0));
        boxes.push(box(`ups-${n}`, `UPS-${n}`, -312, feedY(n), 56, RD, 104, U.mode === 'online' ? 'ONLINE' : U.mode === 'battery' ? `BATT ${U.batt}%` : U.mode === 'fault' ? 'FAULT' : 'EMPTY', U.mode === 'online' ? '' : U.mode === 'battery' ? 'warn' : 'crit', U.mode === 'online' ? 100 : U.batt));
      } else {
        const f = src.feeds[n], pct = f.capW ? Math.round((src.feedLoad(n, aggs) / f.capW) * 100) : 0;
        boxes.push(box(`feed-${n}`, `FEED-${n}`, -392, feedY(n), 62, RD, 118, !f.up ? 'LOST' : fedRacks(n).length ? `UP ${pct}%` : 'NO RACKS', f.up ? '' : 'crit', f.up ? Math.max(8, pct) : 0));
      }
      looseOf(n).forEach((x, i) => {
        const idle = !(x.racks || []).length, down = !x.up;
        boxes.push(box(`node:${x.name}`, x.name.toUpperCase(), -392 + i * 46, feedY(n) + RD + 8, 40, 26, 44, down ? (x.mode === 'dead' ? 'NO SUPPLY' : x.mode.toUpperCase()) : idle ? 'NO RACKS' : `${(x.racks || []).length} RACKS`, down ? 'crit' : idle ? 'warn' : '', idle ? 0 : 60));
      });
    });
    // a row's panels: in the demo one per row; live, the twin's panels that serve the row's racks,
    // named after them (a row no panel serves has no box: its racks draw from the feeds)
    const panelOf = (row, n) => (src.kind === 'live' ? (src.rppNode || {})[`${row}-${n}`] : { name: `rpp-${row}` });
    const rowPanels = {};
    if (infra) src.rows.forEach((row, i) => {
      const fs = rowFeeds(Object.values(src.racks).filter((r) => r.row === row.name)).filter((n) => panelOf(row.name, n)); if (!fs.length) return;
      rowPanels[i] = true;
      const names = [...new Set(fs.map((n) => panelOf(row.name, n).name.toUpperCase()))];
      const tr = fs.map((n) => [n, !!src.rpp[`${row.name}-${n}`] || !src.feeds[n].up]);
      boxes.push(box(`rpp-${row.name}`, names.length > 1 ? `${names[0]} +${names.length - 1}` : names[0], -182, i * PY + 10, 46, 58, 92, tr.map(([n, t]) => `${n}${t ? '✕' : '✓'}`).join(' '), tr.some(([, t]) => t) ? 'crit' : '', 60));
    });
    const room = { x: -408, y: -24, w: 168, h: Math.max(H, (nF - 1) * gapF + RD + (loose.length ? 36 : 0)) + 48 };

    // flows
    const OX = 160, OY = 60, SW = W + 320, SH = H + 120;
    const mk = (els) => h('svg', { width: SW, height: SH, style: `position:absolute;left:${-OX}px;top:${-OY}px;overflow:visible` }, h('g', { transform: `translate(${OX},${OY})` }, els));
    const line = (key, d, c, w, dur, broken) => {
      const o = [h('path', { key: key + 'g', d, fill: 'none', stroke: c, 'stroke-opacity': broken ? 0.85 : 0.22, 'stroke-width': w + 3, 'stroke-linecap': 'round', style: 'filter:blur(2px)' }),
        h('path', { key: key + 'b', d, fill: 'none', stroke: c, 'stroke-opacity': broken ? 1 : 0.45, 'stroke-width': w, 'stroke-dasharray': broken ? '5 5' : null })];
      if (!broken && anim) o.push(h('path', { key: key + 'f', d, fill: 'none', stroke: '#ffffff', 'stroke-opacity': 0.9, 'stroke-width': w + 0.6, 'stroke-linecap': 'round', 'stroke-dasharray': '1.5 14', style: `animation:dcFlow ${dur.toFixed(2)}s linear infinite;filter:drop-shadow(0 0 3px ${c})` }));
      return o;
    };
    const lab = (key, x, y, t, c) => h('text', { key, x, y, fill: c, 'font-size': 10, 'font-family': MONO, 'letter-spacing': 1.5 }, t);
    const ri = Object.fromEntries(src.rows.map((r, i) => [r.name, i]));
    const racks = Object.values(src.racks);
    const power = [], network = [], cooling = [];
    const FEEDC = ['#965cde', '#b48ae8', '#7b8cff', '#e08ad0', '#8fd0c4'];
    const feedC = (n) => FEEDC[feedK[n] % FEEDC.length];
    // Each feed from its utility line through the switchgear and UPS (or straight from the feed)
    // to a riser, which drops a line into each row it feeds: into the row's panel, or to the
    // row's bus where the estate has no panels.
    const lanes = src.rows.map((row, i) => {
      const rr = racks.filter((r) => r.row === row.name), fs = rowFeeds(rr), m = fs.length;
      return { row, i, rr, fs, exit: (n) => i * PY + RD * (m === 1 ? 0.5 : 0.3 + (0.4 * fs.indexOf(n)) / (m - 1)) };
    });
    if (L.power) feedNames.forEach((n) => {
      const k = feedK[n], c = feedC(n), fu = src.feeds[n].up, ut = hasUps(n) ? src.utility[n].up : fu;
      const y0 = feedY(n) + RD / 2, rx = -226 + Math.min(k, 3) * 10;
      power.push(...line(`u${n}`, `M-470,${y0} H-392`, ut ? c : '#ec7575', 2.4, 0.9, !ut));
      power.push(lab(`ul${n}`, -470, y0 - 7, hasUps(n) ? `UTILITY ${n}` : `FEED ${n}`, ut ? c : '#ec7575'));
      const exits = lanes.filter((l) => l.fs.includes(n)).map((l) => l.exit(n)), into = lanes.filter((l) => l.fs.includes(n)).map((l) => (rowPanels[l.i] ? -176 : -136));
      if (!exits.length) { power.push(lab(`un${n}`, -470, y0 + 16, 'feeds no rack', '#757575')); return; }
      if (hasUps(n)) power.push(...line(`su${n}`, `M-330,${y0} H-312`, ut ? c : '#ec7575', 2.4, 0.9, !ut));
      power.push(...line(`ur${n}`, `M${hasUps(n) ? -256 : -330},${y0} H${rx}`, fu ? c : '#ec7575', 2.4, 0.9, !fu));
      const lo = Math.min(y0, ...exits), hi = Math.max(y0, ...exits);
      if (hi > lo) power.push(...line(`uv${n}`, `M${rx},${lo} V${hi}`, fu ? c : '#ec7575', 2.4, 0.9, !fu));
      exits.forEach((y, i) => power.push(...line(`rr${n}${i}`, `M${rx},${y} H${into[i]}`, fu ? c : '#ec7575', 2, 0.9, !fu)));
    });
    // Each row's buses run on the floor along the back of the row, opposite the cooling, which
    // runs along the front: out of the row's RPP (or the feed, where the estate has no panels),
    // into a lane behind the row (one per feed the row's racks draw from, the first outermost, as
    // on the 2D floor), with a tap into the back of every rack on that feed. A tap is red and
    // dashed when that feed is down for the rack.
    if (L.power) lanes.forEach(({ row, i, rr, fs, exit }) => {
      if (!rr.length) return;
      const end = Math.max(...rr.map((r) => r.pos)) * PX, back = i * PY, m = fs.length;
      fs.forEach((n, j) => {
        const c = feedC(n), lane = back - 14 * (m - j), turn = -122 + 8 * j;
        const up = src.feeds[n].up && !(src.rpp && src.rpp[`${row.name}-${n}`]), col = up ? c : '#ec7575';
        const on = rr.filter((r) => rackFeeds(r).includes(n));
        const load = on.reduce((s2, r) => s2 + aggs[r.name].w / src.rackCap(r), 0) / Math.max(1, on.length), dur = Math.max(0.5, 2.2 - load * 1.6);
        power.push(...line(`p${n}${i}`, `M-136,${exit(n)} H${turn} V${lane} H${end - 6}`, col, 2, dur, !up));
        // live, a bus no panel of a feed with a UPS feeds draws from the feed itself: no ride-through
        const bypass = src.kind === 'live' && hasUps(n) && !panelOf(row.name, n);
        power.push(lab(`pl${n}${i}`, end + 2, lane + 3, `BUS ${row.name.toUpperCase()}-${n}${bypass ? ` · NO UPS (no ${n} panel serves it)` : ''}`, bypass ? '#d1b854' : col));
        on.forEach((r) => {
          const x = (r.pos - 1) * PX + (m <= 2 ? [12, 34][j] : 8 + (j * 32) / (m - 1)), ok = !src.feedBroken(r, n), tc = ok ? c : '#ec7575';
          power.push(...line(`pt${n}${r.name}`, `M${x},${lane} V${back}`, tc, 1.4, dur, !ok));
          power.push(h('circle', { key: `pd${n}${r.name}`, cx: x, cy: lane, r: 2.4, fill: tc }));
        });
      });
    });
    // The networks run on the floor along the front of each row (power runs along the back): a
    // lane per cabled fabric (OOB, frontend, backend IB, RoCE, …) in its own colour, with a tap
    // from the front of every rack that has a port on it, joined by the fabric's trunk at the end
    // of the hall; the rack where the fabric is switched has a heavier tap. A tap is red and
    // dashed when that rack has a pulled cable on the fabric.
    const fabs = src.fabrics().filter((f) => S.layers['net:' + f.name] !== 0);
    const laneY = (i, k) => i * PY + RD + NET0 + k * NETGAP, tapX = (k) => [8, 18, 28, 38][k % 4];
    fabs.forEach((f, k) => {
      const rowsOn = [], tx = W + 30 + k * 12;
      src.rows.forEach((row, i) => {
        const on = racks.filter((r) => r.row === row.name && f.racks.has(r.name)); if (!on.length) return;
        rowsOn.push(i);
        const y = laneY(i, k), x0 = (Math.min(...on.map((r) => r.pos)) - 1) * PX + tapX(k);
        network.push(...line(`n${f.name}${i}`, `M${x0},${y} H${tx}`, f.color, 1.4, 0.8, false));
        on.forEach((r) => {
          const x = (r.pos - 1) * PX + tapX(k), down = f.down.has(r.name), hub = r.name === f.hub, c = down ? '#ec7575' : f.color;
          network.push(...line(`nt${f.name}${r.name}`, `M${x},${i * PY + RD} V${y}`, c, hub ? 2.2 : 1.1, 0.8, down));
          network.push(h('circle', { key: `nd${f.name}${r.name}`, cx: x, cy: y, r: hub ? 3.2 : 1.9, fill: c }));
          if (down) network.push(h('circle', { key: `nx${f.name}${r.name}`, cx: x, cy: y, r: 5.5, fill: 'none', stroke: '#ec7575', 'stroke-width': 1.4 }));
        });
      });
      if (!rowsOn.length) return;
      const y0 = laneY(rowsOn[0], k), y1 = laneY(rowsOn[rowsOn.length - 1], k);
      if (y1 > y0) network.push(...line(`ntr${f.name}`, `M${tx},${y0} V${y1}`, f.color, 2, 0.8, false));
      network.push(lab(`nl${f.name}`, W + 30 + fabs.length * 12, y0 + 3, `${f.short.toUpperCase()}${f.hub ? ' · ' + f.hub : ''}`, f.color));
    });
    // each loop's supply and return along the front of each row it has racks in, below the
    // networks (a second loop in a row a lane further out), with a drop to every rack on it
    if (L.cooling) for (const { c, rows, lanes } of cduAt) {
      const bad = c.state !== 'OK', sc = bad ? '#ec7575' : '#5b9dff', dur = bad ? 5 : 1.0;
      rows.forEach((ri, n) => {
        const lp = c.racks.map((k) => src.racks[k]).filter((r) => r && r.row === src.rows[ri].name); if (!lp.length) return;
        const j = lanes[ri], y0 = ri * PY, cx0 = -PX - 14 - j * (PX + 6) + 23, ys = y0 + RD + NET0 + 4 * NETGAP + 6 + j * 34, yr = ys + 16, xe = (Math.max(...lp.map((r) => r.pos)) - 1) * PX + 34;
        cooling.push(...line(`cs${c.name}${ri}`, `M${cx0 - 8},${y0 + RD} V${ys} H${xe}`, sc, 2.2, dur, false));
        cooling.push(...line(`cr${c.name}${ri}`, `M${xe + 10},${y0 + RD} V${yr} H${cx0 + 8} V${y0 + RD}`, '#d4a853', 2.2, dur, false));
        lp.forEach((r) => { const x = (r.pos - 1) * PX; cooling.push(...line(`csd${c.name}${r.name}`, `M${x + 14},${ys} V${y0 + RD}`, sc, 1.2, dur, false), ...line(`crd${c.name}${r.name}`, `M${x + 32},${y0 + RD} V${yr}`, '#d4a853', 1.2, dur, false)); });
        if (n === 0) cooling.push(lab(`cl${c.name}`, cx0 - 10, yr + 16, bad ? `${c.name.toUpperCase()} · FLOW LOW` : `${c.name.toUpperCase()} · SUPPLY / RETURN`, bad ? '#ec7575' : '#5b9dff'));
      });
    }
    const rings = [];
    for (const [p, t] of Object.entries(src.pulses)) {
      if (now - t > 2800 || !src.racks[p]) continue; const r = src.racks[p];
      const cx = (r.pos - 1) * PX + 23, cy = ri[r.row] * PY + RD / 2;
      rings.push(h('div', { key: p + t, style: `position:absolute;left:${cx - 70}px;top:${cy - 70}px;width:140px;height:140px;border-radius:50%;border:2px solid #ec7575;box-shadow:0 0 30px #ec7575, inset 0 0 30px rgba(236,117,117,0.4);animation:dcRing 1.3s ease-out 2;pointer-events:none` }));
    }
    // the camera frames the hall and what is drawn beyond it (feeds lower in the electrical room,
    // a loop with no racks after the last row)
    const Hx = Math.max(H, room.y + room.h - 24, ...cduAt.map((a) => a.y + RD));
    const fit = Math.max(0.45, Math.min(2, (S.fw - 10) / (W + 340), (S.fh - 10) / Math.max(200, Hx - 48)));
    const aisles = [];
    for (let i = 0; i < nRows - 1; i++) aisles.push({ y: i * PY + RD, h: PY - RD, label: i % 2 ? 'HOT AISLE · CONTAINED' : 'COLD AISLE' });
    return { W, H, cells, aisles, rowLabels: src.rows.map((r, i) => ({ name: r.name.toUpperCase(), y: i * PY + RD / 2 - 8 })), infra: boxes, room,
      power: L.power ? mk(power) : null, network: network.length ? mk(network) : null, cooling: L.cooling ? mk(cooling) : null, rings,
      pz: PZ, nz: NZ, fit, cx: (W - 300) / 2, cy: Hx / 2, PY };
  }

  // The ISO camera. The scene hangs from an anchor at the centre of the view; the orbit target
  // (the floor point at the centre, the scene's centre plus the pan offset tx, ty) is moved onto
  // the anchor, then the scene is turned about it (rotateZ), tilted (rotateX) and scaled. So
  // rotating and zooming always pivot on what is in the middle of the view, and panning moves
  // that point across the floor.
  function camTransform(S, geo) {
    const s = geo.fit * S.zoom;
    return `scale(${s.toFixed(4)}) rotateX(${S.rx.toFixed(2)}deg) rotateZ(${S.rz.toFixed(2)}deg) translate(${(-(geo.cx + S.tx)).toFixed(1)}px,${(-(geo.cy + S.ty)).toFixed(1)}px)`;
  }

  function isoScene(A, iso, designMode) {
    const S = A.S;
    A._cam = { fit: iso.fit, cx: iso.cx, cy: iso.cy };
    const infraEl = (b) => h('div', { key: b.key, onClick: b.onClick, style: `position:absolute;left:${b.x}px;top:${b.y}px;width:${b.w}px;height:${b.d}px;transform-style:preserve-3d;cursor:pointer` },
      h('div', { style: `position:absolute;inset:-10px;border-radius:6px;background:${b.tint};box-shadow:${b.glow}` }),
      h('div', { style: `position:absolute;left:0;top:0;width:${b.w}px;height:${b.h}px;transform-origin:top;transform:rotateX(90deg);background:#17181c;border:1px solid #ffffff12` }),
      h('div', { style: `position:absolute;left:0;top:0;width:${b.h}px;height:${b.d}px;transform-origin:left;transform:rotateY(-90deg);background:#16171a;border:1px solid #ffffff10` }),
      h('div', { style: `position:absolute;left:${b.w}px;top:0;width:${b.h}px;height:${b.d}px;transform-origin:left;transform:rotateY(-90deg);background:linear-gradient(90deg,#141518,#1d1e23);border:1px solid #ffffff14;display:flex;flex-direction:column;justify-content:center;gap:4px;padding:0 6px` },
        h('div', { style: `height:3px;border-radius:2px;background:${b.led};box-shadow:${b.ledGlow};animation:${b.anim}` }),
        h('div', { style: 'height:3px;border-radius:2px;background:#2a2c33' }), h('div', { style: 'height:3px;border-radius:2px;background:#2a2c33' })),
      h('div', { style: `position:absolute;left:0;top:${b.d}px;width:${b.w}px;height:${b.h}px;transform-origin:top;transform:rotateX(90deg);background:linear-gradient(0deg,#1a1b20,#23242a);border:1px solid ${b.edge};display:flex;flex-direction:column-reverse;gap:3px;padding:7px 6px` },
        h('div', { style: `height:4px;border-radius:2px;background:${b.led};box-shadow:${b.ledGlow};animation:${b.anim}` }),
        h('div', { style: 'height:16px;border-radius:2px;background:#101114;border:1px solid #2a2c33;overflow:hidden;display:flex;align-items:flex-end' }, h('div', { style: `width:100%;height:${b.meter}%;background:${b.meterC}` })),
        h('div', { style: 'flex:1;border-radius:2px;background:repeating-linear-gradient(0deg,#1f2026 0 2px,#16171b 2px 5px)' })),
      h('div', { style: `position:absolute;inset:0;transform:translateZ(${b.h}px);background:${b.top};border:1px solid ${b.edge};box-shadow:${b.topGlow};padding:6px 6px;display:flex;flex-direction:column;gap:6px;overflow:hidden` },
        h('span', { style: `font:700 10px/1.1 ${MONO};color:#edeef0;white-space:nowrap` }, b.name),
        h('span', { style: `font:600 9px/1.1 ${MONO};color:${b.stateC};white-space:nowrap` }, b.state)));
    const rackEl = (c, k) => h('div', { key: 'r' + c.name, onClick: c.onClick, style: `position:absolute;left:${c.x}px;top:${c.y}px;width:46px;height:78px;transform-style:preserve-3d;cursor:pointer` },
      h('div', { style: 'position:absolute;left:4px;top:6px;width:54px;height:84px;background:rgba(0,0,0,0.55);filter:blur(5px)' }),
      h('div', { style: `position:absolute;inset:-8px;border-radius:6px;background:${c.tint};box-shadow:${c.glow}` }),
      h('div', { style: `position:absolute;left:0;top:0;width:46px;height:${c.h}px;transform-origin:top;transform:rotateX(90deg);background:linear-gradient(0deg,#16171a,#1e1f24);border:1px solid #ffffff14` }),
      h('div', { style: `position:absolute;left:0;top:0;width:${c.h}px;height:78px;transform-origin:left;transform:rotateY(-90deg);background:#18191d;border:1px solid #ffffff12` }),
      h('div', { style: `position:absolute;left:46px;top:0;width:${c.h}px;height:78px;transform-origin:left;transform:rotateY(-90deg);background:linear-gradient(90deg,#141518,#1c1d22);border:1px solid #ffffff14` }),
      h('div', { style: `position:absolute;left:0;top:78px;width:46px;height:${c.h}px;transform-origin:top;transform:rotateX(90deg);background:linear-gradient(0deg,#191a1e,#202127);border:1px solid ${c.edge};display:flex;flex-direction:column-reverse;gap:1px;padding:5px 5px` },
        c.leds.map((l) => h('div', { style: `flex:${l.f} 1 0;min-height:1px;border-radius:1px;background:${l.c};box-shadow:${l.g};animation:${l.anim}` }))),
      h('div', { style: `position:absolute;inset:0;transform:translateZ(${c.h}px);background:${c.top};border:1px solid ${c.edge};box-shadow:${c.topGlow};padding:6px 5px;display:flex;flex-direction:column;gap:2px;overflow:hidden` },
        h('span', { style: `font:700 11px ${MONO};color:#edeef0` }, c.name),
        h('span', { style: `font-size:9px;color:${c.markColor}` }, c.mark),
        h('div', { style: 'flex:1' }),
        h('span', { style: `font:8.5px ${MONO};color:#b3b3b3;writing-mode:vertical-rl;transform:rotate(180deg);align-self:flex-start` }, c.val),
        c.fx));
    const emptyEl = (c, k) => h('div', { key: 'e' + k, onClick: c.onClick, className: designMode ? 'hv-empty' : '', style: `position:absolute;left:${c.x}px;top:${c.y}px;width:46px;height:78px;border:1px dashed ${c.dash};background:${c.bg};border-radius:2px;display:flex;flex-direction:column;align-items:center;justify-content:center;cursor:${c.cursor};transform:translateZ(1px)` },
      h('span', { style: `font-size:${c.plusSize}px;color:#965cde;line-height:1` }, c.plus),
      h('span', { style: `font:8px ${MONO};color:#b48ae8;text-align:center` }, c.draft));
    return h('div', { style: 'position:absolute;inset:0;overflow:hidden;perspective:2400px;perspective-origin:50% 50%' },
      h('div', { ref: A.camRef, style: `position:absolute;left:50%;top:50%;width:0;height:0;transform-origin:0 0;transform-style:preserve-3d;will-change:transform;transform:${camTransform(S, A._cam)};transition:${S.dragging ? 'none' : 'transform .35s cubic-bezier(.2,.7,.2,1)'}` },
      h('div', { style: `position:absolute;left:0;top:0;width:${iso.W}px;height:${iso.H}px;transform-style:preserve-3d` },
        h('div', { style: 'position:absolute;left:-420px;top:-110px;right:-120px;bottom:-110px;background-color:#0c0c0e;background-image:repeating-linear-gradient(0deg,#1b1b1f 0 1px,transparent 1px 24px),repeating-linear-gradient(90deg,#1b1b1f 0 1px,transparent 1px 24px);border:1px solid #2a2a2a;box-shadow:0 0 120px rgba(150,92,222,0.08) inset' }),
        iso.aisles.map((a, i) => h('div', { key: 'a' + i, style: `position:absolute;left:-150px;right:-110px;top:${a.y}px;height:${a.h}px;background:repeating-linear-gradient(90deg,rgba(91,157,255,0.06) 0 22px,transparent 22px 24px);border-top:1px solid rgba(91,157,255,0.14);border-bottom:1px solid rgba(91,157,255,0.14);display:flex;align-items:center;padding-left:90px` }, h('span', { style: `font:600 10px ${MONO};letter-spacing:.3em;color:rgba(91,157,255,0.45)` }, a.label))),
        iso.rowLabels.map((l) => h('span', { key: 'l' + l.name, style: `position:absolute;left:-128px;top:${l.y}px;font:600 12px ${MONO};letter-spacing:.2em;color:#4d4d4d` }, `ROW ${l.name}`)),
        iso.cooling, iso.rings,
        h('div', { style: `position:absolute;left:${iso.room.x}px;top:${iso.room.y}px;width:${iso.room.w}px;height:${iso.room.h}px;background:repeating-linear-gradient(45deg,rgba(150,92,222,0.05) 0 10px,transparent 10px 20px);border:1px solid rgba(150,92,222,0.22);transform:translateZ(0.5px)` }, h('span', { style: `position:absolute;left:10px;bottom:8px;font:600 10px ${MONO};letter-spacing:.3em;color:rgba(180,138,232,0.55)` }, 'ELECTRICAL · LV ROOM')),
        iso.infra.map(infraEl),
        iso.cells.map((c, k) => (c.isRack ? rackEl(c, k) : emptyEl(c, k))),
        h('div', { style: `position:absolute;left:0;top:0;width:${iso.W}px;height:${iso.H}px;transform:translateZ(${iso.pz}px);pointer-events:none` }, iso.power),
        h('div', { style: `position:absolute;left:0;top:0;width:${iso.W}px;height:${iso.H}px;transform:translateZ(${iso.nz}px);pointer-events:none` }, iso.network))),
      h('div', { className: 'tc-pivot' }, h('span', null, 'orbit centre')));
  }

  // ------------------------------------------------------------------ 2D floor
  function buildFlows(A, M, g, W, H) {
    const S = A.S, src = A.src, aggs = M.aggs, L = S.layers, anim = S.animate, out = [], txt = [];
    const ri = Object.fromEntries(src.rows.map((r, i) => [r.name, i]));
    const rect = (r) => { const x = g.x0 + (r.pos - 1) * g.PX, y = g.y0 + ri[r.row] * g.PY; return { x, y, cx: x + g.T / 2, b: y + g.T }; };
    const add = (key, d, c, w, dur, broken) => {
      out.push(h('path', { key: key + 'b', d, fill: 'none', stroke: c, 'stroke-opacity': broken ? 0.9 : 0.3, 'stroke-width': w, 'stroke-dasharray': broken ? '4 4' : null }));
      if (!broken && anim) out.push(h('path', { key: key + 'f', d, fill: 'none', stroke: c, 'stroke-width': w + 0.8, 'stroke-linecap': 'round', 'stroke-dasharray': '2 10', style: `animation:dcFlow ${dur.toFixed(2)}s linear infinite` }));
    };
    const label = (key, x, y, t, c) => txt.push(h('text', { key, x, y, fill: c, 'font-size': 9, 'font-family': MONO, 'text-anchor': 'end' }, t));
    const racks = Object.values(src.racks);
    // a lane above each row for every feed its racks draw from (the first outermost), with a drop
    // into each rack on that feed
    const feedNames = Object.keys(src.feeds), FEEDC = ['#965cde', '#b48ae8', '#7b8cff', '#e08ad0', '#8fd0c4'];
    const rackFeeds = (r) => (r.feeds && r.feeds.length ? r.feeds : feedNames.slice(0, 2));
    if (L.power) src.rows.forEach((row, i) => {
      const top = g.y0 + i * g.PY, xs = g.x0 - 12, rr = racks.filter((r) => r.row === row.name);
      const used = new Set(rr.flatMap(rackFeeds)), fs = feedNames.filter((n) => used.has(n)), m = fs.length;
      fs.forEach((n, j) => {
        const dy = -11 * (m - j), fx = m === 1 ? 0.5 : 0.3 + (0.4 * j) / (m - 1), c = FEEDC[feedNames.indexOf(n) % FEEDC.length];
        const up = src.feeds[n].up, col = up ? c : '#ec7575';
        label(`pl${n}${i}`, xs - 3, top + dy + 3, 'feed ' + n, col);
        rr.filter((r) => rackFeeds(r).includes(n)).forEach((r) => { const q = rect(r), x = q.x + g.T * fx, pct = aggs[r.name].w / src.rackCap(r); add(`p${n}${r.name}`, `M${xs},${top + dy} H${x} V${q.y}`, col, 1.2, Math.max(0.5, 2.4 - pct * 2), src.feedBroken(r, n)); });
      });
    });
    // a feed no rack draws from has no lane: say so above the first row
    if (L.power) {
      const used = new Set(racks.flatMap(rackFeeds));
      feedNames.filter((n) => !used.has(n)).forEach((n, k) => label(`pi${n}`, g.x0 - 15, g.y0 - 34 - k * 11, `feed ${n} · feeds no rack`, '#757575'));
    }
    // networks: a lane per fabric just below each row, a tap from every rack on it, trunks at the right
    const fabs = src.fabrics().filter((f) => S.layers['net:' + f.name] !== 0);
    const right = g.x0 + Math.max(1, ...src.rows.map((r) => r.positions)) * g.PX;
    fabs.forEach((f, k) => {
      const tx = right + 4 + k * 6, ys = [];
      src.rows.forEach((row, i) => {
        const on = racks.filter((r) => r.row === row.name && f.racks.has(r.name)); if (!on.length) return;
        const y = g.y0 + i * g.PY + g.T + 3 + k * 3; ys.push(y);
        add(`n${f.name}${i}`, `M${rect(on[0]).x + 10 + k * 12},${y} H${tx}`, f.color, 1, 0.9, false);
        on.forEach((r) => { const q = rect(r), x = q.x + 10 + k * 12, down = f.down.has(r.name); add(`nt${f.name}${r.name}`, `M${x},${q.b} V${y}`, down ? '#ec7575' : f.color, r.name === f.hub ? 1.8 : 1, 0.9, down); });
      });
      if (ys.length > 1) add(`ntr${f.name}`, `M${tx},${ys[0]} V${ys[ys.length - 1]}`, f.color, 1.2, 0.9, false);
    });
    // each loop's supply and return under its racks (a second loop a lane further down); a loop
    // with no racks is named above the first row
    if (L.cooling) (src.cdus || (src.cdu ? [{ ...src.cdu, racks: src.loop }] : [])).forEach((c, k) => {
      const lp = c.racks.map((n) => src.racks[n]).filter(Boolean), lost = c.state !== 'OK', sc = lost ? '#ec7575' : '#5b9dff';
      if (!lp.length) { label(`cdu${c.name}`, g.x0 - 15, g.y0 - 34 - (feedNames.length + k) * 11, `${c.name} · no racks`, '#d1b854'); return; }
      const q0 = rect(lp[0]), xs = q0.x - 12, dy = k * 8;
      label(`cdu${c.name}`, xs - 3, q0.b + 20.5 + dy, c.name, sc);
      lp.forEach((r) => { const q = rect(r); add(`cs${c.name}${r.name}`, `M${xs},${q.b + 17 + dy} H${q.cx - 14} V${q.b}`, sc, 1.3, lost ? 5 : 1.1, false); add(`cr${c.name}${r.name}`, `M${q.cx + 14},${q.b} V${q.b + 23 + dy} H${xs}`, '#d4a853', 1.3, lost ? 5 : 1.1, false); });
    });
    return h('svg', { width: W, height: H, style: 'position:absolute;left:0;top:0;overflow:visible;pointer-events:none' }, [...out, ...txt]);
  }

  function floor2d(A, M, rows, designMode) {
    const S = A.S, src = A.src;
    const maxPos = Math.max(1, ...src.rows.map((r) => r.positions));
    const W2 = 60 + maxPos * 84 + 14, H2 = 30 + src.rows.length * 124;
    const zoom2d = Math.max(0.7, Math.min(1.6, (S.fw - 12) / W2, (S.fh - 12) / (H2 + 10))).toFixed(3);
    const flows = buildFlows(A, M, { x0: 60, y0: 30, T: 76, PX: 84, PY: 124 }, W2, H2);
    const rackTile = (c) => designMode
      ? h('div', { key: c.name, onClick: c.onClick, className: 'tc-tile', title: c.removing ? `${c.name} is marked for removal` : `select ${c.name}`, style: `position:relative;width:76px;height:76px;border-radius:6px;background:${c.removing ? '#861a1a22' : '#18191b'};border:${c.removing ? '1.5px dashed #ec7575' : c.designSel ? '1px solid #965cde' : '1px solid #2a2a2a'};outline:${c.designSel ? '1px solid #965cde55' : 'none'};outline-offset:2px;padding:7px 8px;display:flex;flex-direction:column;opacity:${c.removing || c.designSel ? 1 : 0.8};cursor:pointer` },
        h('span', { style: `font-weight:600;font-size:12px;color:${c.removing ? '#ec7575' : '#edeef0'};${c.removing ? 'text-decoration:line-through' : ''}` }, c.name), h('span', { style: 'font-size:10px;color:#757575' }, c.tag),
        h('div', { style: 'flex:1' }), c.removing ? h('span', { style: `font:10px ${MONO};color:#ec7575` }, '− remove') : null,
        h('button', { className: 'tc-x', title: c.removing ? 'keep this rack' : 'remove this rack', onClick: (e) => { e.stopPropagation(); c.toggleRemove(); }, onMouseDown: (e) => e.stopPropagation(),
          style: `position:absolute;top:4px;right:4px;width:18px;height:18px;border-radius:4px;border:1px solid ${c.removing ? '#2a2a2a' : '#ec757566'};background:${c.removing ? '#18191b' : '#861a1a44'};color:${c.removing ? '#cfcfcf' : '#ec7575'};font-size:11px;line-height:1;padding:0;cursor:pointer` }, c.removing ? '↺' : '✕'))
      : h('div', { key: c.name, onClick: c.onClick, className: 'hv-accent', style: `position:relative;width:76px;height:76px;border-radius:6px;background:${c.bg};border:${c.border};outline:${c.outline};outline-offset:2px;cursor:pointer;padding:7px 8px;display:flex;flex-direction:column;overflow:visible` },
        h('div', { style: 'display:flex;justify-content:space-between;align-items:center' },
          h('span', { style: 'font-weight:600;font-size:12px;color:#edeef0' }, c.name),
          h('span', { style: `font-size:10px;color:${c.markColor}` }, c.mark)),
        h('span', { style: 'font-size:10px;color:#757575' }, c.tag),
        h('div', { style: 'flex:1' }),
        h('span', { style: `font:10px ${MONO};color:${c.valColor};white-space:nowrap;overflow:hidden;text-overflow:ellipsis` }, c.val),
        h('div', { style: `position:absolute;left:0;right:0;bottom:0;height:3px;border-radius:0 0 6px 6px;overflow:hidden;background:${c.barTrack}` }, h('div', { style: `height:100%;width:${c.barPct}%;background:${c.barColor}` })),
        c.fx);
    const emptyTile = (c, i) => h('div', { key: 'e' + i, onClick: c.onClick, className: designMode ? 'hv-empty' : '', style: `width:76px;height:76px;border-radius:6px;border:1px dashed ${c.dash};background:${c.bg};display:flex;flex-direction:column;align-items:center;justify-content:center;gap:2px;cursor:${c.cursor}` },
      h('span', { style: `font-size:${c.plusSize}px;color:#965cde;line-height:1` }, c.plus),
      h('span', { style: `font:10px ${MONO};color:#b48ae8` }, c.draft));
    A._zoom2d = Number(zoom2d);
    const grid = h('div', { ref: A.gridRef, style: `position:relative;padding:30px 14px 8px;display:flex;flex-direction:column;gap:48px;width:max-content;zoom:${zoom2d};transform:translate(${(S.px / zoom2d).toFixed(1)}px,${(S.py / zoom2d).toFixed(1)}px)` },
      rows.map((row) => h('div', { key: row.name, style: 'display:flex;align-items:center;gap:8px' },
        h('span', { style: `width:38px;font:11px ${MONO};color:#4d4d4d` }, `row ${row.name}`),
        row.cells.map((c, i) => (c.isRack ? rackTile(c) : emptyTile(c, i))))),
      flows);
    return designMode ? grid : h('div', { style: 'position:absolute;inset:0;overflow:auto;display:flex;align-items:safe center;justify-content:safe center' }, grid);
  }

  // ------------------------------------------------------------------ infra popover
  // a node of the twin's power chain that the floor draws on its own (live)
  function nodePopover(A, M, name) {
    const src = A.src, n = (src.chain || []).find((x) => x.name === name); if (!n) return null;
    const idle = !(n.racks || []).length, under = (src.chain || []).filter((x) => x.from === n.name);
    const fault = src.nodeFault && src.nodeFault(n.path, n.kind === 'ups' ? 'generic/power/ups-failure' : 'generic/power/breaker-trip');
    const P = { name: n.name, kind: `${n.kind === 'pdu-panel' ? 'power panel' : n.kind} · from ${n.from}`, dot: !n.up ? '#ec7575' : idle ? '#d1b854' : '#965cde', bd: !n.up ? '#ec757566' : idle ? '#d1b85466' : 'rgba(255,255,255,0.08)',
      kv: [['state', n.mode === 'dead' ? 'no supply' : n.mode, n.up ? '#cfcfcf' : '#ec7575'], ['load', kw(n.loadW || 0), '#cfcfcf'], ['capacity', n.capacityW ? kw(n.capacityW) : '—', '#cfcfcf'],
        ['racks', (n.racks || []).join(' ') || 'none', idle ? '#d1b854' : '#b3b3b3'], ...(n.kind === 'ups' ? [['battery', `${Math.round(n.charge ?? 0)} %`, '#cfcfcf']] : []), ...(under.length ? [['feeds', under.map((x) => x.name).join(', '), '#b3b3b3']] : [])],
      hasBatt: n.kind === 'ups', batt: Math.round(n.charge ?? 0), battC: '#965cde',
      note: idle && !under.length ? `Serves no rack, so nothing draws through it. In Design, set its serves to the racks, and make those racks draw from feed ${n.feed}.` : idle ? 'Serves no rack itself; it feeds the nodes below it.' : `Serves ${(n.racks || []).join(' ')} on feed ${n.feed}.`,
      actions: [{ label: n.kind === 'ups' ? (fault ? 'Restore UPS' : 'UPS failure') : fault ? 'Close breaker' : 'Trip breaker', danger: !fault, onClick: () => src.facility(`node-${n.name}`) }] };
    return popoverEl(A, P);
  }
  function infraPopover(A, M) {
    const S = A.S, src = A.src, id = S.infraSel;
    if (!id) return null;
    if (id.startsWith('node:')) return nodePopover(A, M, id.slice(5));
    const [type, a] = id.split('-');
    const act = (label, kind, danger) => ({ label, danger, onClick: () => src.facility(kind) });
    const has = (k) => src.has && src.has(k);
    const tot = M.tot;
    let P = null;
    if (type === 'feed') {
      const f = src.feeds[a]; if (!f) return null; const load = src.feedLoad(a, M.aggs);
      P = { name: `feed ${a}`, kind: 'facility power feed', dot: f.up ? '#965cde' : '#ec7575', bd: f.up ? 'rgba(255,255,255,0.08)' : '#ec757566',
        kv: [['state', f.up ? 'up' : 'lost', f.up ? '#cfcfcf' : '#ec7575'], ['load', kw(load), '#cfcfcf'], ['capacity', f.capW ? kw(f.capW) : '—', '#cfcfcf'], ['racks', String(Object.values(src.racks).filter((r) => (r.feeds || []).includes(a)).length || '—'), '#b3b3b3']],
        hasBatt: true, batt: f.capW ? Math.min(100, Math.round((load / f.capW) * 100)) : 0, battC: f.up ? '#965cde' : '#ec7575',
        note: src.kind === 'live' ? `POST /v1/feeds. Every source on this feed loses AC; each rack input's redundancy decides what stays up. ${(src.chain || []).length ? 'Its power chain is on the Facility screen.' : 'The estate models no UPS or switchgear.'}` : 'Losing a feed takes every PSU on it down; dual-corded racks stay up on the other feed.',
        actions: [f.up ? act(`Fail feed ${a}`, `feed-${a}`, true) : act(`Restore feed ${a}`, `feed-${a}`)] };
    } else if (type === 'swgr') {
      const up = src.utility[a].up;
      P = { name: `swgr-${a}`, kind: 'LV switchgear · utility feed ' + a, dot: up ? '#965cde' : '#ec7575', bd: up ? 'rgba(255,255,255,0.08)' : '#ec757566',
        kv: [['utility', up ? 'energised' : 'lost', up ? '#cfcfcf' : '#ec7575'], ['capacity', src.kind === 'live' ? kw(src.feeds[a].capW || 0) : '900 kW', '#cfcfcf'],
          ['downstream', src.kind === 'live' ? `${src.ups[a].name} → ${(src.chain || []).filter((x) => x.from === src.ups[a].name).map((x) => x.name).join(', ') || 'racks'}` : `ups-${a.toLowerCase()} → rpp-a/b/c`, '#b3b3b3']],
        note: up ? `Utility loss puts ${src.ups[a].name || 'ups-' + a} on battery; when it runs out, feed ${a} is lost and its racks fall back to their other feed.` : `Restoring utility returns ${src.ups[a].name || 'ups-' + a} to online.`,
        actions: [has(`generic/power/utility-loss · feed ${a}`) ? act('Restore utility', `utility-${a}`) : act('Utility loss', `utility-${a}`, true)] };
    } else if (type === 'ups') {
      const U = src.ups[a], f = src.feeds[a].up, share = src.feedShare(a);
      P = { name: U.name || `ups-${a}`, kind: 'double-conversion UPS', dot: U.mode === 'online' ? '#965cde' : U.mode === 'battery' ? '#d1b854' : '#ec7575', bd: U.mode === 'online' ? 'rgba(255,255,255,0.08)' : U.mode === 'battery' ? '#d1b85466' : '#ec757566',
        kv: [['mode', U.mode === 'online' ? 'online' : U.mode === 'battery' ? 'on battery' : U.mode === 'depleted' ? 'depleted' : 'fault', U.mode === 'online' ? '#cfcfcf' : U.mode === 'battery' ? '#d1b854' : '#ec7575'], ['battery', U.batt + ' %', U.batt < 30 ? '#d1b854' : '#cfcfcf'], ['output', kw(U.node ? U.node.loadW || 0 : tot * share), '#cfcfcf'], U.node && U.mode === 'battery' ? ['left', `${Math.round((U.node.runtimeLeftS || 0) / 60)} min at this load`, '#d1b854'] : ['feed ' + a, f ? 'up' : 'lost', f ? '#cfcfcf' : '#ec7575']],
        hasBatt: true, batt: U.batt, battC: U.batt < 30 ? '#ec7575' : U.mode === 'battery' ? '#d1b854' : '#965cde',
        note: 'A UPS failure drops feed ' + a + ' immediately (no static bypass in this estate).',
        actions: [has(`generic/power/ups-failure · ups-${a}`) ? act('Restore UPS', `ups-${a}`) : act('UPS failure', `ups-${a}`, true)] };
    } else if (type === 'rpp') {
      const row = a, rk = Object.values(src.racks).filter((r) => r.row === row);
      const w = rk.reduce((acc, r) => acc + M.aggs[r.name].w, 0);
      const st = (n) => (!src.feeds[n].up ? 'no supply' : src.rpp[`${row}-${n}`] ? 'tripped' : 'closed');
      const used = new Set(rk.flatMap((r) => (r.feeds && r.feeds.length ? r.feeds : Object.keys(src.feeds).slice(0, 2))));
      const fs = Object.keys(src.feeds).filter((n) => used.has(n) && (src.kind !== 'live' || src.rppNode[`${row}-${n}`]));
      P = { name: `rpp-${row}`, kind: `remote power panel · row ${row}`, dot: fs.some((n) => src.rpp[`${row}-${n}`]) ? '#ec7575' : '#965cde', bd: 'rgba(255,255,255,0.08)',
        kv: [...fs.map((n) => [`breaker ${n}`, st(n), st(n) === 'closed' ? '#cfcfcf' : '#ec7575']), ['row load', kw(w), '#cfcfcf'], ['racks', rk.map((r) => r.name).join(' '), '#b3b3b3']],
        note: 'Tripping one breaker takes that busway down for the whole row; dual-corded racks stay up on the other feed.',
        actions: fs.map((n) => (has(`generic/power/breaker-trip · rpp-${row} ${n}`) ? act(`Reset breaker ${n}`, `rpp-${row}-${n}`) : act(`Trip breaker ${n}`, `rpp-${row}-${n}`, true))) };
      if (src.kind === 'live') { const pn = (n) => (src.rppNode[`${row}-${n}`] || {}).name; P.name = fs.map(pn).filter(Boolean).join(' · ') || P.name; P.kind = `power panels · row ${row}`; P.note = 'A trip opens the panel\'s main breaker (generic/power/breaker-trip): every rack it serves loses that side; dual-corded racks stay up on the other feed.'; }
    }
    if (!P) return null;
    return popoverEl(A, P);
  }
  function popoverEl(A, P) {
    const S = A.S, left = S.screen !== 'cockpit' ? 12 : S.floorMax ? 12 : 274;
    return h('div', { style: `position:absolute;top:${S.screen === 'cockpit' ? 58 : 12}px;left:${left}px;width:290px;background:rgba(12,12,14,0.82);backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);border:1px solid ${P.bd};border-radius:10px;box-shadow:0 12px 40px rgba(0,0,0,0.55);padding:12px;display:flex;flex-direction:column;gap:9px;z-index:6` },
      h('div', { style: 'display:flex;align-items:center;gap:8px' },
        h('span', { style: `width:7px;height:7px;border-radius:50%;background:${P.dot};box-shadow:0 0 8px ${P.dot}` }),
        h('span', { style: `font:600 13px ${MONO};color:#edeef0` }, P.name),
        h('span', { style: 'font-size:11px;color:#757575;flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis' }, P.kind),
        h('button', { onClick: () => A.set({ infraSel: null }), className: 'hv-text', style: 'background:none;border:0;color:#757575;cursor:pointer;font-size:14px;line-height:1;padding:0 2px' }, '×')),
      h('div', { style: 'display:grid;grid-template-columns:max-content minmax(0,1fr);gap:4px 12px;font-size:11.5px' },
        P.kv.map(([k, v, c]) => [h('span', { style: 'color:#757575' }, k), h('span', { style: `font:11px ${MONO};color:${c};text-align:right` }, v)])),
      P.hasBatt ? h('div', { style: 'height:5px;background:#1f1f1f;border-radius:3px;overflow:hidden' }, h('div', { style: `height:100%;width:${P.batt}%;background:${P.battC};transition:width .8s` })) : null,
      h('div', { style: 'font-size:11px;color:#757575;line-height:1.45' }, P.note),
      h('div', { style: 'display:flex;gap:6px;flex-wrap:wrap' },
        P.actions.map((x) => h('button', { onClick: x.onClick, className: 'hv-bright', style: `background:${x.danger ? '#861a1a44' : '#18191b'};border:1px solid ${x.danger ? '#ec757566' : '#2a2a2a'};border-radius:6px;padding:5px 10px;font-size:11.5px;color:${x.danger ? '#ec7575' : '#cfcfcf'};cursor:pointer` }, x.label))));
  }

  // ------------------------------------------------------------------ panels
  function rackPanel(A, M) {
    const src = A.src, R = M.R, D = M.D; if (!R) return null;
    const ra = M.aggs[R.name], U = 9, cap = src.rackCap(R);
    const slots = R.slots.map((p) => src.devs[p]).filter(Boolean).map((d) => {
      const sel = D && d.path === D.path; const units = Math.max(1, d.units || 1); const hh = units * U + (units > 1 ? units - 1 : 0);
      const isGroup = d.cls === 'power-shelf';
      return h('div', { key: d.path, onClick: () => A.selectDev(d.path), className: 'hv-slot', style: `position:relative;height:${Math.max(hh, isGroup ? 7 : 11)}px;display:flex;align-items:center;gap:6px;padding:0 6px;border-radius:2px;background:${sel ? '#965cde22' : d.health === 'Critical' ? '#861a1a33' : d.health === 'Warning' ? '#423a1a55' : d.cls === 'switch-tray' ? '#131419' : '#18191b'};border-left:2px solid ${SEVN[d.health] ? SEVC[d.health] : d.cls === 'switch-tray' ? '#2f2f2f' : '#2a2a2a'};outline:${sel ? '1px solid #965cde' : 'none'};opacity:${d.power === 'On' ? 1 : 0.45};cursor:pointer;font:10px ${MONO};color:${isGroup ? '#4d4d4d' : '#b3b3b3'};min-width:0` },
        h('span', { style: 'white-space:nowrap;overflow:hidden;text-overflow:ellipsis;flex:1' }, isGroup ? d.name.replace('power-shelf-', 'shelf ') : d.name + (d.power !== 'On' ? ` · ${d.power}` : '')),
        h('span', { style: 'font-size:9px;color:#5cde7b' }, d.fid === 'vm' ? 'vm' : ''),
        h('span', { style: `font-size:9px;color:${!d.bmc ? '#d1b854' : SEVC[d.health]};width:10px;text-align:right` }, !d.bmc ? '⊘' : SEVM[d.health]),
        fxEl(src, d.path));
    });
    const sensors = R.sensors.map((p, i) => {
      const d = src.devs[p]; if (!d) return null; const prev = i ? src.devs[R.sensors[i - 1]] : null; const at = d.at || 0; const pat = prev ? prev.at || 0 : 0;
      return h('div', { key: p, onClick: () => A.selectDev(p), title: d.name, style: `width:10px;height:14px;border-radius:3px;background:${d.leak ? '#ec7575' : d.health !== 'OK' ? '#d1b854' : '#2a2a2a'};cursor:pointer;margin-top:${Math.max(0, (at - pat) * (U + 1) - (i ? 18 : 0))}px` });
    });
    const pct = Math.min(100, (ra.w / cap) * 100).toFixed(0);
    const domLabel = R.f === 'nvl' ? 'NVLink domain' : 'health', dom = R.f === 'nvl' ? ra.dom || '—' : ra.health === 'OK' ? 'nominal' : ra.health;
    const domWarn = R.f === 'nvl' ? ra.dom !== 'Ready' : ra.health !== 'OK';
    return h('section', { style: `position:absolute;top:58px;left:12px;bottom:12px;width:250px;${GLASS};display:${A.S.floorMax ? 'none' : 'flex'};flex-direction:column;overflow:hidden` },
      h('div', { style: 'display:flex;align-items:center;gap:8px;padding:8px 12px;border-bottom:1px solid #1f1f1f' },
        h('span', { style: LABEL }, 'Rack'), h('span', { style: 'font-weight:600;color:#edeef0' }, R.name),
        h('span', { style: `font:11px ${MONO};color:#757575;white-space:nowrap;overflow:hidden;text-overflow:ellipsis`, title: R.title }, R.title)),
      h('div', { style: 'padding:10px 12px;display:flex;flex-direction:column;gap:6px;border-bottom:1px solid #1f1f1f;font-size:11.5px' },
        h('div', { style: 'display:flex;justify-content:space-between;color:#757575' }, h('span', null, 'power'), h('span', { style: `font-family:${MONO};color:#cfcfcf` }, `${kw(ra.w)} / ${kw(cap)}`)),
        h('div', { style: 'height:4px;background:#1f1f1f;border-radius:2px;overflow:hidden' }, h('div', { style: `height:100%;width:${pct}%;background:${ra.w / cap > 0.9 ? '#d1b854' : '#965cde'}` })),
        h('div', { style: 'display:flex;justify-content:space-between;color:#757575' }, h('span', null, R.cooling === 'liquid' ? 'coolant-in (avg)' : 'inlet (avg)'), h('span', { style: `font-family:${MONO};color:#cfcfcf` }, ra.temp ? ra.temp.toFixed(1) + ' °C' : '—')),
        h('div', { style: 'display:flex;justify-content:space-between;color:#757575' }, h('span', null, domLabel), h('span', { style: `color:${domWarn ? '#d1b854' : '#b3b3b3'}` }, dom))),
      h('div', { style: 'flex:1;min-height:0;overflow:auto;padding:10px 12px 12px;display:flex;gap:8px' },
        h('div', { style: 'display:flex;flex-direction:column;gap:4px;padding-top:2px' }, sensors),
        h('div', { style: 'flex:1;display:flex;flex-direction:column;gap:1px;background:#0a0a0a;border:1px solid #2a2a2a;border-radius:6px;padding:4px;min-width:0;height:max-content' }, slots)));
  }

  function devicePanel(A, M) {
    const S = A.S, src = A.src, D = M.D;
    if (!D) return h('section', { style: `flex:1 1 0;min-height:0;${GLASS};padding:12px;color:#757575;font-size:12px` }, 'Select a device.');
    const temp = src.devTemp(D);
    const kv = [
      ['health', `${SEVM[D.health]} ${D.health}`, SEVN[D.health] ? SEVC[D.health] : '#cfcfcf'], ['power', `${D.power} · ${kw(src.devWatts(D))}`, D.power === 'On' ? '#cfcfcf' : '#d1b854'],
      [D.cls === 'compute-tray' || D.cls === 'switch-tray' ? 'coolant-in' : 'inlet', temp == null ? '—' : temp.toFixed(1) + ' °C', temp > 45 ? '#d1b854' : '#cfcfcf'],
      ['BMC', !D.dialect ? '—' : D.bmc ? `reachable${D.bmcSlow ? ' · slow' : ''}${D.ip ? ' · ' + D.ip : ''}` : 'unreachable', D.bmc && !D.bmcSlow ? '#cfcfcf' : '#d1b854'],
      ['DHCP lease', D.dialect && D.ip ? `${D.ip} · ${D.mac}` : D.dialect ? `— · ${D.mac || ''}` : '—', '#cfcfcf'], ['serial', D.serial || '—', '#cfcfcf'], ['firmware', D.fw, D.fw === 'in baseline' ? '#cfcfcf' : '#d1b854'],
      ['leak', D.cls === 'compute-tray' || D.cls === 'sensor' || D.hasLeak ? (D.leak ? 'Leak' : 'OK') : '—', D.leak ? '#ec7575' : '#cfcfcf'],
      ['faults', String(src.activeOn(D)), '#cfcfcf'],
    ];
    const tabs = [['overview', 'Overview'], ['components', 'Components'], ['interfaces', 'Interfaces'], ['fidelity', 'Fidelity']];
    let body;
    if (S.tab === 'components') {
      body = h('div', { style: 'display:flex;flex-direction:column' }, D.comps.map((c) => h('div', { key: c.name, onClick: () => c.kind === 'accelerator' && A.set({ comp: c.name, target: 'device' }), className: 'hv-row', style: `display:grid;grid-template-columns:70px minmax(0,1fr) auto;gap:8px;padding:6px 4px;border-bottom:1px solid #1f1f1f;font-size:12px;cursor:pointer;background:${c.name === S.comp ? '#965cde14' : 'transparent'}` },
        h('span', { style: `font-family:${MONO};color:#cfcfcf` }, c.name),
        h('span', { style: 'color:#757575;white-space:nowrap;overflow:hidden;text-overflow:ellipsis' }, (c.model || '') + (c.state === 'offline' ? ' · offline' : '')),
        h('span', { style: `color:${SEVN[c.health] ? SEVC[c.health] : '#757575'}` }, `${SEVM[c.health] || ''} ${c.health}`))));
    } else if (S.tab === 'interfaces') {
      const rf = src.redfish(D, A);
      const reqs = src.reqsOf(D).slice(-12).reverse();
      body = h('div', { style: 'display:flex;flex-direction:column;gap:10px' },
        h('div', { style: 'font-size:11px;color:#757575' }, 'Redfish as the stack sees it · ', h('span', { style: `font-family:${MONO};color:#b3b3b3` }, D.dialect || 'none')),
        h('pre', { style: `margin:0;padding:10px;background:#0a0a0a;border:1px solid #1f1f1f;border-radius:6px;font:11px/1.5 ${MONO};color:#cfcfcf;white-space:pre-wrap;overflow-wrap:anywhere` }, rf),
        h('div', { style: 'font-size:11px;color:#757575' }, `Requests the stack made · ${src.reqCount(D)}`),
        h('div', { style: `display:flex;flex-direction:column;font:11px ${MONO}` }, reqs.map((q, i) => h('div', { key: i, style: 'display:grid;grid-template-columns:58px 40px minmax(0,1fr) 34px;gap:6px;padding:4px 0;border-bottom:1px solid #1f1f1f' },
          h('span', { style: 'color:#4d4d4d' }, q.t), h('span', { style: 'color:#5b9dff' }, q.m), h('span', { style: 'color:#cfcfcf;overflow-wrap:anywhere' }, q.p), h('span', { style: `color:${q.s === '200' || q.s === '204' ? '#757575' : '#d1b854'};text-align:right` }, q.s)))));
    } else if (S.tab === 'fidelity') {
      body = h('div', { style: 'display:flex;flex-direction:column;gap:8px;font-size:12px;color:#b3b3b3' },
        h('div', { style: 'color:#757575;font-size:11px' }, 'This profile is not'),
        (D.isNot || []).map((n, i) => h('div', { key: i, style: 'padding:8px 10px;background:#18191b;border-radius:6px;line-height:1.5' }, n)));
    } else {
      body = h('div', { style: 'display:grid;grid-template-columns:max-content minmax(0,1fr);gap:5px 14px;font-size:12px' },
        kv.map(([k, v, c]) => [h('span', { style: 'color:#757575' }, k), h('span', { style: `font-family:${MONO};font-size:11.5px;color:${c};overflow-wrap:anywhere` }, v)]));
    }
    const fidColor = D.fid === 'vm' ? '#5cde7b' : '#757575';
    return h('section', { style: `flex:1 1 0;min-height:0;${GLASS};display:flex;flex-direction:column;overflow:hidden` },
      h('div', { style: 'padding:10px 12px 0;display:flex;flex-direction:column;gap:4px' },
        h('div', { style: 'display:flex;align-items:center;gap:8px' },
          h('span', { style: LABEL }, 'Device'),
          h('span', { style: `font:10px ${MONO};color:${fidColor};border:1px solid ${fidColor};border-radius:3px;padding:0 5px` }, D.fid),
          h('div', { style: 'flex:1' }),
          h('span', { style: `font-size:12px;color:${SEVN[D.health] ? SEVC[D.health] : '#757575'}` }, `${SEVM[D.health]} ${D.health}`)),
        h('div', { style: `font:600 13px ${MONO};color:#edeef0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis`, title: D.path }, D.short || D.path),
        h('div', { style: 'font-size:11.5px;color:#757575' }, `${D.profile} · ${D.cls}`)),
      h('div', { style: 'display:flex;gap:2px;padding:0 8px;border-bottom:1px solid #1f1f1f;margin-top:8px' },
        tabs.map(([v, l]) => h('button', { key: v, onClick: () => A.set({ tab: v }), style: `background:none;border:0;border-bottom:2px solid ${S.tab === v ? '#965cde' : 'transparent'};padding:6px 8px;font-size:12px;color:${S.tab === v ? '#edeef0' : '#757575'};cursor:pointer` }, l))),
      h('div', { style: 'flex:1;min-height:0;overflow:auto;padding:10px 12px' }, body));
  }

  function breakPanel(A, M) {
    const S = A.S, src = A.src, R = M.R, D = M.D;
    const rackMode = S.target === 'rack';
    const list = rackMode ? (R ? src.rackFaults(R) : []) : D ? src.faultsFor(D) : [];
    const sel = list.find((f) => f.id === S.faultId) || list[0];
    const chips = sel && sel.comps && sel.comps.length ? sel.comps : null;
    const comp = chips ? (chips.includes(S.comp) ? S.comp : chips[0]) : null;
    const target = rackMode ? (sel && sel.targetPath ? sel.targetPath : R ? `${R.name} (rack)` : '—') : sel && chips ? `${D.short || D.path}/${comp}` : D ? D.short || D.path : '—';
    const msg = S.msg || (list.length ? '' : rackMode ? 'No rack-level fault targets this rack.' : 'No catalogue fault targets this device class.');
    const inject = () => {
      if (!sel) return;
      const tgt = rackMode ? (sel.targetDev || R.name) : D.path;
      const m = src.inject(sel.id, tgt, chips ? comp : null, undefined, S.when, sel.paramValues ? sel.paramValues(S.params) : undefined);
      if (m && m.then) m.then((x) => x && A.flash(x)); else if (m) A.flash(m);
    };
    const gpusColor = (c) => { const x = D && D.comps.find((y) => y.name === c); return x && SEVN[x.health] ? SEVC[x.health] : '#757575'; };
    const acts = [['off', 'Power off at PDU'], ['on', 'Power on'], ['ac', 'AC cycle'], ['reseat', 'Reseat'], ['bmc', 'BMC reset']];
    const active = src.active.slice().reverse();
    return h('section', { style: `flex:1.25 1 0;min-height:0;${GLASS};display:flex;flex-direction:column;overflow:hidden` },
      h('div', { style: 'display:flex;align-items:center;gap:8px;padding:8px 12px;border-bottom:1px solid #1f1f1f' },
        h('span', { style: LABEL }, 'Break it'), h('div', { style: 'flex:1' }),
        segBox(seg([['device', 'device'], ['rack', 'rack']], S.target, (v) => A.set({ target: v, faultId: null }), 'padding:2px 8px;font-size:11px;cursor:pointer'))),
      h('div', { style: 'flex:1;min-height:0;overflow:auto;padding:10px 12px;display:flex;flex-direction:column;gap:10px' },
        h('div', { style: 'display:flex;align-items:center;gap:8px;font-size:11.5px' },
          h('span', { style: 'color:#757575;width:44px' }, 'target'),
          h('span', { style: `font:11.5px ${MONO};color:#edeef0;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap`, title: target }, target)),
        chips ? h('div', { style: 'display:flex;align-items:center;gap:4px;flex-wrap:wrap;padding-left:52px' },
          chips.map((c) => h('button', { key: c, onClick: () => A.set({ comp: c }), style: `background:${c === comp ? '#965cde26' : 'transparent'};color:${c === comp ? '#edeef0' : rackMode ? '#757575' : gpusColor(c)};border:1px solid ${c === comp ? '#965cde' : '#2a2a2a'};border-radius:4px;padding:1px 6px;font:10.5px ${MONO};cursor:pointer` }, c))) : null,
        list.length ? h('div', { style: 'display:flex;flex-direction:column;border:1px solid #1f1f1f;border-radius:6px;overflow:hidden' },
          list.map((f) => h('div', { key: f.id, onClick: () => A.set({ faultId: f.id, params: {} }), className: 'hv-row', style: `display:flex;flex-direction:column;gap:1px;padding:6px 9px;background:${f === sel ? '#965cde14' : 'transparent'};border-left:2px solid ${f === sel ? '#965cde' : 'transparent'};cursor:pointer;border-bottom:1px solid #1a1a1a` },
            h('span', { style: `font-size:12px;color:${f === sel ? '#edeef0' : '#b3b3b3'}` }, f.title),
            h('span', { style: `font:10px ${MONO};color:#757575` }, f.id)))) : null,
        sel ? h('div', { style: 'display:flex;flex-direction:column;gap:5px;padding:9px 10px;background:#0a0a0a;border:1px solid #1f1f1f;border-radius:6px' },
          h('span', { style: SUB }, sel.effects ? 'Effects preview' : 'Fault'),
          (sel.effects || sel.notes || []).map((e, i) => h('span', { key: i, style: `font:11px ${MONO};color:#cfcfcf` }, `· ${e}`)),
          sel.params ? Object.entries(sel.params).map(([k, p]) => h('div', { key: k, style: 'display:flex;align-items:center;gap:6px;font-size:11px' },
            h('span', { style: `font:11px ${MONO};color:#b3b3b3;min-width:76px` }, k),
            h('input', { className: 'tc-input', style: 'width:90px', type: p.type === 'int' || p.type === 'number' ? 'number' : 'text', value: (S.params || {})[k] ?? p.default ?? '', onInput: (e) => { A.S.params = { ...(A.S.params || {}), [k]: e.target.value }; } }),
            h('span', { style: 'color:#4d4d4d' }, p.range ? `${p.range[0]}–${p.range[1]}` : p.type || ''))) : null,
          h('span', { style: 'font-size:11px;color:#757575;margin-top:2px' }, `clears ${sel.clears}`)) : null,
        h('div', { style: 'display:flex;align-items:center;gap:8px;font-size:11.5px;flex-wrap:wrap' },
          h('span', { style: 'color:#757575;width:44px' }, 'when'),
          segBox(seg([['now', 'now'], ['10', 'in 10 s'], ['30', 'in 30 s']], S.when, (v) => A.set({ when: v }), 'padding:3px 9px;font-size:11px;cursor:pointer'))),
        h('div', { style: 'display:flex;align-items:center;gap:8px;justify-content:flex-end' },
          h('span', { style: `font-size:11px;color:${S.msg ? '#b48ae8' : '#757575'};flex:1` }, msg),
          primary('Inject fault', inject, !sel)),
        h('div', { style: 'height:1px;background:#1f1f1f' }),
        h('span', { style: SUB }, 'Technician acts'),
        h('div', { style: 'display:flex;flex-wrap:wrap;gap:6px' },
          acts.map(([k, l]) => h('button', { key: k, onClick: () => D && src.act(k, D.path), className: 'hv-act', style: 'background:#18191b;border:1px solid #2a2a2a;border-radius:6px;padding:4px 9px;font-size:11.5px;color:#cfcfcf;cursor:pointer' }, l))),
        h('div', { style: 'height:1px;background:#1f1f1f' }),
        h('div', { style: 'display:flex;align-items:center;gap:8px' },
          h('span', { style: SUB }, 'Active faults'),
          h('span', { style: `font:11px ${MONO};color:${src.active.length ? '#ec7575' : '#4d4d4d'}` }, String(src.active.length))),
        active.map((a) => h('div', { key: a.key, style: 'display:flex;align-items:center;gap:8px;padding:6px 8px;background:#861a1a22;border:1px solid #861a1a66;border-radius:6px' },
          h('span', { style: 'color:#ec7575;font-size:10px' }, '▲'),
          h('div', { style: 'display:flex;flex-direction:column;min-width:0;flex:1' },
            h('span', { style: 'font-size:11.5px;color:#edeef0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis' }, a.title),
            h('span', { style: `font:10px ${MONO};color:#b3b3b3;white-space:nowrap;overflow:hidden;text-overflow:ellipsis`, title: a.target }, `${a.target} · ${a.by} · ${a.t}`)),
          h('button', { onClick: () => src.clear(a), className: 'hv-danger', style: 'background:none;border:1px solid #3a3a3a;border-radius:5px;color:#cfcfcf;font-size:11px;padding:2px 7px;cursor:pointer' }, 'Clear'))),
        active.length ? null : h('span', { style: 'font-size:11.5px;color:#4d4d4d' }, 'None. The estate is nominal.'),
        h('div', { style: 'display:flex;align-items:center;gap:10px;padding:8px 10px;background:#18191b;border-radius:6px;margin-top:2px' },
          h('button', { onClick: () => src.setChaos(!src.chaos.on, src.chaos.rate), title: src.kind === 'live' ? 'POST /v1/chaos — the API does not report chaos state; this shows what was last set here' : '', style: `width:30px;height:16px;border-radius:999px;border:0;background:${src.chaos.on ? '#965cde' : '#2a2a2a'};position:relative;cursor:pointer;flex:none` },
            h('span', { style: `position:absolute;top:2px;left:${src.chaos.on ? 16 : 2}px;width:12px;height:12px;border-radius:50%;background:#fff;transition:left .15s` })),
          h('div', { style: 'display:flex;flex-direction:column;flex:1' },
            h('span', { style: 'font-size:12px;color:#edeef0' }, 'Chaos'),
            h('span', { style: 'font-size:10.5px;color:#757575' }, `random faults from the seed · ${src.chaos.rate}${src.caps.chaosUnit}`)),
          h('button', { onClick: () => src.setChaos(src.chaos.on, Math.max(1, src.chaos.rate - 1)), style: 'background:none;border:1px solid #2a2a2a;border-radius:4px;color:#b3b3b3;width:22px;height:22px;cursor:pointer' }, '−'),
          h('button', { onClick: () => src.setChaos(src.chaos.on, Math.min(10, src.chaos.rate + 1)), style: 'background:none;border:1px solid #2a2a2a;border-radius:4px;color:#b3b3b3;width:22px;height:22px;cursor:pointer' }, '+'))));
  }

  function eventsPanel(A, M) {
    const S = A.S, src = A.src, R = M.R, D = M.D;
    const scope = S.evScope === 'rack' && R ? R.path : S.evScope === 'device' && D ? D.path : null;
    const evs = src.events.filter((e) => S.kinds[e.kind] !== 0 && (!scope || e.full === scope || (e.full || e.path).startsWith(scope)));
    const now = Date.now();
    const kinds = Object.keys(S.kinds);
    for (const e of src.events) if (!(e.kind in S.kinds) && !kinds.includes(e.kind)) kinds.push(e.kind);
    return h('section', { style: `position:absolute;left:274px;right:374px;bottom:12px;height:210px;min-width:540px;${GLASS};display:${S.floorMax ? 'none' : 'flex'};flex-direction:column;overflow:hidden` },
      h('div', { style: 'display:flex;align-items:center;gap:8px;padding:6px 10px 6px 12px;border-bottom:1px solid #1f1f1f;flex-wrap:wrap' },
        h('span', { style: LABEL }, 'Events'),
        h('span', { style: `font:11px ${MONO};color:#4d4d4d` }, String(evs.length)),
        h('div', { style: 'display:flex;gap:3px;flex-wrap:wrap' }, kinds.map((k) => { const on = S.kinds[k] !== 0; const [c] = KIND_C[k] || KIND_C.twin; return h('button', { key: k, onClick: () => A.setKind(k, on ? 0 : 1), style: `background:${on ? '#18191b' : 'transparent'};color:${on ? c : '#4d4d4d'};border:1px solid ${on ? '#2a2a2a' : '#1a1a1a'};border-radius:4px;padding:0 6px;font:10.5px/18px ${MONO};cursor:pointer` }, k); })),
        h('div', { style: 'flex:1' }),
        segBox(seg([['all', 'estate'], ['rack', R ? R.name : 'rack'], ['device', D ? D.name : 'device']], S.evScope, (v) => A.set({ evScope: v }), `padding:2px 8px;font:10.5px ${MONO};cursor:pointer`)),
        h('button', { onClick: () => A.set({ follow: !S.follow }), style: `background:none;border:1px solid #2a2a2a;border-radius:6px;color:${S.follow ? '#b3b3b3' : '#d4a853'};font-size:11px;padding:2px 8px;cursor:pointer` }, S.follow ? '⏸ follow' : '▶ follow')),
      h('div', { ref: A.evRef, style: `flex:1;min-height:0;overflow:auto;font:11.5px ${MONO}` },
        evs.slice(-160).map((e) => { const [kc, kbg] = KIND_C[e.kind] || KIND_C.twin;
          return h('div', { key: e.seq, onClick: () => A.gotoEvent(e), className: 'hv-ev', title: e.title || '', style: `display:grid;grid-template-columns:58px 62px minmax(70px,150px) minmax(220px,1fr) 84px;gap:8px;padding:3px 12px;border-bottom:1px solid #161616;align-items:baseline;cursor:pointer;animation:${now - e.at < 1500 ? 'dcFlash 1.5s ease-out' : 'none'}` },
            h('span', { style: 'color:#4d4d4d' }, e.t),
            h('span', null, h('span', { style: `font-size:10px;padding:0 5px;border-radius:3px;background:${kbg};color:${kc}` }, e.kind)),
            h('span', { style: 'color:#757575;white-space:nowrap;overflow:hidden;text-overflow:ellipsis', title: e.path }, e.path),
            h('span', { style: `color:${e.sev === 'crit' ? '#ec7575' : e.sev === 'warn' ? '#d1b854' : e.kind === 'read' ? '#757575' : '#cfcfcf'};overflow-wrap:anywhere;padding-left:${e.depth * 16}px` }, h('span', { style: 'color:#4d4d4d' }, e.depth ? '↳ ' : ''), e.msg),
            h('span', { style: 'color:#4d4d4d;text-align:right;white-space:nowrap;overflow:hidden;text-overflow:ellipsis', title: e.src }, e.src)); })));
  }

  // ------------------------------------------------------------------ the cockpit
  function cockpit(A, M) {
    const S = A.S, src = A.src;
    const view = S.view;
    const rows = floorRows(A, M);
    const iso = view === 'iso' ? iso3d(A, M, rows) : null;
    const rg = !S.floorMax ? { t: 56, l: 270, r: 370, b: 228 } : { t: 56, l: 12, r: 12, b: 24 };
    const legend = { health: 'grey = nominal · ◆ warning · ▲ critical · ⊘ BMC unreachable', power: 'height / bar = load vs rack capacity', temp: 'tint = coolant-in (liquid) / inlet (air)', domains: `dashed = NVLink domain · ≋ cooling loop ${(src.cdus || []).filter((c) => c.racks.length).map((c) => c.name).join(', ')}` }[S.overlay];
    const LCOL = { power: '#965cde', network: '#5b9dff', cooling: '#d4a853' };
    const camPresets = [['iso', 'ISO', 58, -32], ['aisle', 'AISLE', 72, 0], ['end', 'END', 66, -90], ['plan', 'PLAN', 0, 0]];
    return h('div', { style: 'min-height:0;min-width:0;overflow:auto' },
      h('div', { style: 'position:relative;height:100%;min-height:700px;min-width:1300px;overflow:hidden;background:radial-gradient(ellipse 70% 60% at 50% 45%,#16131d 0%,#0b0b0d 55%,#080808 100%)' },
        h('div', { onMouseDown: A.orbitStart, onWheel: A.onWheel, onContextMenu: A.noMenu, onDblClick: A.focus, style: `position:absolute;inset:0;cursor:${S.dragging === 'pan' ? 'move' : view === 'iso' ? (S.dragging ? 'grabbing' : 'grab') : 'move'}` },
          h('div', { ref: A.floorRef, style: `position:absolute;top:${rg.t}px;left:${rg.l}px;right:${rg.r}px;bottom:${rg.b}px;transition:all .3s` },
            view === 'iso' ? isoScene(A, iso, false) : floor2d(A, M, rows, false))),
        h('div', { style: 'position:absolute;top:12px;left:12px;right:12px;display:flex;align-items:flex-start;justify-content:space-between;gap:10px;pointer-events:none' },
          h('div', { style: `display:flex;align-items:center;gap:16px;flex:0 1 auto;min-width:0;overflow:hidden;white-space:nowrap;padding:8px 14px;${GLASS};font:11px ${MONO};color:#757575;pointer-events:none` },
            h('span', { style: 'display:flex;align-items:center;gap:8px' }, h('span', { style: 'width:6px;height:6px;border-radius:50%;background:#965cde;box-shadow:0 0 8px #965cde' }), h('span', { style: 'letter-spacing:.16em;color:#b3b3b3' }, src.estate.hall)),
            h('span', null, 'IT load ', h('b', { style: 'color:#edeef0;font-weight:500' }, kw(M.tot))),
            h('span', { style: `color:${src.active.length ? '#ec7575' : '#4d4d4d'}` }, `${src.active.length} faults`)),
          h('div', { style: 'display:flex;gap:8px;align-items:center;flex:none;pointer-events:auto' },
            h('div', { style: `display:flex;align-items:center;gap:8px;padding:6px 8px;${GLASS}` },
              segBox(seg([['health'], ['power'], ['temp', 'inlet °C'], ['domains']], S.overlay, (v) => A.set({ overlay: v }), 'padding:3px 9px;font-size:11.5px;cursor:pointer')),
              h('div', { style: 'width:1px;height:18px;background:#2a2a2a' }),
              layerChips(A, LCOL, true),
              animToggle(A, '2px 7px'),
              h('div', { style: 'width:1px;height:18px;background:#2a2a2a' }),
              segBox(camPresets.map(([k, l, rx, rz]) => { const on = view === 'iso' && Math.abs(S.rx - rx) < 1 && Math.abs(S.rz - rz) < 1; return h('button', { key: k, onClick: () => A.camTo({ rx, rz, zoom: 1, tx: 0, ty: 0, view: 'iso' }), style: `background:${on ? '#965cde26' : 'transparent'};color:${on ? '#edeef0' : '#757575'};border:0;padding:3px 9px;font:11px ${MONO};cursor:pointer` }, l); }))),
            h('div', { style: `display:flex;overflow:hidden;${GLASS}` }, seg([['2d', '2D'], ['iso', 'ISO']], view, (v) => A.set({ view: v }), `padding:7px 10px;font:11px ${MONO};cursor:pointer`)),
            h('button', { onClick: () => A.set({ floorMax: !S.floorMax }), title: S.floorMax ? 'restore cockpit panels' : 'floor full screen', className: 'hv-text', style: `${GLASS};color:#b3b3b3;font-size:11px;padding:7px 10px;cursor:pointer` }, S.floorMax ? '⤡ Restore' : '⤢ Expand'))),
        h('div', { style: `position:absolute;left:12px;bottom:12px;font:10px ${MONO};color:#4d4d4d;display:${S.floorMax || view !== 'iso' ? 'block' : 'none'};pointer-events:none` }, `${view === 'iso' ? 'drag to orbit · shift-, right- or two-finger drag to pan · scroll or pinch to zoom · double-click a point to orbit around it · ISO / AISLE / END / PLAN reset' : 'drag to pan'} · click ${src.caps.infra ? 'switchgear, UPS or RPP' : 'a feed'} to fail power · ${legend}`),
        rackPanel(A, M),
        h('div', { style: `position:absolute;top:58px;right:12px;bottom:12px;width:350px;display:${S.floorMax ? 'none' : 'flex'};flex-direction:column;gap:8px` }, devicePanel(A, M), breakPanel(A, M)),
        infraPopover(A, M),
        eventsPanel(A, M)));
  }
  function layerChips(A, LCOL, glow) {
    const S = A.S;
    const items = [['power', LCOL.power, 'power'], ...A.src.fabrics().map((f) => ['net:' + f.name, f.color, f.short, `network ${f.name}: ${f.racks.size} racks${f.hub ? ', switched in ' + f.hub : ''}`]), ['cooling', LCOL.cooling, 'cooling']];
    return items.map(([k, color, label, title]) => { const on = S.layers[k] !== 0; const sw = on ? color : '#3a3a3a';
      return h('button', { key: k, title: title || label, onClick: () => A.set({ layers: { ...S.layers, [k]: on ? 0 : 1 } }), style: `display:flex;align-items:center;gap:5px;background:${on ? '#18191b' : 'transparent'};color:${on ? '#edeef0' : '#757575'};border:1px solid ${on ? '#2f2f2f' : '#1f1f1f'};border-radius:4px;padding:${glow ? '2px' : '1px'} 7px;font-size:11px;cursor:pointer` },
        h('span', { style: `width:${glow ? 10 : 8}px;height:2px;border-radius:1px;background:${sw};${glow ? `box-shadow:0 0 6px ${sw}` : ''}` }), label); });
  }
  function animToggle(A, padding) {
    const S = A.S;
    return h('button', { onClick: () => A.set({ animate: !S.animate }), style: `background:${S.animate ? '#965cde26' : 'transparent'};color:${S.animate ? '#edeef0' : '#757575'};border:1px solid ${S.animate ? '#965cde88' : '#2a2a2a'};border-radius:4px;padding:${padding};font-size:11px;cursor:pointer` }, S.animate ? '❚❚ flows' : '▶ flows');
  }

  Object.assign(TC.views, { camTransform, h, seg, segBox, primary, secondary, model, topBar, cockpit, floorRows, iso3d, isoScene, floor2d, infraPopover, layerChips, animToggle, GLASS, LABEL, SUB, MONO });
})();
