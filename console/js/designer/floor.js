// floor.js — halls, rows and racks: the floor of the draft (drawn from the twin's compiled
// layout of the draft, so generated switches and new racks appear where they will be) and the
// hall, row, rack and empty-position inspectors. Drag a format from the Library onto an empty
// position to add a rack, drag a rack to move it, click a rack to select it, double-click to open
// its elevation. While a selector is being picked (a fabric's racks, a loop's members), clicking
// racks adds or removes them.
window.TC = window.TC || {};
(() => {
  const { h } = TC.h;
  const F = TC.forms, Z = TC.dz, { P, Q } = Z;
  const MONO = F.MONO;
  const pad = (n) => String(n).padStart(2, '0');
  const kw = (w) => `${(w / 1000).toFixed(0)} kW`;

  // ---------------------------------------------------------------- the floor
  function floor(A, c) {
    const S = A.S, dz = S.dz;
    const layout = c.layout || { halls: [] };
    const halls = layout.halls || [];
    const hallName = dz.hall && halls.some((x) => x.name === dz.hall) ? dz.hall : (halls[0] || {}).name;
    const hall = halls.find((x) => x.name === hallName) || { rows: [] };
    const baseLayout = c.src.layout || { halls: [] };
    const baseHall = (baseLayout.halls || []).find((x) => x.name === hallName) || { rows: [] };
    const baseRacks = {}; for (const r of baseHall.rows || []) for (const k of r.racks || []) baseRacks[k.name] = { row: r.name, pos: k.position, format: k.format };
    const draftNames = new Set(Q.racks(c.tree).map((x) => x.rack.name));
    const im = c.D.impact();
    const imR = {}; if (im) for (const r of im.racks) imR[r.name] = r;
    const fab = {}; if (im) for (const f of im.fabrics) for (const r of f.racks) (fab[r] = fab[r] || []).push(f.name);
    const pick = dz.pick, picked = pick ? new Set(pick.names) : null;
    const set = (o) => A.set({ dz: { ...A.S.dz, ...o } });

    const addAt = (row, pos, format) => {
      const name = Z.freeRackName(c.tree, `${row}${pad(pos)}`);
      c.push({ op: 'rack.add', hall: hallName, row, name, position: pos, format });
      c.select({ kind: 'rack', name });
    };
    const onDrop = (row, pos) => (e) => {
      e.preventDefault();
      const f = e.dataTransfer.getData('text/tc-format'), r = e.dataTransfer.getData('text/tc-rack');
      if (f) addAt(row, pos, f);
      else if (r) { c.push({ op: 'rack.move', name: r, toHall: hallName, toRow: row, toPosition: pos }); c.select({ kind: 'rack', name: r }); }
    };
    const allow = (e) => { e.preventDefault(); e.dataTransfer.dropEffect = e.dataTransfer.types.includes('text/tc-format') ? 'copy' : 'move'; };

    const tile = (row, k) => {
      const b = baseRacks[k.name], isNew = !b, moved = b && (b.row !== row || b.pos !== k.position), changedFmt = b && b.format !== k.format;
      const sel = c.sel && c.sel.kind === 'rack' && c.sel.name === k.name, inPick = picked && picked.has(k.name);
      const r = imR[k.name], pct = r && r.capacityW ? r.maxW / r.capacityW : 0;
      const tag = (k.format || '').split('/').pop();
      const border = inPick ? '1.5px solid #5cde7b' : sel ? '1px solid #965cde' : isNew ? '1.5px dashed #965cde' : moved || changedFmt ? '1.5px dashed #b48ae8' : '1px solid #2a2a2a';
      return h('div', { key: 'k' + k.name, draggable: pick ? null : 'true', onDragStart: (e) => { e.dataTransfer.setData('text/tc-rack', k.name); e.dataTransfer.effectAllowed = 'move'; },
        onClick: () => { if (pick) { const s = new Set(pick.names); s.has(k.name) ? s.delete(k.name) : s.add(k.name); set({ pick: { ...pick, names: [...s] } }); } else c.select({ kind: 'rack', name: k.name }); },
        onDblClick: () => { if (!pick) A.set({ dz: { ...A.S.dz, sel: { kind: 'rack', name: k.name }, view: 'rack' } }); },
        title: `${k.name} · ${k.format}${isNew ? ' · new in this draft' : moved ? ` · moved from ${b.row}${pad(b.pos)}` : ''}`,
        style: `position:relative;width:84px;height:84px;border-radius:6px;background:${inPick ? '#5cde7b14' : sel ? '#965cde1a' : isNew ? '#965cde0f' : '#18191b'};border:${border};outline:${sel ? '1px solid #965cde55' : 'none'};outline-offset:2px;padding:6px 7px;display:flex;flex-direction:column;cursor:${pick ? 'copy' : 'grab'};flex:none` },
        h('div', { style: 'display:flex;align-items:center;gap:4px' }, h('span', { style: 'font-weight:600;font-size:12px;color:#edeef0' }, k.name), h('div', { style: 'flex:1' }),
          isNew ? h('span', { style: `font:9px ${MONO};color:#b48ae8` }, 'new') : moved ? h('span', { style: `font:9px ${MONO};color:#b48ae8` }, 'moved') : changedFmt ? h('span', { style: `font:9px ${MONO};color:#b48ae8` }, 'format') : null),
        h('span', { style: 'font-size:10px;color:#757575;white-space:nowrap;overflow:hidden;text-overflow:ellipsis' }, tag),
        h('div', { style: 'flex:1' }),
        h('div', { style: 'display:flex;gap:3px;align-items:center;height:8px' }, (fab[k.name] || []).map((f) => h('span', { key: f, title: f, style: `width:7px;height:7px;border-radius:2px;background:${(TC.data.fabricStyle(f, 0) || {}).color}` }))),
        h('span', { title: r ? `max draw ${kw(r.maxW)} of ${kw(r.capacityW)} capacity` : '', style: `font:10px ${MONO};color:${pct > 1 ? '#ec7575' : '#b3b3b3'};margin-top:3px;white-space:nowrap` }, r ? `${Math.round(r.maxW / 1000)}/${Math.round(r.capacityW / 1000)} kW` : `${(k.devices || []).length} dev`),
        h('div', { style: 'position:absolute;left:0;right:0;bottom:0;height:3px;border-radius:0 0 6px 6px;overflow:hidden;background:#0a0a0a' }, h('div', { style: `height:100%;width:${Math.min(100, pct * 100)}%;background:${pct > 1 ? '#ec7575' : pct > 0.9 ? '#d1b854' : '#965cde'}` })));
    };
    const ghost = (name, b) => h('div', { key: 'g' + name, title: `${name} · removed in this draft`, onClick: () => c.select({ kind: 'removed', name }),
      style: 'width:84px;height:84px;border-radius:6px;border:1.5px dashed #ec7575;background:#861a1a1a;padding:6px 7px;display:flex;flex-direction:column;cursor:pointer;flex:none;opacity:.8' },
      h('span', { style: 'font-weight:600;font-size:12px;color:#ec7575;text-decoration:line-through' }, name), h('span', { style: 'font-size:10px;color:#757575' }, (b.format || '').split('/').pop()), h('div', { style: 'flex:1' }), h('span', { style: `font:9.5px ${MONO};color:#ec7575` }, 'removed'));
    const empty = (row, pos) => {
      const sel = c.sel && c.sel.kind === 'pos' && c.sel.row === row && c.sel.pos === pos;
      return h('div', { key: 'e' + pos, onDragOver: allow, onDrop: onDrop(row, pos),
        onClick: () => { if (pick) return; if (dz.armed) addAt(row, pos, dz.armed); else c.select({ kind: 'pos', hall: hallName, row, pos }); },
        title: dz.armed ? `add ${dz.armed} here` : 'empty position: drop a format here, or click to add a rack',
        className: 'hv-empty', style: `width:84px;height:84px;border-radius:6px;border:1px dashed ${sel ? '#965cde' : dz.armed ? '#965cde88' : '#2a2a2a'};background:${sel ? '#965cde14' : 'transparent'};display:flex;flex-direction:column;align-items:center;justify-content:center;cursor:pointer;flex:none;color:#4d4d4d` },
        h('span', { style: 'font-size:18px;color:#965cde;line-height:1' }, '+'), h('span', { style: `font:9.5px ${MONO};color:#4d4d4d;margin-top:4px` }, `${row}${pad(pos)}`));
    };

    const rows = (hall.rows || []).map((r) => {
      const at = {}; for (const k of r.racks || []) at[k.position] = k;
      const ghosts = {}; for (const [n, b] of Object.entries(baseRacks)) if (b.row === r.name && !draftNames.has(n)) ghosts[b.pos] = n;
      const rowSel = c.sel && c.sel.kind === 'row' && c.sel.row === r.name;
      return h('div', { key: 'row' + r.name, style: 'display:flex;align-items:center;gap:8px' },
        h('button', { onClick: () => c.select({ kind: 'row', hall: hallName, row: r.name }), title: 'select the row', style: `width:58px;flex:none;text-align:left;background:none;border:0;cursor:pointer;font:11px ${MONO};color:${rowSel ? '#b48ae8' : '#4d4d4d'}` }, `row ${r.name}`, h('br'), h('span', { style: 'font-size:9.5px' }, `${(r.racks || []).length}/${r.positions}`)),
        Array.from({ length: r.positions }, (_, i) => i + 1).map((pos) => (at[pos] ? tile(r.name, at[pos]) : ghosts[pos] ? ghost(ghosts[pos], baseRacks[ghosts[pos]]) : empty(r.name, pos))));
    });
    return h('div', { style: 'padding:14px 16px;display:flex;flex-direction:column;gap:14px;min-width:max-content' },
      h('div', { style: 'display:flex;align-items:center;gap:10px' },
        halls.length > 1 ? F.select(A, 'dz-hall', hallName, halls.map((x) => x.name), (v) => set({ hall: v })) : h('span', { style: 'font-weight:600;color:#edeef0' }, hallName || 'no hall'),
        h('span', { style: 'font-size:11.5px;color:#757575' }, 'drag a format from the Library onto an empty position · drag racks to move them · double-click a rack for its elevation'),
        h('div', { style: 'flex:1' }),
        F.btn('+ row', () => c.select({ kind: 'new', what: 'row', hall: hallName })),
        F.btn('+ hall', () => c.select({ kind: 'new', what: 'hall' }))),
      pick ? h('div', { style: 'display:flex;align-items:center;gap:10px;padding:8px 12px;background:#5cde7b10;border:1px solid #5cde7b55;border-radius:8px;font-size:12px;color:#cfcfcf' },
        h('span', null, `Picking racks for ${pick.label}: click racks to add or remove them · ${pick.names.length} picked`),
        h('span', { style: `font:11px ${MONO};color:#5cde7b` }, Z.braceList(pick.names) || '(none)'), h('div', { style: 'flex:1' }),
        F.btn('Done', () => { const p = pick; set({ pick: null }); p.commit(p.names); }, 'primary'), F.btn('Cancel', () => set({ pick: null }))) : null,
      rows.length ? rows : F.note('This hall has no rows yet: add one.'),
      c.D.planning ? F.note('checking the draft…', '#b48ae8') : null);
  }

  // ---------------------------------------------------------------- inspectors
  const head = (title, sub, right) => h('div', { style: 'padding:12px 12px 8px;display:flex;align-items:center;gap:8px;border-bottom:1px solid #1f1f1f' },
    h('div', { style: 'display:flex;flex-direction:column;gap:2px;min-width:0;flex:1' }, h('span', { style: `font:600 13px ${MONO};color:#edeef0` }, title), sub ? h('span', { style: 'font-size:11px;color:#757575;overflow:hidden;text-overflow:ellipsis' }, sub) : null), right || null);

  function inspectRack(A, c, sel) {
    const at = Q.rack(c.tree, sel.name);
    if (!at) return h('div', { style: 'padding:14px' }, F.note(`Rack ${sel.name} is not in the draft.`));
    const k = at.rack, cat = c.catalogue, fmt = (cat.formats || []).find((f) => f.name === k.format) || { params: {} };
    const path = P.rack(c.tree, at.hall, at.row, k.name);
    const hall = Q.hall(c.tree, at.hall), rows = (hall.rows || []).map((r) => r.name);
    const row = (hall.rows || []).find((r) => r.name === at.row) || { positions: 1, racks: [] };
    const used = new Set((row.racks || []).filter((x) => x.name !== k.name).map((x) => x.position));
    const im = (c.D.impact() || { racks: [], fabrics: [] });
    const r = im.racks.find((x) => x.name === k.name);
    const feeds = Q.feeds(c.tree).map((f) => f.name), myFeeds = k.feeds || null;
    const loops = Q.loops(c.tree), myLoop = (loops.find((l) => (l.racks || []).includes(k.name)) || {}).name || '';
    const setLoop = (name) => {
      const ops = [];
      for (const l of loops) {
        const has = (l.racks || []).includes(k.name), want = l.name === name;
        if (has !== want) ops.push({ op: 'set', path: [...P.loop(l.name), 'racks'], value: want ? [...(l.racks || []), k.name] : (l.racks || []).filter((x) => x !== k.name) });
      }
      if (ops.length) c.push(...ops);
    };
    const paramOp = (name, v) => (v === undefined ? { op: 'remove', path: [...path, 'params', name] } : { op: 'set', path: [...path, 'params', name], value: v });
    const fabricsHere = im.fabrics.filter((f) => f.racks.includes(k.name)).map((f) => f.name);
    return h('div', null,
      head(k.name, `${k.format} · row ${at.row} position ${k.position}`, F.btn('Elevation', () => A.set({ dz: { ...A.S.dz, view: 'rack' } }))),
      F.section('Place', [
        F.row('name', F.text(A, 'rk-name-' + k.name, k.name, (v) => { if (v && v !== k.name) { c.push({ op: 'rack.rename', name: k.name, newName: v }); c.select({ kind: 'rack', name: v }); } })),
        F.row('row', F.select(A, 'rk-row-' + k.name, at.row, rows, (v) => c.push({ op: 'rack.move', name: k.name, toRow: v, toPosition: k.position }))),
        F.row('position', F.select(A, 'rk-pos-' + k.name, k.position, Array.from({ length: row.positions }, (_, i) => i + 1).map((p) => [p, used.has(p) ? `${p} (taken)` : String(p)]), (v) => c.push({ op: 'rack.move', name: k.name, toRow: at.row, toPosition: Number(v) }))),
        F.row('format', F.select(A, 'rk-fmt-' + k.name, k.format, (cat.formats || []).map((f) => [f.name, `${f.name}${f.pinned ? '' : ' (pack not pinned)'}`]), (v) => {
          if (v !== k.format) c.push({ op: 'rack.remove', name: k.name }, { op: 'rack.add', hall: at.hall, row: at.row, name: k.name, position: k.position, format: v });
        }), 'changing the format re-racks the position: new devices, new identities'),
      ]),
      Object.keys(fmt.params || {}).length ? F.section(`Parameters · ${k.format}`, Object.entries(fmt.params).map(([n, d]) => F.param(A, `rk-p-${k.name}-${n}`, n, d, (k.params || {})[n], (v) => c.push(paramOp(n, v)), { catalogue: cat, racks: c.racks.filter((x) => x !== k.name) }))) : null,
      F.section('Power and cooling', [
        // with no feeds of its own a rack draws from the estate's first feeds (as many as its
        // format's inputs take): the compiled draft says which
        F.row('feeds', F.chips(feeds.map((f) => { const dflt = (r && r.feeds && r.feeds.length ? r.feeds : feeds.slice(0, 2)).includes(f); return [f, myFeeds ? myFeeds.includes(f) : dflt, !myFeeds && dflt ? `${f} (default)` : f]; }), (f, on) => {
          const cur = myFeeds || (r && r.feeds && r.feeds.length ? r.feeds : feeds.slice(0, 2)); const next = on ? feeds.filter((x) => x === f || cur.includes(x)) : cur.filter((x) => x !== f);
          c.push({ op: 'set', path: [...path, 'feeds'], value: next });
        }), 'the facility feeds this rack draws from; by default the first ones'),
        myFeeds ? h('div', { style: 'padding-left:104px' }, F.btn('use the default feeds', () => c.push({ op: 'remove', path: [...path, 'feeds'] }))) : null,
        F.row('cooling loop', F.select(A, 'rk-loop-' + k.name, myLoop, loops.map((l) => l.name), setLoop, { none: '— none —' })),
        r ? F.kv('draw', `${kw(r.idleW)} idle · ${kw(r.maxW)} max · capacity ${kw(r.capacityW)}`, r.maxW > r.capacityW ? '#ec7575' : null) : null,
        r && r.liquid && !myLoop ? F.note('liquid-cooled and on no cooling loop: put it on one.', '#d1b854') : null,
      ]),
      F.section('Networks', [F.kv('fabrics', fabricsHere.join(', ') || 'none'), F.note('A rack joins a fabric through the fabric\'s select; edit it under Fabrics.')]),
      F.section('Remove', [h('div', { style: 'display:flex;gap:8px;align-items:center' }, F.btn(`Remove ${k.name}`, () => { c.push({ op: 'rack.remove', name: k.name }); c.select(null); }, 'danger'), F.note('also leaves its cooling loop and hand cabling'))]));
  }

  function inspectPos(A, c, sel) {
    const cat = c.catalogue, dz = A.S.dz;
    const fmts = (cat.formats || []).filter((f) => f.pinned);
    const key = `pos-${sel.row}-${sel.pos}`;
    const fmt = dz.edit[key + '-fmt'] || dz.armed || (fmts[0] || {}).name;
    const name = dz.edit[key + '-name'] ?? Z.freeRackName(c.tree, `${sel.row}${pad(sel.pos)}`);
    return h('div', null, head(`${sel.row}${pad(sel.pos)}`, `empty position · row ${sel.row}`),
      F.section('Add a rack', [
        F.row('format', h('select', { className: 'dz-in', style: F.INPUT, value: fmt, onChange: (e) => { dz.edit[key + '-fmt'] = e.target.value; A.render(); } }, fmts.map((f) => h('option', { key: f.name, value: f.name }, f.name)))),
        F.row('name', h('input', { className: 'dz-in', style: F.INPUT, value: name, onInput: (e) => { dz.edit[key + '-name'] = e.target.value; } })),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add rack', () => { const n = (dz.edit[key + '-name'] || name).trim(); delete dz.edit[key + '-fmt']; delete dz.edit[key + '-name']; c.push({ op: 'rack.add', hall: sel.hall, row: sel.row, name: n, position: sel.pos, format: fmt }); c.select({ kind: 'rack', name: n }); }, 'primary')),
        F.note('Parameters take the values of a rack of the same format already in the hall, or the format\'s defaults; change them once it is added.'),
      ]));
  }

  function inspectRow(A, c, sel) {
    const hall = Q.hall(c.tree, sel.hall), r = (hall.rows || []).find((x) => x.name === sel.row);
    if (!r) return h('div', { style: 'padding:14px' }, F.note(`Row ${sel.row} is not in the draft.`));
    const path = P.row(c.tree, hall.name, r.name);
    const maxPos = Math.max(0, ...(r.racks || []).map((k) => k.position));
    return h('div', null, head(`row ${r.name}`, `${hall.name} · ${(r.racks || []).length} racks in ${r.positions} positions`),
      F.section('Row', [
        F.row('name', F.text(A, 'row-name-' + r.name, r.name, (v) => { if (v) { c.push({ op: 'set', path: [...path, 'name'], value: v }); c.select({ kind: 'row', hall: hall.name, row: v }); } })),
        F.row('positions', F.number(A, 'row-pos-' + r.name, r.positions, (v) => c.push({ op: 'set', path: [...path, 'positions'], value: v }), { min: Math.max(1, maxPos), max: 16 }), 'at most 16: port allocation is by row and position'),
      ]),
      F.section('Racks', (r.racks || []).slice().sort((a, b) => a.position - b.position).map((k) => h('div', { key: k.name, onClick: () => c.select({ kind: 'rack', name: k.name }), className: 'hv-row', style: `display:flex;gap:8px;padding:3px 4px;cursor:pointer;font:11.5px ${MONO};color:#cfcfcf` }, h('span', { style: 'width:28px;color:#757575' }, pad(k.position)), h('span', { style: 'width:48px' }, k.name), h('span', { style: 'color:#757575;overflow:hidden;text-overflow:ellipsis' }, k.format)))),
      F.section('Remove', [h('div', { style: 'display:flex;gap:8px;align-items:center' }, F.btn(`Remove row ${r.name}`, () => { c.push({ op: 'row.remove', hall: hall.name, name: r.name }); c.select(null); }, 'danger'), F.note(`and its ${(r.racks || []).length} racks`))]));
  }

  function inspectHall(A, c, sel) {
    const hall = Q.hall(c.tree, sel.hall);
    const path = P.hall(c.tree, hall.name);
    return h('div', null, head(hall.name, `${(hall.rows || []).length} rows · ${(hall.rows || []).reduce((a, r) => a + (r.racks || []).length, 0)} racks`),
      F.section('Hall', [F.row('name', F.text(A, 'hall-name-' + hall.name, hall.name, (v) => { if (v) { c.push({ op: 'set', path: [...path, 'name'], value: v }); c.select({ kind: 'hall', hall: v }); } }))]),
      F.section('Rows', [(hall.rows || []).map((r) => h('div', { key: r.name, onClick: () => c.select({ kind: 'row', hall: hall.name, row: r.name }), className: 'hv-row', style: `display:flex;gap:8px;padding:3px 4px;cursor:pointer;font:11.5px ${MONO};color:#cfcfcf` }, h('span', { style: 'width:60px' }, `row ${r.name}`), h('span', { style: 'color:#757575' }, `${(r.racks || []).length}/${r.positions}`))),
        h('div', null, F.btn('+ row', () => c.select({ kind: 'new', what: 'row', hall: hall.name })))]),
      Q.halls(c.tree).length > 1 ? F.section('Remove', [F.btn(`Remove ${hall.name}`, () => { c.push({ op: 'hall.remove', name: hall.name }); c.select(null); }, 'danger')]) : null);
  }

  function inspectRemoved(A, c, sel) {
    const i = c.D.ops.findIndex((o) => o.op === 'rack.remove' && o.name === sel.name);
    return h('div', null, head(sel.name, 'removed in this draft'),
      F.section('Removed', [F.note('This rack is taken out by the draft: powered off, unplugged and de-racked when the draft applies.'),
        i >= 0 ? h('div', null, F.btn('Keep it', () => { c.D.removeAt(i); c.select({ kind: 'rack', name: sel.name }); })) : null]));
  }

  Object.assign(Z.inspectors, { rack: inspectRack, pos: inspectPos, row: inspectRow, hall: inspectHall, removed: inspectRemoved });
  Z.views.floor = floor;
  Z.head = head;
})();
