// shell.js — the designer (Design mode on a live twin): the Explorer and the Library on the left,
// the views (floor, rack, fabrics, power and cooling) in the middle, the Inspector and the draft
// on the right. Areas register their views in TC.dz.views and their inspectors in
// TC.dz.inspectors; this file lays them out and runs the draft panel.
window.TC = window.TC || {};
(() => {
  const { h } = TC.h;
  const F = TC.forms, Z = TC.dz, { Q } = Z;
  const MONO = F.MONO;
  const CARD = 'background:#101010;border:1px solid #1f1f1f;border-radius:8px';
  Z.views = Z.views || {}; Z.inspectors = Z.inspectors || {};

  function ctxOf(A) {
    const D = A.draft, src = A.src;
    const tree = D.tree(), base = D.base();
    return { A, D, src, tree, base, layout: D.layout(), plan: D.plan, catalogue: src.catalogueFull || {}, host: src.host,
      racks: Q.racks(tree).map((x) => x.rack.name).sort((a, b) => a.localeCompare(b, undefined, { numeric: true })),
      sel: A.S.dz.sel, select: (sel) => A.set({ dz: { ...A.S.dz, sel } }), push: (...ops) => { D.push(...ops); A.render(); } };
  }
  const changed = (a, b) => JSON.stringify(a) !== JSON.stringify(b);

  // ---------------------------------------------------------------- left: explorer and library
  function item(c, label, sel, opts = {}) {
    const on = c.sel && JSON.stringify(c.sel) === JSON.stringify(sel);
    return h('div', { key: JSON.stringify(sel), onClick: () => c.select(sel), className: 'hv-row',
      style: `display:flex;align-items:center;gap:6px;padding:3px 8px 3px ${8 + (opts.depth || 0) * 14}px;border-radius:5px;cursor:pointer;background:${on ? '#965cde22' : 'transparent'};color:${on ? '#edeef0' : opts.dim ? '#757575' : '#cfcfcf'};font-size:12px;white-space:nowrap;overflow:hidden` },
      opts.icon ? h('span', { style: `color:${opts.iconColor || '#4d4d4d'};font-size:10px;width:10px` }, opts.icon) : null,
      h('span', { style: `overflow:hidden;text-overflow:ellipsis;${opts.mono ? `font-family:${MONO};font-size:11.5px` : ''}` }, label),
      opts.meta ? h('span', { style: `margin-left:auto;font:10.5px ${MONO};color:${opts.warn ? '#d1b854' : '#4d4d4d'};flex:none` }, opts.meta) : null,
      opts.dirty ? h('span', { title: 'changed in this draft', style: 'width:6px;height:6px;border-radius:50%;background:#b48ae8;flex:none;margin-left:4px' }) : null);
  }
  function group(c, title, kids, add) {
    return h('div', { key: 'g' + title, style: 'display:flex;flex-direction:column;gap:1px;margin-top:8px' },
      h('div', { style: 'display:flex;align-items:center;padding:0 8px;height:20px' }, h('span', { style: 'font-size:10.5px;color:#757575;text-transform:uppercase;letter-spacing:.06em' }, title), h('div', { style: 'flex:1' }),
        add ? h('button', { onClick: add, title: `add ${title.toLowerCase().replace(/s$/, '')}`, className: 'hv-text', style: 'background:none;border:0;color:#965cde;cursor:pointer;font-size:14px;line-height:1;padding:0 2px' }, '+') : null),
      kids);
  }
  function explorer(c) {
    const t = c.tree, b = c.base, ex = c.A.S.dz.expanded || {};
    const toggle = (k) => c.A.set({ dz: { ...c.A.S.dz, expanded: { ...ex, [k]: !ex[k] } } });
    const halls = Q.halls(t).map((hl) => {
      const bh = Q.halls(b).find((x) => x.name === hl.name);
      return [item(c, hl.name, { kind: 'hall', hall: hl.name }, { icon: '▦', iconColor: '#965cde', dirty: changed(hl, bh), meta: `${(hl.rows || []).length} rows` }),
        (hl.rows || []).map((r) => {
          const br = bh && (bh.rows || []).find((x) => x.name === r.name), k = hl.name + '/' + r.name;
          return [h('div', { key: 'r' + k, style: 'display:flex;align-items:center' },
            h('button', { onClick: () => toggle(k), style: 'background:none;border:0;color:#4d4d4d;cursor:pointer;width:16px;padding:0 0 0 14px;font-size:9px' }, ex[k] ? '▾' : '▸'),
            h('div', { style: 'flex:1;min-width:0' }, item(c, `row ${r.name}`, { kind: 'row', hall: hl.name, row: r.name }, { dirty: changed(r, br), meta: `${(r.racks || []).length}/${r.positions}` }))),
          ex[k] ? (r.racks || []).slice().sort((x, y) => x.position - y.position).map((k2) => item(c, k2.name, { kind: 'rack', name: k2.name }, { depth: 2, mono: true, meta: (k2.format || '').split('/').pop(), dirty: changed(k2, br && (br.racks || []).find((x) => x.name === k2.name)) })) : null];
        })];
    });
    const fabrics = Q.fabrics(t).map((f) => item(c, f.name, { kind: 'fabric', name: f.name }, { mono: true, icon: '━', iconColor: (TC.data.fabricStyle(f.name, 0) || {}).color, meta: (f.template || '').split('/').pop(), dirty: changed(f, Q.fabrics(b).find((x) => x.name === f.name)) }));
    const nets = Object.entries(Q.networks(t)).sort().map(([n, v]) => item(c, n, { kind: 'network', name: n }, { mono: true, meta: v.role, dirty: changed(v, Q.networks(b)[n]) }));
    const feeds = Q.feeds(t).map((f) => item(c, `feed ${f.name}`, { kind: 'feed', name: f.name }, { icon: '⚡', iconColor: '#965cde', meta: f.capacityW ? `${Math.round(f.capacityW / 1000)} kW` : '', dirty: changed(f, Q.feeds(b).find((x) => x.name === f.name)) }));
    const idleNode = (n) => !(n.serves || []).length && !Q.topology(t).some((x) => x.from === n.name);
    const nodes = Q.topology(t).map((n) => item(c, n.name, { kind: 'node', name: n.name }, { depth: 1, mono: true, meta: idleNode(n) ? `${n.kind} · no racks` : n.kind, warn: idleNode(n), dirty: changed(n, Q.topology(b).find((x) => x.name === n.name)) }));
    const loops = Q.loops(t).map((l) => item(c, l.name, { kind: 'loop', name: l.name }, { icon: '≋', iconColor: '#5b9dff', mono: true, meta: (l.racks || []).length ? `${l.racks.length} racks` : 'no racks', warn: !(l.racks || []).length, dirty: changed(l, Q.loops(b).find((x) => x.name === l.name)) }));
    const fdevs = Q.fdevs(t).map((d) => item(c, d.name, { kind: 'fdev', name: d.name }, { mono: true, meta: (d.profile || '').split('/').pop() }));
    const ovs = Q.overrides(t).map((o, i) => item(c, `${o.select} → ${o.level}`, { kind: 'override', i }, { mono: true }));
    const cab = Q.cabling(t), ncab = ((cab.add || []).length + (cab.remove || []).length);
    const add = {
      fabric: () => c.select({ kind: 'new', what: 'fabric' }), network: () => c.select({ kind: 'new', what: 'network' }), feed: () => c.select({ kind: 'new', what: 'feed' }),
      node: () => c.select({ kind: 'new', what: 'node' }), loop: () => c.select({ kind: 'new', what: 'loop' }), override: () => c.select({ kind: 'new', what: 'override' }), hall: () => c.select({ kind: 'new', what: 'hall' }),
    };
    return h('div', { style: 'flex:1;min-height:0;overflow:auto;padding:4px 4px 12px' },
      item(c, `${(t.metadata || {}).name || 'estate'} · settings`, { kind: 'settings' }, { icon: '⚙', meta: Q.spec(t).clock || 'wall', dirty: changed({ ...Q.spec(t), sites: 0, fabrics: 0, networks: 0, facility: 0, fidelity: 0, cabling: 0, stacks: 0 }, { ...Q.spec(b), sites: 0, fabrics: 0, networks: 0, facility: 0, fidelity: 0, cabling: 0, stacks: 0 }) }),
      group(c, 'Halls', halls, add.hall),
      group(c, 'Fabrics', fabrics, add.fabric),
      group(c, 'Networks', nets, add.network),
      group(c, 'Power', [feeds, nodes.length ? h('div', { key: 'tl', style: 'font-size:10px;color:#4d4d4d;padding:2px 22px' }, 'power chain') : null, nodes], add.feed),
      h('div', { key: 'addnode', style: 'padding:0 8px' }, h('button', { onClick: add.node, style: 'background:none;border:0;color:#965cde;cursor:pointer;font-size:11px;padding:2px 0' }, '+ UPS, busway or panel')),
      group(c, 'Cooling', loops, add.loop),
      fdevs.length ? group(c, 'Facility devices', fdevs) : null,
      group(c, 'BMS', [item(c, (Q.facility(t).bms || {}).dialect || 'none', { kind: 'bms' }, { mono: true })]),
      group(c, 'Fidelity overrides', ovs.length ? ovs : [h('div', { key: 'nov', style: 'font-size:11px;color:#4d4d4d;padding:2px 8px' }, `every device at ${((Q.spec(t).fidelity || {}).default) || 'model'}`)], add.override),
      group(c, 'Cabling', [item(c, ncab ? `${ncab} hand-cabled changes` : 'generated only', { kind: 'cabling' }, {})]),
      group(c, 'Stacks', [item(c, Q.stacks(t).map((s) => s.name).join(', ') || 'none', { kind: 'stacks' }, { mono: true })]));
  }

  function library(c) {
    const dz = c.A.S.dz, q = (dz.libQuery || '').toLowerCase(), kind = dz.libKind || 'formats', cat = c.catalogue;
    const set = (o) => c.A.set({ dz: { ...c.A.S.dz, ...o } });
    const list = (cat[kind] || []).filter((x) => !q || JSON.stringify([x.name, x.class, x.archetype, x.generator, x.manufacturer, x.model]).toLowerCase().includes(q));
    const sub = (x) => kind === 'formats' ? `${x.archetype} · ${x.layout && x.layout.height}U · ${Math.round(((x.power || {}).capacityW || 0) / 1000)} kW${((x.cooling || {}).kind) ? ' · ' + x.cooling.kind : ''}`
      : kind === 'profiles' ? `${x.class} · ${x.form && x.form.units ? x.form.units + 'U' : x.form && x.form.mounting} · ${Math.round((x.power || {}).maxW || 0)} W${x.manufacturer ? ' · ' + x.manufacturer : ''}`
      : `${x.generator}${x.params ? ' · ' + Object.keys(x.params).join(', ') : ''}`;
    return h('div', { style: 'flex:1;min-height:0;display:flex;flex-direction:column' },
      h('div', { style: 'display:flex;gap:6px;padding:8px' },
        h('input', { className: 'dz-in', style: F.INPUT, placeholder: 'search the packs', value: dz.libQuery || '', onInput: (e) => { c.A.S.dz.libQuery = e.target.value; c.A.render(); } })),
      h('div', { style: 'display:flex;gap:2px;padding:0 8px 6px' }, [['formats', 'Rack formats'], ['profiles', 'Devices'], ['templates', 'Fabrics']].map(([k, l]) => h('button', { key: k, onClick: () => set({ libKind: k }),
        style: `background:${kind === k ? '#965cde26' : 'transparent'};color:${kind === k ? '#edeef0' : '#757575'};border:1px solid ${kind === k ? '#965cde66' : '#2a2a2a'};border-radius:5px;padding:2px 8px;font-size:11px;cursor:pointer` }, l))),
      kind === 'formats' ? h('div', { style: 'font-size:10.5px;color:#4d4d4d;padding:0 10px 6px' }, dz.armed ? `click an empty position to add ${dz.armed}, or drag a format onto one` : 'drag a format onto an empty position, or arm it and click one') : null,
      h('div', { style: 'flex:1;min-height:0;overflow:auto;padding:0 6px 10px;display:flex;flex-direction:column;gap:4px' },
        list.map((x) => { const armed = dz.armed === x.name, on = c.sel && c.sel.kind === 'lib' && c.sel.name === x.name;
          return h('div', { key: x.name, draggable: kind === 'formats' ? 'true' : null, onDragStart: (e) => { e.dataTransfer.setData('text/tc-format', x.name); e.dataTransfer.effectAllowed = 'copy'; },
            onClick: () => c.select({ kind: 'lib', lib: kind, name: x.name }),
            style: `padding:7px 9px;border:1px solid ${armed ? '#965cde' : on ? '#965cde66' : '#1f1f1f'};background:${armed ? '#965cde14' : '#141416'};border-radius:6px;cursor:${kind === 'formats' ? 'grab' : 'pointer'};display:flex;flex-direction:column;gap:2px;opacity:${x.pinned === false ? 0.55 : 1}` },
            h('div', { style: 'display:flex;align-items:center;gap:6px' }, h('span', { style: `font:11.5px ${MONO};color:#edeef0;overflow:hidden;text-overflow:ellipsis` }, x.name), h('div', { style: 'flex:1' }),
              kind === 'formats' ? h('button', { onClick: (e) => { e.stopPropagation(); set({ armed: armed ? null : x.name }); }, title: 'arm: the next empty position you click gets this format', style: `background:${armed ? '#965cde' : 'transparent'};color:${armed ? '#fff' : '#965cde'};border:1px solid #965cde88;border-radius:4px;font-size:10px;padding:0 6px;cursor:pointer` }, armed ? 'armed' : 'arm') : null),
            h('span', { style: 'font-size:10.5px;color:#757575' }, sub(x), x.pinned === false ? ' · pack not pinned' : '')); })));
  }

  // ---------------------------------------------------------------- right: the draft panel
  function draftPanel(c) {
    const S = c.A.S, dz = S.dz, D = c.D, p = D.plan, tab = dz.draftTab || 'changes';
    const set = (o) => c.A.set({ dz: { ...c.A.S.dz, ...o } });
    const n = D.ops.length;
    const status = !n ? ['no changes', '#4d4d4d'] : D.planning ? ['checking…', '#b48ae8'] : !p ? ['…', '#757575'] : p.ok ? [`valid · ${(p.warnings || []).length} warnings`, '#5cde7b'] : [`${(p.problems || []).length} problems`, '#ec7575'];
    const canApply = !!(n && p && p.ok && !D.planning && !dz.applying && c.src.caps.design);
    const apply = () => {
      if (!canApply) return;
      if (!dz.confirm) { set({ confirm: true }); return; }
      set({ confirm: false, applying: true, log: [], draftTab: 'log' });
      c.src.applyOps(D.ops, (line) => { c.A.S.dz.log = [...(c.A.S.dz.log || []), line]; c.A.render(); }, (ok) => { c.A.S.dz.applying = false; if (ok) { D.ops = []; D.redoStack = []; D.save(); D.rebase(); } c.A.render(); });
    };
    const exportYaml = () => {
      const text = p && p.yaml; if (!text) return;
      const url = URL.createObjectURL(new Blob([text], { type: 'application/yaml' }));
      const a = document.createElement('a'); a.href = url; a.download = `${c.src.estate.name}.yaml`; document.body.appendChild(a); a.click(); a.remove(); setTimeout(() => URL.revokeObjectURL(url), 2000);
    };
    const tabs = [['changes', `Changes ${n || ''}`], ['checks', 'Checks'], ['impact', 'Impact'], ['diff', 'Diff'], ...(dz.log ? [['log', 'Apply']] : [])];
    let body;
    if (tab === 'checks') body = checks(c);
    else if (tab === 'impact') body = impact(c);
    else if (tab === 'diff') body = h('pre', { style: `margin:0;padding:8px 10px;font:10.5px/1.55 ${MONO};white-space:pre;overflow:auto` }, (p && p.diff ? p.diff.split('\n') : ['(no changes)']).map((l, i) => h('div', { key: i, style: `color:${l[0] === '+' ? '#5cde7b' : l[0] === '-' ? '#ec7575' : l[0] === '@' ? '#5b9dff' : '#757575'}` }, l || ' ')));
    else if (tab === 'log') body = h('div', { style: `padding:8px 12px;display:flex;flex-direction:column;gap:4px;font:11px ${MONO}` }, (dz.log || []).map((l, i) => h('span', { key: i, style: `color:${/^✕/.test(l) ? '#ec7575' : /^…/.test(l) ? '#757575' : '#b3b3b3'};overflow-wrap:anywhere` }, /^[✕…]/.test(l) ? l : `✓ ${l}`)), dz.applying ? h('span', { style: 'color:#b48ae8' }, '… working') : null);
    else body = h('div', { style: 'display:flex;flex-direction:column' }, n ? D.ops.map((o, i) => h('div', { key: i, style: 'display:flex;align-items:center;gap:8px;padding:5px 12px;border-bottom:1px solid #161616' },
      h('span', { style: `font:10px ${MONO};color:#4d4d4d;width:18px` }, String(i + 1)),
      h('span', { style: `flex:1;font:11px ${MONO};color:#cfcfcf;overflow-wrap:anywhere` }, D.describe(o)),
      p && p.classes && p.classes[i] ? h('span', { title: p.classes[i] === 'hot' ? 'a hot change on a running site (03): applied here by a restart for now' : 'a cold change: restarts part of the substrate', style: `font:9.5px ${MONO};color:${p.classes[i] === 'hot' ? '#757575' : '#d1b854'};border:1px solid ${p.classes[i] === 'hot' ? '#2a2a2a' : '#d1b85466'};border-radius:3px;padding:0 4px` }, p.classes[i]) : null,
      h('button', { onClick: () => D.removeAt(i), title: 'drop this change', className: 'hv-text', style: 'background:none;border:0;color:#757575;cursor:pointer;font-size:13px;padding:0' }, '×'))) : h('div', { style: 'padding:12px;font-size:11.5px;color:#4d4d4d;line-height:1.6' }, 'No changes yet. Edit anything in the Explorer, the floor or the Inspector; every change is checked by the twin as you go.'));
    return h('div', { style: `${CARD};display:flex;flex-direction:column;min-height:0;overflow:hidden;flex:1 1 0` },
      h('div', { style: 'display:flex;align-items:center;gap:6px;padding:8px 10px;border-bottom:1px solid #1f1f1f' },
        h('div', { style: 'display:flex;flex-direction:column;min-width:0;flex:1' }, h('span', { style: 'font-weight:600;color:#edeef0' }, 'Draft'), h('span', { style: `font-size:10.5px;color:${status[1]};white-space:nowrap;overflow:hidden;text-overflow:ellipsis` }, status[0])),
        h('button', { onClick: () => D.undo(), disabled: !n, title: 'undo', className: 'hv-text', style: 'background:none;border:1px solid #2a2a2a;border-radius:4px;color:#b3b3b3;cursor:pointer;font-size:12px;padding:1px 6px' }, '↶'),
        h('button', { onClick: () => D.redo(), disabled: !D.redoStack.length, title: 'redo', className: 'hv-text', style: 'background:none;border:1px solid #2a2a2a;border-radius:4px;color:#b3b3b3;cursor:pointer;font-size:12px;padding:1px 6px' }, '↷'),
        h('button', { className: 'ds-btn ds-secondary', onClick: () => { if (dz.confirm) set({ confirm: false }); else D.clear(); }, disabled: !n && !dz.confirm }, dz.confirm ? 'Cancel' : 'Discard'),
        h('button', { className: 'ds-btn ds-secondary', onClick: exportYaml, disabled: !(p && p.yaml), title: 'download the estate file with this draft' }, 'Export'),
        h('span', { title: !c.src.caps.design ? 'this twin was not started from an estate file' : dz.confirm ? 'the estate goes down and comes up again on the changed file' : '' },
          h('button', { className: 'ds-btn ds-primary', onClick: apply, disabled: !canApply }, dz.applying ? 'Applying…' : dz.confirm ? 'Confirm restart' : 'Apply'))),
      h('div', { style: 'display:flex;gap:2px;padding:0 8px;border-bottom:1px solid #1f1f1f' }, tabs.map(([k, l]) => h('button', { key: k, onClick: () => set({ draftTab: k }),
        style: `background:none;border:0;border-bottom:2px solid ${tab === k ? '#965cde' : 'transparent'};padding:5px 8px;font-size:11.5px;color:${tab === k ? '#edeef0' : '#757575'};cursor:pointer` }, l))),
      h('div', { style: 'flex:1;min-height:0;overflow:auto' }, body));
  }

  function checks(c) {
    const p = c.D.plan;
    if (!c.D.ops.length) return F.note('Nothing to check: the draft has no changes.');
    if (!p) return h('div', { style: 'padding:12px' }, F.note('checking…', '#b48ae8'));
    const line = (icon, color, t, k) => h('div', { key: k, style: 'display:flex;gap:8px;padding:4px 12px;font-size:11.5px;line-height:1.45' }, h('span', { style: `color:${color};width:12px;flex:none` }, icon), h('span', { style: 'color:#cfcfcf;overflow-wrap:anywhere' }, t));
    const s = p.summary || {};
    return h('div', { style: 'display:flex;flex-direction:column;padding:6px 0' },
      p.ok ? line('✓', '#5cde7b', `compiles: ${(s.racks || [])[0]} → ${(s.racks || [])[1]} racks, ${(s.devices || p.devices || [])[0]} → ${(s.devices || p.devices || [])[1]} devices, ${(s.links || p.links || [])[0]} → ${(s.links || p.links || [])[1]} links`, 'ok') : null,
      (p.problems || []).map((x, i) => line('✕', '#ec7575', x, 'p' + i)),
      (p.warnings || []).map((x, i) => line('⚠', '#d1b854', x, 'w' + i)),
      (p.notes || []).map((x, i) => line('·', '#757575', x, 'n' + i)),
      p.ok ? line('⚠', '#d1b854', 'apply restarts the estate: active faults, power changes and the event log start fresh, and a stack sees every BMC drop and come back', 'r') : null);
  }

  function bar(v, cap, warnAt = 0.8) {
    const pct = cap ? Math.min(100, (v / cap) * 100) : 0, over = cap && v > cap, warn = cap && v > cap * warnAt;
    return h('div', { style: 'height:5px;background:#1f1f1f;border-radius:3px;overflow:hidden;flex:1;min-width:60px' }, h('div', { style: `height:100%;width:${pct}%;background:${over ? '#ec7575' : warn ? '#d1b854' : '#965cde'}` }));
  }
  const kw = (w) => `${(w / 1000).toFixed(w >= 100000 ? 0 : 1)} kW`;
  function impact(c) {
    const im = c.D.impact();
    if (!im) return h('div', { style: 'padding:12px' }, F.note('The impact shows once the draft compiles.'));
    const hdr = (t) => h('div', { style: 'font-size:10.5px;color:#757575;text-transform:uppercase;letter-spacing:.06em;padding:10px 12px 4px' }, t);
    const rowL = (k, kids) => h('div', { key: k, style: `display:flex;align-items:center;gap:8px;padding:3px 12px;font:11px ${MONO};color:#b3b3b3` }, kids);
    const host = c.host || {};
    return h('div', { style: 'display:flex;flex-direction:column;padding-bottom:10px' },
      hdr('Feeds · max draw, shared and if the other feed fails'),
      im.feeds.map((f) => [rowL('fs' + f.name, [h('span', { style: 'width:52px;color:#edeef0' }, `feed ${f.name}`), bar(f.sharedMaxW, f.capacityW), h('span', { style: 'width:150px;text-align:right' }, `${kw(f.sharedMaxW)} / ${kw(f.capacityW)}`)]),
        rowL('ff' + f.name, [h('span', { style: 'width:52px;color:#757575' }, 'failover'), bar(f.failoverMaxW, f.capacityW, 1), h('span', { style: `width:150px;text-align:right;color:${f.failoverMaxW > f.capacityW ? '#ec7575' : '#b3b3b3'}` }, `${kw(f.failoverMaxW)}${f.failoverMaxW > f.capacityW ? ' ▲ over' : ''}`)])]),
      im.loops.length ? [hdr('Cooling loops · max heat against capacity'), im.loops.map((l) => rowL('l' + l.name, [h('span', { style: 'width:52px;color:#edeef0' }, l.name), bar(l.maxKW, l.capacityKW), h('span', { style: 'width:150px;text-align:right' }, `${l.maxKW.toFixed(0)} / ${l.capacityKW} kW`)]))] : null,
      hdr('Fabrics · generated switches and cables'),
      im.fabrics.map((f) => { const ds = f.switches[1] - f.switches[0], dc = f.cables[1] - f.cables[0]; const d = (x) => (x > 0 ? `+${x}` : x < 0 ? String(x) : '±0');
        return rowL('f' + f.name, [h('span', { style: `width:96px;color:${(TC.data.fabricStyle(f.name, 0) || {}).color}` }, f.name), h('span', { style: 'flex:1' }, `${f.switches[1]} switches (${d(ds)}) · ${f.cables[1]} cables (${d(dc)})`), h('span', { style: 'color:#757575' }, `${f.racks.length} racks${f.hub ? ' · ' + f.hub : ''}`)]); }),
      hdr('VM fidelity · against this host'),
      rowL('vm', [h('span', { style: 'flex:1' }, `${im.vm.devices} VMs · ${im.vm.vcpu} vCPU · ${im.vm.ramGB} GB`), h('span', { style: `color:${host.memoryGB && im.vm.ramGB > host.memoryGB ? '#ec7575' : '#757575'}` }, host.cpus ? `host ${host.cpus} CPUs · ${Math.round(host.memoryGB)} GB` : '')]),
      (im.stacks || []).length ? [hdr('Stack capability'), im.stacks.map((s) => rowL('s' + s.stack, [h('span', { style: 'flex:1' }, `${s.stack}: ${Object.entries(s.verdicts).map(([k, v]) => `${v} ${k}`).join(' · ')}`), (s.missing || []).length ? h('span', { style: 'color:#ec7575' }, `${s.missing.length} missing`) : null]))] : null,
      hdr('Racks · max draw against capacity'),
      im.racks.slice().sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true })).map((r) => rowL('r' + r.name, [h('span', { style: 'width:52px;color:#edeef0' }, r.name), bar(r.maxW, r.capacityW, 0.9), h('span', { style: 'width:150px;text-align:right' }, `${kw(r.maxW)} / ${kw(r.capacityW)}`)])));
  }

  // ---------------------------------------------------------------- the designer
  function designer(A, M) {
    const S = A.S;
    if (!A.draft) return h('div', { style: 'padding:24px;color:#757575' }, 'The designer needs a live twin started from an estate file.');
    const c = ctxOf(A), dz = S.dz;
    const views = [['floor', 'Floor'], ['rack', 'Rack'], ['fabrics', 'Fabrics'], ['power', 'Power & cooling']];
    const view = dz.view || 'floor';
    const inspect = Z.inspectors[c.sel && c.sel.kind] || Z.inspectors.none;
    return h('div', { style: 'display:grid;grid-template-columns:272px minmax(0,1fr) 392px;gap:8px;padding:8px;min-height:0;height:100%' },
      h('div', { style: `${CARD};display:flex;flex-direction:column;min-height:0;overflow:hidden` },
        h('div', { style: 'display:flex;gap:2px;padding:0 8px;border-bottom:1px solid #1f1f1f' }, [['explorer', 'Explorer'], ['library', 'Library']].map(([k, l]) => h('button', { key: k, onClick: () => A.set({ dz: { ...S.dz, left: k } }),
          style: `background:none;border:0;border-bottom:2px solid ${(dz.left || 'explorer') === k ? '#965cde' : 'transparent'};padding:8px 8px;font-size:12px;color:${(dz.left || 'explorer') === k ? '#edeef0' : '#757575'};cursor:pointer` }, l))),
        (dz.left || 'explorer') === 'library' ? library(c) : explorer(c)),
      h('div', { style: `${CARD};display:flex;flex-direction:column;min-height:0;min-width:0;overflow:hidden` },
        h('div', { style: 'display:flex;align-items:center;gap:2px;padding:0 10px;border-bottom:1px solid #1f1f1f' },
          views.map(([k, l]) => h('button', { key: k, onClick: () => A.set({ dz: { ...S.dz, view: k } }), style: `background:none;border:0;border-bottom:2px solid ${view === k ? '#965cde' : 'transparent'};padding:8px 10px;font-size:12px;color:${view === k ? '#edeef0' : '#757575'};cursor:pointer` }, l)),
          h('div', { style: 'flex:1' }),
          h('span', { style: `font:11px ${MONO};color:#4d4d4d` }, `${c.src.estate.name} · etag ${c.src.etag || '—'}`)),
        h('div', { style: 'flex:1;min-height:0;overflow:auto;position:relative' }, (Z.views[view] || (() => null))(A, c))),
      h('div', { style: 'display:flex;flex-direction:column;gap:8px;min-height:0' },
        h('div', { style: `${CARD};flex:1.15 1 0;min-height:0;overflow:auto` }, inspect(A, c, c.sel)),
        draftPanel(c)));
  }

  Z.inspectors.none = (A, c) => h('div', { style: 'padding:14px;display:flex;flex-direction:column;gap:8px' },
    h('span', { style: 'font-weight:600;color:#edeef0' }, 'Inspector'),
    F.note('Select a hall, row or rack on the floor or in the Explorer, a fabric, a network, a feed, a loop or a setting, and edit it here. Every change goes into the draft, and the twin checks it as you go.'));
  Z.ctxOf = ctxOf;
  TC.views.designer = designer;
})();
