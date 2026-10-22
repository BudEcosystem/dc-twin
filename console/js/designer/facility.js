// facility.js — power and cooling: a one-line view of the draft (feeds, the power chain of UPS,
// busway and panel nodes, the racks they serve, each against capacity), cooling loops with the
// heat they carry, and the feed, power-node, loop, facility-device and BMS inspectors.
window.TC = window.TC || {};
(() => {
  const { h } = TC.h;
  const F = TC.forms, Z = TC.dz, { P, Q } = Z;
  const MONO = F.MONO;
  const kw = (w) => `${(w / 1000).toFixed(0)} kW`;
  const KINDS = [['ups', 'UPS'], ['busway', 'busway'], ['pdu-panel', 'power panel (RPP)']];
  // the racks a node serves: none when its serves is empty (unlike a fabric's select)
  const servedBy = (n, c) => ((n.serves || []).length ? Z.selectRacks(n.serves.join(' '), c.racks) : []);
  const impactOf = (c) => c.D.impact() || { feeds: [], loops: [], racks: [] };
  const CHAIN_NOTE = 'Each node becomes a facility device. A rack source draws through the deepest node of its feed that serves its rack; on a feed loss a UPS carries its load for runtimeS at rated load (longer at less), and a tripped panel drops only its racks. Fail them from Operate or the Facility screen once applied.';

  function bar(v, cap, w = 120) {
    const pct = cap ? Math.min(100, (v / cap) * 100) : 0, over = cap && v > cap;
    return h('div', { style: `width:${w}px;height:5px;background:#1f1f1f;border-radius:3px;overflow:hidden;flex:none` }, h('div', { style: `height:100%;width:${pct}%;background:${over ? '#ec7575' : pct > 80 ? '#d1b854' : '#965cde'}` }));
  }

  // What keeps a part of the power chain or a loop from carrying anything, with the edit that
  // fixes it: a node that serves no rack, racks it serves that draw from another feed, a UPS
  // fed by a UPS, a feed no rack draws from, a loop with no racks, liquid racks on no loop.
  function fixes(A, c) {
    const t = c.tree, im = impactOf(c), nodes = Q.topology(t), loops = Q.loops(t), feedNames = Q.feeds(t).map((f) => f.name);
    const byName = {}; for (const n of nodes) byName[n.name] = n;
    const feedOf = (n) => { const seen = new Set(); let x = n; while (x && byName[x.from] && !seen.has(x.name)) { seen.add(x.name); x = byName[x.from]; } return x ? x.from : null; };
    // a rack's feeds: its own in the draft, else what the last compile gave it, else the first two
    const rackFeeds = (name) => { const at = Q.rack(t, name); if (at && (at.rack.feeds || []).length) return at.rack.feeds; const r = im.racks.find((x) => x.name === name); return r && r.feeds && r.feeds.length ? r.feeds : feedNames.slice(0, 2); };
    const isBelow = (a, b) => { const seen = new Set(); for (let x = a; x && !seen.has(x.name); x = byName[x.from]) { if (x.name === b.name) return true; seen.add(x.name); } return false; };
    // take one rack out of what a node serves: the entry that names it, or the node's racks less it
    const dropServe = (n, r) => { const sv = n.serves || []; const rest = sv.includes(r) ? sv.filter((x) => x !== r) : (() => { const left = servedBy(n, c).filter((x) => x !== r); return left.length ? [Z.braceList(left)] : []; })(); return { op: 'set', path: [...P.node(n.name), 'serves'], value: rest }; };
    const setFeeds = (name, feeds) => { const at = Q.rack(t, name); return at ? [{ op: 'set', path: [...P.rack(t, at.hall, at.row, name), 'feeds'], value: feeds }] : []; };
    const pick = (label, names, commit) => () => A.set({ dz: { ...A.S.dz, view: 'floor', pick: { label, names, commit } } });
    const out = [];
    for (const r of c.racks) for (const F of rackFeeds(r)) {
      // a rack drawing from a feed with a chain, served by no node of it: past the UPS
      const onF = nodes.filter((n) => feedOf(n) === F);
      if (onF.length && !onF.some((n) => servedBy(n, c).includes(r))) {
        const to = onF.filter((n) => n.kind !== 'ups').length ? onF.filter((n) => n.kind !== 'ups') : onF;
        out.push({ key: `p${r}${F}`, text: `${r} draws from feed ${F} straight, past ${onF.filter((n) => n.kind === 'ups').map((n) => n.name).join(', ') || 'its chain'}: no node of feed ${F} serves it, so a loss of ${F} is not ridden through`,
          acts: to.map((p) => [`serve from ${p.name}`, () => c.push({ op: 'set', path: [...P.node(p.name), 'serves'], value: [...(p.serves || []), r] })]) });
      }
      const ns = nodes.filter((n) => feedOf(n) === F && servedBy(n, c).includes(r));
      const tips = ns.filter((n) => !ns.some((m) => m !== n && isBelow(m, n)));
      if (tips.length > 1) out.push({ key: `b${r}${F}`, text: `${r} is served from feed ${F} by both ${tips.map((n) => n.name).join(' and ')}, on different branches of the chain: the twin cannot tell which one feeds it`,
        acts: tips.map((keep) => [`only from ${keep.name}`, () => c.push(...tips.filter((n) => n !== keep).map((n) => dropServe(n, r)))]) });
    }
    for (const n of nodes) {
      if (n.kind !== 'ups' && n.runtimeS) out.push({ key: 'r' + n.name, text: `${n.name} is a ${n.kind === 'busway' ? 'busway' : 'power panel'} with a runtimeS; only a UPS has a battery`,
        acts: [['Remove its runtimeS', () => c.push({ op: 'remove', path: [...P.node(n.name), 'runtimeS'] })]] });
      const parent = byName[n.from], F = feedOf(n), served = servedBy(n, c), below = nodes.some((x) => x.from === n.name);
      if (n.kind === 'ups' && parent && parent.kind === 'ups') out.push({ key: 'u' + n.name, text: `${n.name} is a UPS fed by the UPS ${parent.name}; under a UPS a panel is a power panel`,
        acts: [['Make it a power panel', () => c.push({ op: 'set', path: [...P.node(n.name), 'kind'], value: 'pdu-panel' }, ...(n.runtimeS ? [{ op: 'remove', path: [...P.node(n.name), 'runtimeS'] }] : []))]] });
      if (!(n.serves || []).length && !below) out.push({ key: 's' + n.name, text: `${n.name} serves no rack, so nothing draws through it`,
        acts: [['Pick its racks on the floor', pick(n.name, [], (names) => names.length && c.push({ op: 'set', path: [...P.node(n.name), 'serves'], value: [Z.braceList(names)] }))]] });
      else if ((n.serves || []).length && !served.length) out.push({ key: 'm' + n.name, text: `${n.name}: serves ${(n.serves || []).join(' ')} matches no rack`,
        acts: [['Pick its racks on the floor', pick(n.name, [], (names) => names.length && c.push({ op: 'set', path: [...P.node(n.name), 'serves'], value: [Z.braceList(names)] }))]] });
      if (F) for (const r of served) {
        const fs = rackFeeds(r);
        if (fs.includes(F)) continue;
        out.push({ key: `f${n.name}${r}`, text: `${r} draws from ${fs.join(' and ')}, not ${F}, so ${n.name} carries none of it`,
          acts: fs.map((x) => [`${r} on ${F} instead of ${x}`, () => c.push(...setFeeds(r, fs.map((y) => (y === x ? F : y))))]) });
      }
    }
    for (const f of im.feeds) {
      const reached = nodes.some((n) => feedOf(n) === f.name && (n.serves || []).length);
      if (f.racks || reached) continue;
      out.push({ key: 'F' + f.name, text: `feed ${f.name}: no rack draws from it (a rack takes the first feeds unless it names its own)`,
        acts: [[`Pick racks to draw from ${f.name}`, pick(`feed ${f.name}`, [], (names) => c.push(...names.flatMap((r) => { const fs = rackFeeds(r); return fs.includes(f.name) ? [] : setFeeds(r, [...fs.slice(0, Math.max(1, fs.length - 1)), f.name]); })))]], note: 'each picked rack draws from it in place of its last feed' });
    }
    const liquidOff = im.racks.filter((r) => r.liquid && !r.loop);
    for (const l of loops) if (!(l.racks || []).length) out.push({ key: 'L' + l.name, text: `${l.name} has no racks${liquidOff.length ? `; ${liquidOff.map((r) => r.name).join(', ')} ${liquidOff.length > 1 ? 'are' : 'is'} liquid-cooled and on no loop` : ''}`,
      acts: [...liquidOff.map((r) => [`add ${r.name}`, () => c.push({ op: 'set', path: [...P.loop(l.name), 'racks'], value: [r.name] })]), ['Pick its racks on the floor', pick(l.name, [], (names) => names.length && c.push({ op: 'set', path: [...P.loop(l.name), 'racks'], value: names }))]] });
    for (const r of liquidOff) if (!loops.some((l) => !(l.racks || []).length)) out.push({ key: 'q' + r.name, text: `${r.name} is liquid-cooled and on no loop: ${kw(r.maxW)} of heat on the default supply`,
      acts: loops.map((l) => [`add to ${l.name}`, () => c.push({ op: 'set', path: [...P.loop(l.name), 'racks'], value: [...(l.racks || []), r.name] })]) });
    if (!out.length) return null;
    return h('div', { style: 'display:flex;flex-direction:column;gap:6px;padding:10px 12px;border:1px solid #d1b85455;background:#d1b8540d;border-radius:8px' },
      h('span', { style: 'font-size:10.5px;color:#d1b854;text-transform:uppercase;letter-spacing:.06em' }, 'Suggested fixes'),
      out.map((x) => h('div', { key: x.key, style: 'display:flex;align-items:center;gap:8px;font-size:11.5px;flex-wrap:wrap' },
        h('span', { style: 'color:#cfcfcf;flex:1;min-width:220px' }, x.text, x.note ? h('span', { style: 'color:#757575' }, ` · ${x.note}`) : null),
        x.acts.map(([l, fn]) => h('span', { key: l }, F.btn(l, fn, 'primary'))))));
  }

  function view(A, c) {
    const t = c.tree, im = impactOf(c), feeds = Q.feeds(t), nodes = Q.topology(t), loops = Q.loops(t);
    const fi = {}; for (const f of im.feeds) fi[f.name] = f;
    const box = (sel, title, sub, extra, kids) => h('div', { key: JSON.stringify(sel), onClick: () => c.select(sel), style: `border:1px solid ${c.sel && JSON.stringify(c.sel) === JSON.stringify(sel) ? '#965cde' : '#1f1f1f'};background:#141416;border-radius:6px;padding:8px 10px;cursor:pointer;display:flex;flex-direction:column;gap:4px;min-width:170px` },
      h('span', { style: `font:600 12px ${MONO};color:#edeef0` }, title), sub ? h('span', { style: 'font-size:10.5px;color:#757575' }, sub) : null, extra, kids);
    const childrenOf = (name) => nodes.filter((n) => n.from === name);
    const ni = {}; for (const x of im.nodes || []) ni[x.name] = x;
    const loadBars = (x, cap) => (x ? h('div', { style: 'display:flex;flex-direction:column;gap:3px;margin-top:2px' },
      h('div', { style: `display:flex;align-items:center;gap:6px;font:10.5px ${MONO};color:${x.sharedMaxW > cap ? '#ec7575' : '#b3b3b3'}` }, bar(x.sharedMaxW, cap), `${kw(x.sharedMaxW)} shared`),
      h('div', { style: `display:flex;align-items:center;gap:6px;font:10.5px ${MONO};color:${x.failoverMaxW > cap ? '#ec7575' : '#b3b3b3'}` }, bar(x.failoverMaxW, cap), `${kw(x.failoverMaxW)} if alone${x.failoverMaxW > cap ? ' ▲' : ''}`)) : null);
    const served = (n) => servedBy(n, c);
    const chain = (n, depth) => h('div', { key: n.name, style: `display:flex;gap:10px;align-items:flex-start;margin-left:${depth ? 18 : 0}px;margin-top:6px` },
      h('span', { style: 'color:#4d4d4d;font:11px ' + MONO + ';padding-top:8px' }, '└'),
      h('div', { style: 'display:flex;flex-direction:column' },
        box({ kind: 'node', name: n.name }, n.name, `${(KINDS.find((k) => k[0] === n.kind) || [0, n.kind])[1]}${n.capacityW ? ' · ' + kw(n.capacityW) : ''}${n.kind === 'ups' && n.runtimeS ? ` · ${n.runtimeS} s on battery` : ''}`,
          h('div', { style: 'display:flex;flex-direction:column;gap:3px' }, (n.serves || []).length ? h('span', { style: `font:10.5px ${MONO};color:#b3b3b3` }, `serves ${served(n).join(' ') || '(no rack matches)'}`) : null, loadBars(ni[n.name], n.capacityW))),
        childrenOf(n.name).map((x) => chain(x, depth + 1))));
    return h('div', { style: 'padding:14px 16px;display:flex;flex-direction:column;gap:12px' },
      fixes(A, c),
      h('div', { style: 'display:flex;align-items:center;gap:10px' }, h('span', { style: 'font-weight:600;color:#edeef0' }, 'Power'), h('span', { style: 'font-size:11.5px;color:#757575' }, 'feeds, the chain of UPS, busway and panels, and what each carries at max draw'), h('div', { style: 'flex:1' }),
        F.btn('+ feed', () => c.select({ kind: 'new', what: 'feed' })), F.btn('+ UPS / panel', () => c.select({ kind: 'new', what: 'node' }))),
      h('div', { style: 'display:flex;gap:14px;flex-wrap:wrap;align-items:flex-start' }, feeds.map((f) => {
        const x = fi[f.name] || { sharedMaxW: 0, failoverMaxW: 0, racks: 0 };
        return h('div', { key: f.name, style: 'display:flex;flex-direction:column' },
          box({ kind: 'feed', name: f.name }, `feed ${f.name}`, `${kw(f.capacityW || 0)} · ${x.racks} racks`,
            h('div', { style: 'display:flex;flex-direction:column;gap:3px;margin-top:2px' },
              h('div', { style: `display:flex;align-items:center;gap:6px;font:10.5px ${MONO};color:#b3b3b3` }, bar(x.sharedMaxW, f.capacityW), `${kw(x.sharedMaxW)} shared`),
              h('div', { style: `display:flex;align-items:center;gap:6px;font:10.5px ${MONO};color:${x.failoverMaxW > f.capacityW ? '#ec7575' : '#b3b3b3'}` }, bar(x.failoverMaxW, f.capacityW), `${kw(x.failoverMaxW)} if alone${x.failoverMaxW > f.capacityW ? ' ▲' : ''}`))),
          childrenOf(f.name).map((n) => chain(n, 0)));
      })),
      nodes.length ? F.note(CHAIN_NOTE) : F.note('No power chain: the feeds serve the racks directly. Add a UPS, busway or panel to draw the chain between them.'),
      h('div', { style: 'display:flex;align-items:center;gap:10px;margin-top:8px' }, h('span', { style: 'font-weight:600;color:#edeef0' }, 'Cooling'), h('span', { style: 'font-size:11.5px;color:#757575' }, 'loops, their CDU and the heat of their racks at max draw'), h('div', { style: 'flex:1' }), F.btn('+ loop', () => c.select({ kind: 'new', what: 'loop' }))),
      h('div', { style: 'display:flex;gap:10px;flex-wrap:wrap' }, loops.map((l) => { const x = im.loops.find((y) => y.name === l.name) || { maxKW: 0 };
        return box({ kind: 'loop', name: l.name }, l.name, `${l.profile || 'no CDU'} · ${(l.racks || []).length} racks`,
          h('div', { style: `display:flex;align-items:center;gap:6px;font:10.5px ${MONO};color:${x.maxKW > l.capacityKW ? '#ec7575' : '#b3b3b3'}` }, bar(x.maxKW * 1000, l.capacityKW * 1000), `${x.maxKW.toFixed(0)} / ${l.capacityKW} kW`),
          h('span', { style: `font:10.5px ${MONO};color:${(l.racks || []).length ? '#757575' : '#d1b854'}` }, (l.racks || []).join(' ') || 'no racks')); })));
  }

  // ---------------------------------------------------------------- inspectors
  function inspectFeed(A, c, sel) {
    const f = Q.feeds(c.tree).find((x) => x.name === sel.name);
    if (!f) return h('div', { style: 'padding:14px' }, F.note(`Feed ${sel.name} is not in the draft.`));
    const path = P.feed(f.name), im = impactOf(c).feeds.find((x) => x.name === f.name);
    const users = Q.racks(c.tree).filter((x) => (x.rack.feeds || []).includes(f.name));
    const setK = (k, v) => c.push(v === '' || v === undefined || Number.isNaN(v) ? { op: 'remove', path: [...path, k] } : { op: 'set', path: [...path, k], value: v });
    return h('div', null, Z.head(`feed ${f.name}`, `${kw(f.capacityW || 0)}${im ? ` · ${im.racks} racks` : ''}`),
      F.section('Feed', [
        F.row('name', F.text(A, 'fd-name-' + f.name, f.name, (v) => { if (!v || v === f.name) return;
          c.push({ op: 'set', path: [...path, 'name'], value: v }, ...users.map((u) => ({ op: 'set', path: [...P.rack(c.tree, u.hall, u.row, u.rack.name), 'feeds'], value: u.rack.feeds.map((x) => (x === f.name ? v : x)) })),
            ...Q.topology(c.tree).filter((n) => n.from === f.name).map((n) => ({ op: 'set', path: [...P.node(n.name), 'from'], value: v })));
          c.select({ kind: 'feed', name: v }); }), 'renaming also renames it in the racks and the power chain that name it'),
        F.row('capacity W', F.number(A, 'fd-cap-' + f.name, f.capacityW, (v) => setK('capacityW', v), { width: 140, step: 1000 })),
        F.row('phases', F.number(A, 'fd-ph-' + f.name, f.phases, (v) => setK('phases', v))),
        F.row('voltage', F.number(A, 'fd-v-' + f.name, f.voltage, (v) => setK('voltage', v))),
        im ? F.kv('at max draw', `${kw(im.sharedMaxW)} with both feeds · ${kw(im.failoverMaxW)} if it carries its racks alone`, im.failoverMaxW > f.capacityW ? '#ec7575' : null) : null,
        im && im.failoverMaxW > f.capacityW ? F.note('Over capacity if the other feed is lost: the racks would not all stay up on this one.', '#ec7575') : null,
      ]),
      F.section('Remove', [F.btn(`Remove feed ${f.name}`, () => { c.push({ op: 'remove', path }, ...users.map((u) => ({ op: 'set', path: [...P.rack(c.tree, u.hall, u.row, u.rack.name), 'feeds'], value: u.rack.feeds.filter((x) => x !== f.name) }))); c.select(null); }, 'danger'),
        users.length ? F.note(`also takes it out of ${users.map((u) => u.rack.name).join(', ')}`) : null]));
  }

  function inspectNode(A, c, sel) {
    const n = Q.topology(c.tree).find((x) => x.name === sel.name);
    if (!n) return h('div', { style: 'padding:14px' }, F.note(`Power node ${sel.name} is not in the draft.`));
    const path = P.node(n.name);
    const froms = [...Q.feeds(c.tree).map((f) => [f.name, `feed ${f.name}`]), ...Q.topology(c.tree).filter((x) => x.name !== n.name).map((x) => [x.name, `${x.name} (${x.kind})`])];
    const served = servedBy(n, c);
    const ni = ((impactOf(c).nodes) || []).find((x) => x.name === n.name);
    const setK = (k, v) => c.push(v === '' || v === undefined || Number.isNaN(v) ? { op: 'remove', path: [...path, k] } : { op: 'set', path: [...path, k], value: v });
    return h('div', null, Z.head(n.name, `${n.kind} · from ${n.from || '—'}`),
      F.section('Node', [
        F.row('name', F.text(A, 'nd-name-' + n.name, n.name, (v) => { if (!v || v === n.name) return; c.push({ op: 'set', path: [...path, 'name'], value: v }, ...Q.topology(c.tree).filter((x) => x.from === n.name).map((x) => ({ op: 'set', path: [...P.node(x.name), 'from'], value: v }))); c.select({ kind: 'node', name: v }); })),
        F.row('kind', F.select(A, 'nd-kind-' + n.name, n.kind, KINDS, (v) => c.push({ op: 'set', path: [...path, 'kind'], value: v }, ...(v !== 'ups' && n.runtimeS ? [{ op: 'remove', path: [...path, 'runtimeS'] }] : [])))),
        F.row('from', F.select(A, 'nd-from-' + n.name, n.from || '', froms, (v) => setK('from', v), { none: '— choose —' })),
        F.row('capacity W', F.number(A, 'nd-cap-' + n.name, n.capacityW, (v) => setK('capacityW', v), { width: 140, step: 1000 })),
        n.kind === 'ups' ? F.row('ride-through s', F.number(A, 'nd-rt-' + n.name, n.runtimeS, (v) => setK('runtimeS', v)), 'how long the battery carries the load once its feed is lost') : null,
        ni ? F.kv('at max draw', `${kw(ni.sharedMaxW)} with both feeds · ${kw(ni.failoverMaxW)} if the other feed is lost`, ni.failoverMaxW > n.capacityW ? '#ec7575' : null) : null,
        ni && n.kind === 'ups' && n.runtimeS && n.capacityW ? F.kv('ride-through', `about ${Math.round((n.runtimeS * n.capacityW) / Math.max(ni.sharedMaxW, 0.05 * n.capacityW) / 60)} min at its share of max draw`) : null,
      ]),
      F.section('Serves', [
        F.row('racks', F.text(A, 'nd-sv-' + n.name, (n.serves || []).join(' '), (v) => setK('serves', String(v).trim() ? String(v).trim().split(/\s+/) : undefined), { placeholder: 'rack selectors, e.g. a0* or {b01,b02}' })),
        h('div', { style: 'display:flex;gap:6px;align-items:center;padding-left:104px' },
          F.btn('Pick on the floor', () => A.set({ dz: { ...A.S.dz, view: 'floor', pick: { label: n.name, names: served, commit: (names) => c.push({ op: 'set', path: [...path, 'serves'], value: names.length ? [Z.braceList(names)] : [] }) } } })),
          F.note(`${served.length} racks: ${served.join(' ') || '—'}`)),
        F.note(CHAIN_NOTE),
      ]),
      F.section('Remove', [F.btn(`Remove ${n.name}`, () => { c.push({ op: 'remove', path }); c.select(null); }, 'danger')]));
  }

  function inspectLoop(A, c, sel) {
    const l = Q.loops(c.tree).find((x) => x.name === sel.name);
    if (!l) return h('div', { style: 'padding:14px' }, F.note(`Loop ${sel.name} is not in the draft.`));
    const path = P.loop(l.name), cdus = (c.catalogue.profiles || []).filter((p) => p.class === 'cdu').map((p) => p.name);
    const im = impactOf(c).loops.find((x) => x.name === l.name);
    const setK = (k, v) => c.push(v === '' || v === undefined || Number.isNaN(v) ? { op: 'remove', path: [...path, k] } : { op: 'set', path: [...path, k], value: v });
    return h('div', null, Z.head(l.name, `${l.profile || 'no CDU'} · ${(l.racks || []).length} racks`),
      F.section('Loop', [
        F.row('name', F.text(A, 'lp-name-' + l.name, l.name, (v) => { if (v && v !== l.name) { c.push({ op: 'set', path: [...path, 'name'], value: v }); c.select({ kind: 'loop', name: v }); } })),
        F.row('CDU', F.select(A, 'lp-cdu-' + l.name, l.profile || '', cdus, (v) => setK('profile', v), { none: '— none —' })),
        F.row('capacity kW', F.number(A, 'lp-cap-' + l.name, l.capacityKW, (v) => setK('capacityKW', v))),
        F.row('supply °C', F.number(A, 'lp-sup-' + l.name, l.supplyC, (v) => setK('supplyC', v), { placeholder: '30' })),
        im ? F.kv('heat at max draw', `${im.maxKW.toFixed(0)} kW of ${l.capacityKW} kW`, im.maxKW > l.capacityKW ? '#ec7575' : null) : null,
      ]),
      F.section('Racks', [
        F.kv('on the loop', (l.racks || []).join(' ') || '—'),
        h('div', null, F.btn('Pick on the floor', () => A.set({ dz: { ...A.S.dz, view: 'floor', pick: { label: `loop ${l.name}`, names: l.racks || [], commit: (names) => c.push({ op: 'set', path: [...path, 'racks'], value: names }) } } }))),
      ]),
      F.section('Remove', [F.btn(`Remove ${l.name}`, () => { c.push({ op: 'remove', path }); c.select(null); }, 'danger')]));
  }

  function inspectFdev(A, c, sel) {
    const d = Q.fdevs(c.tree).find((x) => x.name === sel.name);
    if (!d) return h('div', { style: 'padding:14px' }, F.note(`${sel.name} is not in the draft.`));
    const path = P.fdev(d.name), profs = (c.catalogue.profiles || []).filter((p) => ['cdu', 'sensor', 'pdu', 'ups'].includes(p.class)).map((p) => p.name);
    return h('div', null, Z.head(d.name, `facility device · ${d.profile}`),
      F.section('Device', [
        F.row('profile', F.select(A, 'fdv-p-' + d.name, d.profile, profs, (v) => c.push({ op: 'set', path: [...path, 'profile'], value: v }))),
        F.row('at', F.text(A, 'fdv-at-' + d.name, d.at || '', (v) => c.push({ op: 'set', path: [...path, 'at'], value: v }))),
      ]),
      F.section('Remove', [F.btn(`Remove ${d.name}`, () => { c.push({ op: 'remove', path }); c.select(null); }, 'danger')]));
  }

  function inspectBMS(A, c) {
    const b = Q.facility(c.tree).bms || {}, path = ['spec', 'facility', 'bms'];
    const topics = (c.catalogue.topics || []).map((x) => [x.name, `${x.name}${x.pinned ? '' : ' (pack not pinned)'}`]);
    return h('div', null, Z.head('BMS', 'how the facility publishes its points to the stack'),
      F.section('Publisher', [
        F.row('dialect', F.select(A, 'bms-d', b.dialect || '', topics, (v) => c.push(v ? { op: 'set', path: [...path, 'dialect'], value: v } : { op: 'remove', path }), { none: '— none —' })),
        b.dialect ? F.row('broker', F.text(A, 'bms-b', b.broker || '', (v) => c.push(v ? { op: 'set', path: [...path, 'broker'], value: v } : { op: 'remove', path: [...path, 'broker'] }), { placeholder: 'host:port, normally a stack output' })) : null,
      ]));
  }

  // ---------------------------------------------------------------- creators
  const form = (A, prefix) => {
    const ed = A.S.dz.edit, v = (f, d) => (ed[prefix + f] !== undefined ? ed[prefix + f] : d);
    return { v, ed,
      input: (f, d, ph) => h('input', { className: 'dz-in', style: F.INPUT, value: v(f, d), placeholder: ph || '', onInput: (e) => { ed[prefix + f] = e.target.value; } }),
      pick: (f, d, opts) => h('select', { className: 'dz-in', style: F.INPUT, value: v(f, d), onChange: (e) => { ed[prefix + f] = e.target.value; A.render(); } }, opts.map(([o, l]) => h('option', { key: o, value: o }, l || o))),
      clear: (fs) => fs.forEach((f) => delete ed[prefix + f]) };
  };
  function newFeed(A, c) {
    const f = form(A, 'new-feed-');
    return h('div', null, Z.head('New feed', 'a utility feed into the site'),
      F.section('Feed', [F.row('name', f.input('name', '', 'e.g. C')), F.row('capacity W', f.input('cap', '900000')),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add feed', () => { const name = String(f.v('name', '')).trim(); if (!name) return; c.push({ op: 'add', path: ['spec', 'facility', 'power', 'feeds', '-'], value: { name, capacityW: Number(f.v('cap', '900000')) } }); f.clear(['name', 'cap']); c.select({ kind: 'feed', name }); }, 'primary'))]));
  }
  function newNode(A, c) {
    const f = form(A, 'new-node-');
    const froms = [...Q.feeds(c.tree).map((x) => [x.name, `feed ${x.name}`]), ...Q.topology(c.tree).map((x) => [x.name, `${x.name} (${x.kind})`])];
    return h('div', null, Z.head('New UPS, busway or panel', 'a node of the power chain between a feed and racks'),
      F.section('Node', [F.row('name', f.input('name', '', 'e.g. ups-a')), F.row('kind', f.pick('kind', 'ups', KINDS)), F.row('from', f.pick('from', (froms[0] || [''])[0], froms)),
        F.row('capacity W', f.input('cap', '500000')), f.v('kind', 'ups') === 'ups' ? F.row('ride-through s', f.input('rt', '300')) : null,
        F.row('serves', f.input('serves', '', 'racks, e.g. a0* (panels and busways)')),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add', () => {
          const name = String(f.v('name', '')).trim(); if (!name) return;
          const node = { name, kind: f.v('kind', 'ups'), from: f.v('from', (froms[0] || [''])[0]), capacityW: Number(f.v('cap', '500000')) };
          if (node.kind === 'ups') node.runtimeS = Number(f.v('rt', '300'));
          const sv = String(f.v('serves', '')).trim(); node.serves = sv ? sv.split(/\s+/) : [];
          c.push({ op: 'add', path: ['spec', 'facility', 'power', 'topology', '-'], value: node }); f.clear(['name', 'kind', 'from', 'cap', 'rt', 'serves']); c.select({ kind: 'node', name });
        }, 'primary')), F.note(CHAIN_NOTE)]));
  }
  function newLoop(A, c) {
    const f = form(A, 'new-loop-'), cdus = (c.catalogue.profiles || []).filter((p) => p.class === 'cdu').map((p) => [p.name]);
    const liquid = (impactOf(c).racks || []).filter((r) => r.liquid && !r.loop).map((r) => r.name);
    return h('div', null, Z.head('New cooling loop', 'a CDU and the liquid-cooled racks on it'),
      F.section('Loop', [F.row('name', f.input('name', '', 'e.g. cdu-b')), F.row('CDU', f.pick('cdu', (cdus[0] || [''])[0], cdus)), F.row('capacity kW', f.input('cap', '600')),
        F.row('racks', f.input('racks', liquid.join(' '), 'rack names')), liquid.length ? F.note(`filled in: the liquid-cooled racks on no loop (${liquid.join(', ')})`) : null,
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add loop', () => {
          const name = String(f.v('name', '')).trim(); if (!name) return;
          c.push({ op: 'add', path: ['spec', 'facility', 'cooling', 'loops', '-'], value: { name, profile: f.v('cdu', (cdus[0] || [''])[0]), racks: String(f.v('racks', liquid.join(' '))).split(/[\s,]+/).filter(Boolean), capacityKW: Number(f.v('cap', '600')) } });
          f.clear(['name', 'cdu', 'cap', 'racks']); c.select({ kind: 'loop', name });
        }, 'primary'))]));
  }

  Object.assign(Z.inspectors, { feed: inspectFeed, node: inspectNode, loop: inspectLoop, fdev: inspectFdev, bms: inspectBMS });
  Z.creators = Object.assign(Z.creators || {}, { feed: newFeed, node: newNode, loop: newLoop });
  Z.views.power = view;
})();
