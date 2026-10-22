// rack.js — the rack elevation of the draft and the slot inspector. The elevation is drawn from
// the compiled draft's layout (U and units per device), so a parameter change that adds or
// removes slots (servers: 2 → 4) shows at once, and generated fabric switches in reserved U
// appear too. A slot can be filled with another profile — only profiles the slot's `accepts`
// lets in are offered — emptied, or reset to the format's own.
window.TC = window.TC || {};
(() => {
  const { h } = TC.h;
  const F = TC.forms, Z = TC.dz, { P, Q } = Z;
  const MONO = F.MONO;

  function layoutRack(c, name) {
    for (const hl of (c.layout || {}).halls || []) for (const r of hl.rows || []) for (const k of r.racks || []) if (k.name === name) return k;
    return null;
  }
  // the format slot a device was made from: its names pattern, with the rack's params put in
  function slotOf(fmt, params, device) {
    for (const s of (fmt && fmt.slots) || []) {
      let names = s.names;
      for (const [k, v] of Object.entries(params || {}).sort((a, b) => b[0].length - a[0].length)) names = names.split('$' + k).join(String(v));
      if (Z.expand(names).includes(device)) return s;
    }
    return null;
  }
  function accepts(a, p) {
    if (!a) return true;
    if (a.class && a.class !== p.class) return false;
    if (a.mounting && a.mounting !== (p.form || {}).mounting) return false;
    if (a.cooling && a.cooling !== p.cooling) return false;
    if (a.units && Number(a.units) !== (p.form || {}).units) return false;
    if (a.maxW && (p.power || {}).maxW > Number(a.maxW)) return false;
    return true;
  }

  function elevation(A, c) {
    const S = A.S, dz = S.dz;
    const name = (c.sel && (c.sel.kind === 'rack' || c.sel.kind === 'slot') && c.sel.name ? c.sel.name : dz.rack) || (c.racks[0] || '');
    const k = layoutRack(c, name);
    if (!k) return h('div', { style: 'padding:16px' }, F.note('Select a rack on the floor to see its elevation.'));
    const at = Q.rack(c.tree, name), over = (at && at.rack.slots) || {};
    const H = k.height || 48, UH = 13, W = 300, X = 60, Y = 18;
    const devs = k.devices || [];
    const slotted = devs.filter((d) => d.units > 0 && d.u > 0 && d.side !== 'zero-u');
    const zero = devs.filter((d) => !(d.units > 0 && d.u > 0) || d.side === 'zero-u');
    const sel = c.sel && c.sel.kind === 'slot' && c.sel.name === name ? c.sel.device : null;
    const color = (d) => d.generated ? '#1b2436' : d.class === 'compute-tray' || d.class === 'server' ? '#1d1e24' : d.class === 'switch-tray' || d.class === 'switch' ? '#151a22' : d.class === 'power-shelf' ? '#1a1620' : '#18191b';
    const svg = [h('rect', { key: 'frame', x: X, y: Y, width: W, height: H * UH, fill: '#0c0c0e', stroke: '#2a2a2a' })];
    for (let u = 0; u < H; u += 6) svg.push(h('text', { key: 'u' + u, x: X - 8, y: Y + u * UH + 9, 'text-anchor': 'end', fill: '#4d4d4d', 'font-size': 9, 'font-family': MONO }, `U${H - u}`));
    for (const d of slotted) {
      const y = Y + (H - (d.u + d.units - 1)) * UH, hh = d.units * UH, on = sel === d.name, ov = over[d.name];
      svg.push(h('g', { key: d.path, onClick: () => c.select({ kind: 'slot', name, device: d.name }), style: 'cursor:pointer' },
        h('rect', { x: X + 1, y: y + 0.5, width: W - 2, height: hh - 1, rx: 2, fill: on ? '#2a2140' : color(d), stroke: on ? '#965cde' : ov ? '#b48ae8' : d.generated ? '#5b9dff55' : '#2a2a2a', 'stroke-dasharray': ov ? '3 2' : null }),
        h('text', { x: X + 8, y: y + hh / 2 + 3.5, fill: d.generated ? '#7aa7ff' : '#cfcfcf', 'font-size': 10, 'font-family': MONO }, d.name),
        h('text', { x: X + W - 8, y: y + hh / 2 + 3.5, 'text-anchor': 'end', fill: '#757575', 'font-size': 9, 'font-family': MONO }, d.generated ? `${d.generated} · generated` : (d.profile || '').split('/').pop())));
    }
    zero.forEach((d, i) => {
      const on = sel === d.name, x = X + W + 14 + i * 18;
      svg.push(h('g', { key: d.path, onClick: () => c.select({ kind: 'slot', name, device: d.name }), style: 'cursor:pointer' },
        h('rect', { x, y: Y, width: 14, height: H * UH, rx: 2, fill: on ? '#2a2140' : '#141416', stroke: on ? '#965cde' : '#2a2a2a' }),
        h('text', { x: x + 10, y: Y + 6, fill: '#757575', 'font-size': 9, 'font-family': MONO, transform: `rotate(90 ${x + 10} ${Y + 6})` }, d.name)));
    });
    const empties = Object.entries(over).filter(([, v]) => v && v.empty).map(([n]) => n);
    return h('div', { style: 'padding:14px 16px;display:flex;flex-direction:column;gap:10px' },
      h('div', { style: 'display:flex;align-items:center;gap:10px' },
        F.select(A, 'el-rack', name, c.racks, (v) => A.set({ dz: { ...A.S.dz, rack: v, sel: { kind: 'rack', name: v } } })),
        h('span', { style: 'font-size:11.5px;color:#757575' }, `${k.format} · ${H}U · ${devs.length} devices · click a slot to fill, empty or reset it`)),
      h('svg', { width: X + W + 20 + zero.length * 18 + 20, height: Y + H * UH + 12, style: 'display:block' }, svg),
      empties.length ? F.note(`left empty by this draft: ${empties.join(', ')}`, '#b48ae8') : null);
  }

  function inspectSlot(A, c, sel) {
    const k = layoutRack(c, sel.name), at = Q.rack(c.tree, sel.name);
    const d = k && (k.devices || []).find((x) => x.name === sel.device);
    const cat = c.catalogue, fmt = (cat.formats || []).find((f) => f.name === (k || {}).format);
    const over = ((at && at.rack.slots) || {})[sel.device];
    const path = at ? [...P.rack(c.tree, at.hall, at.row, sel.name), 'slots', sel.device] : null;
    const head = Z.head(sel.device, `${sel.name} · ${d ? `${d.class} · U${d.u}${d.units > 1 ? '–' + (d.u + d.units - 1) : ''}` : 'emptied in this draft'}`);
    if (d && d.generated) return h('div', null, head, F.section('Generated', [F.note(`Generated by fabric ${d.generated} in this rack's reserved U. Change it through the fabric's template and parameters.`)]));
    const slot = slotOf(fmt, (k || {}).params, sel.device);
    const prof = d ? (cat.profiles || []).find((p) => p.name === d.profile) : null;
    const fits = (cat.profiles || []).filter((p) => p.pinned && accepts(slot && slot.accepts, p));
    return h('div', null, head,
      F.section('Fill', [
        F.kv('slot', slot ? `${slot.names}${slot.accepts ? ' · accepts ' + Object.entries(slot.accepts).map(([a, b]) => `${a} ${b}`).join(', ') : ''}` : '—'),
        F.kv('profile', d ? d.profile : '(empty)'),
        F.row('fill with', F.select(A, 'slot-' + sel.name + sel.device, (over && over.profile) || (d && d.profile) || '', fits.map((p) => [p.name, `${p.name} · ${p.class} · ${Math.round((p.power || {}).maxW || 0)} W`]), (v) => c.push({ op: 'set', path, value: { profile: v } }))),
        F.note(slot && slot.accepts ? `${fits.length} profiles fit this slot.` : 'This slot accepts any profile.'),
        h('div', { style: 'display:flex;gap:6px' },
          !(over && over.empty) ? F.btn('Leave empty', () => c.push({ op: 'set', path, value: { empty: true } }), 'danger') : null,
          over ? F.btn('Reset to the format\'s', () => c.push({ op: 'remove', path })) : null),
      ]),
      prof ? F.section('Device', [
        F.kv('class', `${prof.class} · ${prof.form.units ? prof.form.units + 'U' : prof.form.mounting}`),
        F.kv('power', `${prof.power.idleW} W idle · ${prof.power.maxW} W max`),
        F.kv('cooling', prof.cooling || '—'),
        F.kv('ports', (prof.ports || []).filter((p) => p.role).map((p) => `${p.count}× ${p.kind} ${p.role}${p.speed ? ' ' + p.speed : ''}`).join(' · ') || '—'),
        F.kv('fidelity', prof.fidelity.join(', ') + (prof.vm ? ` · vm ${prof.vm.arch}, ${((prof.vm.cost || {}).vcpu) || '?'} vCPU, ${((prof.vm.cost || {}).ramGB) || '?'} GB` : '')),
        prof.manufacturer ? F.kv('made by', `${prof.manufacturer}${prof.model ? ' ' + prof.model : ''}`) : null,
      ]) : null);
  }

  Z.inspectors.slot = inspectSlot;
  Z.views.rack = elevation;
  Z.accepts = accepts;
})();
