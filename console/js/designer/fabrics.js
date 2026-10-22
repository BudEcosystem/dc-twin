// fabrics.js — fabrics and networks: a view of every cabled fabric in the draft (the racks it
// reaches, the rack it is switched in, switches and cables before and after), the fabric and
// network inspectors, and guided fixes for what the twin reports: ports on no fabric, a role
// mapped to two networks, management switches out of ports.
window.TC = window.TC || {};
(() => {
  const { h } = TC.h;
  const F = TC.forms, Z = TC.dz, { P, Q } = Z;
  const MONO = F.MONO;
  const TECH = [['ethernet', 'ethernet'], ['infiniband', 'infiniband'], ['ethernet-roce', 'ethernet-roce']];
  const ROLES = ['management', 'provisioning', 'frontend', 'scale-out', 'storage', 'service'];
  const color = (n) => (TC.data.fabricStyle(n, 0) || {}).color || '#9aa5b8';

  const templateOf = (cat, t) => {
    const name = (cat.templateAliases || {})[t] || t;
    return (cat.templates || []).find((x) => x.name === name);
  };
  const impactOf = (c) => c.D.impact() || { fabrics: [], racks: [] };

  // ---------------------------------------------------------------- guided fixes
  // What the twin said, turned into one-click changes where the fix is clear.
  function fixes(c) {
    const out = [], t = c.tree, nets = Q.networks(t), fabrics = Q.fabrics(t);
    const msgs = [...((c.plan && c.plan.problems) || []), ...((c.plan && c.plan.warnings) || []), ...((!c.plan && c.src.status && c.src.status.warnings) || [])];
    const selected = (f) => Z.selectRacks(f.select, c.racks);
    const fabricsFor = (role) => Object.entries(nets).filter(([, n]) => n.role === role && n.fabric).map(([, n]) => n.fabric);
    for (const m of msgs) {
      let x = /^rack (\S+): (\d+) (infiniband|ethernet) (\S+) ports are on no fabric/.exec(m);
      if (x) {
        const [, rack, n, kind, role] = x;
        const candidates = fabrics.filter((f) => fabricsFor(role).includes(f.name) && ((f.technology || 'ethernet') === kind || (kind === 'ethernet' && (f.technology || '').startsWith('ethernet'))));
        for (const f of candidates) {
          const ops = [{ op: 'set', path: [...P.fabric(f.name), 'select'], value: Z.braceList([...selected(f), rack]) }];
          const clash = fabrics.filter((g) => g !== f && fabricsFor(role).includes(g.name) && selected(g).includes(rack));
          for (const g of clash) ops.push({ op: 'set', path: [...P.fabric(g.name), 'select'], value: Z.braceList(selected(g).filter((r) => r !== rack)) });
          out.push({ text: `${rack} has ${n} ${kind} ${role} ports on no fabric`, action: `Add ${rack} to ${f.name}${clash.length ? ` (and take it out of ${clash.map((g) => g.name).join(', ')})` : ''}`, ops });
        }
        if (!candidates.length) out.push({ text: `${rack} has ${n} ${kind} ${role} ports on no fabric, and no ${kind} fabric carries a ${role} network`, action: null });
        continue;
      }
      x = /^rack (\S+): role (\S+) maps to two networks \((\S+), (\S+)\)/.exec(m);
      if (x) {
        const [, rack, role, n1, n2] = x;
        for (const net of [n1, n2]) {
          const f = fabrics.find((g) => g.name === (nets[net] || {}).fabric); if (!f) continue;
          out.push({ text: `${rack} is on two ${role} networks`, action: `Take ${rack} out of ${f.name}`, ops: [{ op: 'set', path: [...P.fabric(f.name), 'select'], value: Z.braceList(selected(f).filter((r) => r !== rack)) }] });
        }
        continue;
      }
      x = /^rack (\S+): (\d+) management ports exceed its (\d+) management switches/.exec(m);
      if (x) {
        const [, rack] = x;
        const placed = fabrics.filter((f) => (f.placeIn || '') === rack);
        const others = Q.racks(t).filter((r) => r.rack.name !== rack && /network/.test(r.rack.format || '')).map((r) => r.rack.name);
        if (placed.length && others.length) for (const f of placed) out.push({ text: `${rack} has more management ports than its management switches`, action: `Place ${f.name}'s switches in ${others[0]}`, ops: [{ op: 'set', path: [...P.fabric(f.name), 'placeIn'], value: others[0] }] });
        else out.push({ text: `${rack} has more management ports than its management switches: its fabrics' switches need another network rack`, action: 'Arm a network rack to add', arm: 'generic/network-rack' });
      }
    }
    return out;
  }

  // ---------------------------------------------------------------- the view
  function view(A, c) {
    const im = impactOf(c), byName = {}; for (const f of im.fabrics) byName[f.name] = f;
    const fx = fixes(c);
    const card = (f) => {
      const i = byName[f.name] || { switches: [0, 0], cables: [0, 0], racks: [], hub: '' };
      const sel = Z.selectRacks(f.select, c.racks), on = c.sel && c.sel.kind === 'fabric' && c.sel.name === f.name, col = color(f.name);
      const ds = i.switches[1] - i.switches[0], dc = i.cables[1] - i.cables[0], d = (x) => (x > 0 ? `+${x}` : x < 0 ? `${x}` : '');
      const racks = i.racks.filter((r) => r !== i.hub), W = 560, cols = Math.max(1, racks.length), step = Math.min(56, (W - 40) / cols);
      return h('div', { key: f.name, onClick: () => c.select({ kind: 'fabric', name: f.name }), style: `border:1px solid ${on ? '#965cde' : '#1f1f1f'};background:${on ? '#965cde0d' : '#141416'};border-radius:8px;padding:10px 12px;display:flex;flex-direction:column;gap:8px;cursor:pointer` },
        h('div', { style: 'display:flex;align-items:baseline;gap:10px;flex-wrap:wrap' },
          h('span', { style: `font:600 12.5px ${MONO};color:${col}` }, f.name),
          h('span', { style: 'font-size:11px;color:#757575' }, `${f.template}${f.technology ? ' · ' + f.technology : ''} · nos ${f.nos || '—'}`),
          h('div', { style: 'flex:1' }),
          h('span', { style: `font:11px ${MONO};color:#b3b3b3` }, `${i.switches[1]} switches ${d(ds)} · ${i.cables[1]} cables ${d(dc)}`)),
        h('svg', { width: W, height: 86, style: 'display:block' },
          h('rect', { x: W / 2 - 60, y: 4, width: 120, height: 24, rx: 4, fill: '#18191b', stroke: col }),
          h('text', { x: W / 2, y: 20, 'text-anchor': 'middle', fill: '#cfcfcf', 'font-size': 10.5, 'font-family': MONO }, i.hub ? `${i.hub} · ${i.switches[1]} switches` : 'no switches'),
          racks.map((r, k) => { const x = 20 + step * (k + 0.5); return h('g', { key: r },
            h('line', { x1: W / 2, y1: 28, x2: x, y2: 58, stroke: col, 'stroke-opacity': 0.35 }),
            h('rect', { x: x - 20, y: 58, width: 40, height: 18, rx: 3, fill: '#18191b', stroke: sel.includes(r) ? '#2f2f2f' : '#d1b854' }),
            h('text', { x, y: 71, 'text-anchor': 'middle', fill: '#b3b3b3', 'font-size': 9.5, 'font-family': MONO }, r)); })),
        h('div', { style: 'display:flex;gap:12px;font:11px ' + MONO + ';color:#757575;flex-wrap:wrap' },
          h('span', null, `select ${f.select || '(every rack)'} → ${sel.length} racks`), h('span', null, `cabled into ${i.racks.length} racks`), f.placeIn ? h('span', null, `switches in ${f.placeIn}`) : null));
    };
    return h('div', { style: 'padding:14px 16px;display:flex;flex-direction:column;gap:12px' },
      h('div', { style: 'display:flex;align-items:center;gap:10px' }, h('span', { style: 'font-weight:600;color:#edeef0' }, 'Fabrics'), h('span', { style: 'font-size:11.5px;color:#757575' }, 'each cabled fabric in the draft, from its hub to the racks it reaches'), h('div', { style: 'flex:1' }), F.btn('+ fabric', () => c.select({ kind: 'new', what: 'fabric' }))),
      fx.length ? h('div', { style: 'display:flex;flex-direction:column;gap:6px;padding:10px 12px;border:1px solid #d1b85455;background:#d1b8540d;border-radius:8px' },
        h('span', { style: 'font-size:10.5px;color:#d1b854;text-transform:uppercase;letter-spacing:.06em' }, 'Suggested fixes'),
        fx.map((x, i) => h('div', { key: i, style: 'display:flex;align-items:center;gap:10px;font-size:11.5px' }, h('span', { style: 'color:#cfcfcf;flex:1' }, x.text),
          x.ops ? F.btn(x.action, () => c.push(...x.ops), 'primary') : x.arm ? F.btn(x.action, () => A.set({ dz: { ...A.S.dz, armed: x.arm, left: 'library', libKind: 'formats', view: 'floor' } })) : null))) : null,
      Q.fabrics(c.tree).map(card),
      h('div', { style: 'display:flex;align-items:center;gap:10px;margin-top:6px' }, h('span', { style: 'font-weight:600;color:#edeef0' }, 'Networks'), h('div', { style: 'flex:1' }), F.btn('+ network', () => c.select({ kind: 'new', what: 'network' }))),
      h('div', { style: 'display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:8px' }, Object.entries(Q.networks(c.tree)).sort().map(([n, v]) => h('div', { key: n, onClick: () => c.select({ kind: 'network', name: n }), style: `border:1px solid ${c.sel && c.sel.kind === 'network' && c.sel.name === n ? '#965cde' : '#1f1f1f'};background:#141416;border-radius:6px;padding:8px 10px;cursor:pointer;display:flex;flex-direction:column;gap:2px` },
        h('span', { style: `font:12px ${MONO};color:#edeef0` }, n), h('span', { style: 'font-size:11px;color:#757575' }, `${v.role}${v.fabric ? ' · on ' + v.fabric : ''}${v.dhcp ? ' · dhcp ' + v.dhcp : ''}${v.cidr ? ' · ' + v.cidr : ''}`)))));
  }

  // ---------------------------------------------------------------- inspectors
  function inspectFabric(A, c, sel) {
    const f = Q.fabrics(c.tree).find((x) => x.name === sel.name);
    if (!f) return h('div', { style: 'padding:14px' }, F.note(`Fabric ${sel.name} is not in the draft.`));
    const cat = c.catalogue, path = P.fabric(f.name), t = templateOf(cat, f.template);
    const sel2 = Z.selectRacks(f.select, c.racks), im = impactOf(c).fabrics.find((x) => x.name === f.name);
    const nets = Object.entries(Q.networks(c.tree)).filter(([, n]) => n.fabric === f.name).map(([n]) => n);
    const setP = (k, v) => c.push(v === undefined ? { op: 'remove', path: [...path, 'params', k] } : { op: 'set', path: [...path, 'params', k], value: v });
    const undeclared = Object.entries(f.params || {}).filter(([k]) => !(t && t.params && k in t.params));
    const templates = [...Object.keys(cat.templateAliases || {}), ...(cat.templates || []).filter((x) => ['clos', 'rail', 'oob'].includes(x.generator)).map((x) => x.name)];
    return h('div', null,
      Z.head(f.name, `${f.template}${f.technology ? ' · ' + f.technology : ''}${im ? ` · ${im.switches[1]} switches, ${im.cables[1]} cables` : ''}`),
      F.section('Fabric', [
        F.row('name', F.text(A, 'fb-name-' + f.name, f.name, (v) => { if (!v || v === f.name) return; c.push({ op: 'set', path: [...path, 'name'], value: v }, ...nets.map((n) => ({ op: 'set', path: [...P.network(n), 'fabric'], value: v }))); c.select({ kind: 'fabric', name: v }); }), 'renaming also points its networks at the new name'),
        F.row('template', F.select(A, 'fb-t-' + f.name, f.template, templates, (v) => c.push({ op: 'set', path: [...path, 'template'], value: v }))),
        F.row('technology', F.select(A, 'fb-tech-' + f.name, f.technology || 'ethernet', TECH, (v) => c.push({ op: 'set', path: [...path, 'technology'], value: v }))),
        F.row('NOS', F.select(A, 'fb-nos-' + f.name, f.nos || 'none', (cat.nos || []).map((n) => [n.name, n.real ? `${n.name} (real NOS: needs the tap substrate)` : `${n.name} (model switches)`]), (v) => c.push({ op: 'set', path: [...path, 'nos'], value: v }))),
        F.row('manager', F.text(A, 'fb-mgr-' + f.name, f.manager || '', (v) => c.push(v ? { op: 'set', path: [...path, 'manager'], value: v } : { op: 'remove', path: [...path, 'manager'] }), { placeholder: 'e.g. nvidia/ufm' })),
      ]),
      F.section('Racks', [
        F.row('select', F.text(A, 'fb-sel-' + f.name, f.select || '', (v) => c.push(v ? { op: 'set', path: [...path, 'select'], value: v } : { op: 'remove', path: [...path, 'select'] }), { placeholder: 'every rack' })),
        h('div', { style: 'display:flex;gap:6px;align-items:center;padding-left:104px' },
          F.btn('Pick on the floor', () => A.set({ dz: { ...A.S.dz, view: 'floor', pick: { label: `fabric ${f.name}`, names: sel2, commit: (names) => c.push({ op: 'set', path: [...path, 'select'], value: Z.braceList(names) }) } } })),
          F.note(`${sel2.length} racks: ${sel2.join(' ') || '—'}`)),
        F.row('switches in', F.select(A, 'fb-pl-' + f.name, f.placeIn || '', c.racks, (v) => c.push(v ? { op: 'set', path: [...path, 'placeIn'], value: v } : { op: 'remove', path: [...path, 'placeIn'] }), { none: '— the hall\'s first network rack —' })),
        im ? F.kv('cabled', `${im.racks.length} racks · switched in ${im.hub || '—'}`) : null,
      ]),
      t && Object.keys(t.params || {}).length ? F.section(`Parameters · ${t.name}`, Object.entries(t.params).map(([n, d]) => F.param(A, `fb-p-${f.name}-${n}`, n, d, (f.params || {})[n], (v) => setP(n, v), { catalogue: cat, racks: c.racks }))) : null,
      undeclared.length ? F.section('Not declared by the template', [undeclared.map(([k, v]) => F.kv(k, typeof v === 'object' ? JSON.stringify(v) : String(v), '#d1b854')), F.note('kept in the file; the generator ignores them')]) : null,
      F.section('Networks on it', [nets.length ? nets.map((n) => h('div', { key: n, onClick: () => c.select({ kind: 'network', name: n }), className: 'hv-row', style: `cursor:pointer;font:11.5px ${MONO};color:#cfcfcf;padding:2px 4px` }, `${n} · ${Q.networks(c.tree)[n].role}`)) : F.note('none: a fabric carries traffic only for networks that name it'),
        h('div', null, F.btn('+ network on this fabric', () => c.select({ kind: 'new', what: 'network', fabric: f.name })))]),
      F.section('Remove', [F.btn(`Remove ${f.name}`, () => { c.push({ op: 'remove', path }, ...nets.map((n) => ({ op: 'remove', path: P.network(n) }))); c.select(null); }, 'danger'), nets.length ? F.note(`also removes its networks: ${nets.join(', ')}`) : null]));
  }

  function inspectNetwork(A, c, sel) {
    const n = Q.networks(c.tree)[sel.name];
    if (!n) return h('div', { style: 'padding:14px' }, F.note(`Network ${sel.name} is not in the draft.`));
    const path = P.network(sel.name), fabrics = Q.fabrics(c.tree).map((f) => f.name);
    const setK = (k, v) => c.push(v === '' || v === undefined ? { op: 'remove', path: [...path, k] } : { op: 'set', path: [...path, k], value: v });
    return h('div', null, Z.head(sel.name, `${n.role}${n.fabric ? ' on ' + n.fabric : ''}`),
      F.section('Network', [
        F.row('name', F.text(A, 'nw-name-' + sel.name, sel.name, (v) => { if (!v || v === sel.name) return; c.push({ op: 'add', path: P.network(v), value: n }, { op: 'remove', path }); c.select({ kind: 'network', name: v }); })),
        F.row('role', F.select(A, 'nw-role-' + sel.name, n.role, ROLES, (v) => setK('role', v)), 'a rack has at most one network per role'),
        F.row('fabric', F.select(A, 'nw-fab-' + sel.name, n.fabric || '', fabrics, (v) => setK('fabric', v), { none: '— none —' })),
        F.row('DHCP', F.select(A, 'nw-dhcp-' + sel.name, n.dhcp || 'stack', [['stack', 'stack (relayed to the stack\'s server)'], ['twin', 'twin (the labelled twin DHCP)'], ['none', 'none']], (v) => setK('dhcp', v))),
        F.row('CIDR', F.text(A, 'nw-cidr-' + sel.name, n.cidr || '', (v) => setK('cidr', v), { placeholder: 'e.g. 10.40.0.0/20' }), 'needed for dhcp twin or none'),
        F.row('server', F.text(A, 'nw-srv-' + sel.name, n.server || '', (v) => setK('server', v), { placeholder: 'host:67 for dhcp stack' })),
      ]),
      F.section('Remove', [F.btn(`Remove ${sel.name}`, () => { c.push({ op: 'remove', path }); c.select(null); }, 'danger')]));
  }

  // ---------------------------------------------------------------- creators
  function newFabric(A, c, sel) {
    const ed = A.S.dz.edit, k = 'new-fabric-';
    const v = (f, d) => (ed[k + f] !== undefined ? ed[k + f] : d);
    const input = (f, d, ph) => h('input', { className: 'dz-in', style: F.INPUT, value: v(f, d), placeholder: ph || '', onInput: (e) => { ed[k + f] = e.target.value; } });
    const pick = (f, d, opts) => h('select', { className: 'dz-in', style: F.INPUT, value: v(f, d), onChange: (e) => { ed[k + f] = e.target.value; A.render(); } }, opts.map(([o, l]) => h('option', { key: o, value: o }, l || o)));
    return h('div', null, Z.head('New fabric', 'a generated network fabric'),
      F.section('Fabric', [
        F.row('name', input('name', '', 'e.g. storage')),
        F.row('template', pick('template', 'rail-optimised', [['clos-2tier'], ['rail-optimised'], ['oob-flat']])),
        F.row('technology', pick('technology', 'ethernet', TECH)),
        F.row('select', input('select', '', 'racks, e.g. {b02,b03} or a0*')),
        F.row('network', input('network', '', 'a network on it, e.g. backend-storage (optional)')),
        F.row('role', pick('role', 'scale-out', ROLES.map((r) => [r]))),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add fabric', () => {
          const name = (v('name', '') || '').trim(); if (!name) return;
          const fab = { name, template: v('template', 'rail-optimised'), nos: 'none' };
          if (v('technology', 'ethernet') !== 'ethernet') fab.technology = v('technology');
          if (v('select', '')) fab.select = v('select');
          const ops = [{ op: 'add', path: ['spec', 'fabrics', '-'], value: fab }];
          const net = (v('network', '') || '').trim();
          if (net) ops.push({ op: 'add', path: P.network(net), value: { role: v('role', 'scale-out'), fabric: name } });
          for (const f of ['name', 'template', 'technology', 'select', 'network', 'role']) delete ed[k + f];
          c.push(...ops); c.select({ kind: 'fabric', name });
        }, 'primary')),
      ]));
  }
  function newNetwork(A, c, sel) {
    const ed = A.S.dz.edit, k = 'new-net-';
    const v = (f, d) => (ed[k + f] !== undefined ? ed[k + f] : d);
    const fabrics = Q.fabrics(c.tree).map((f) => f.name);
    return h('div', null, Z.head('New network', 'a network on a fabric'),
      F.section('Network', [
        F.row('name', h('input', { className: 'dz-in', style: F.INPUT, value: v('name', ''), placeholder: 'e.g. storage', onInput: (e) => { ed[k + 'name'] = e.target.value; } })),
        F.row('role', h('select', { className: 'dz-in', style: F.INPUT, value: v('role', 'storage'), onChange: (e) => { ed[k + 'role'] = e.target.value; A.render(); } }, ROLES.map((r) => h('option', { key: r, value: r }, r)))),
        F.row('fabric', h('select', { className: 'dz-in', style: F.INPUT, value: v('fabric', sel.fabric || fabrics[0] || ''), onChange: (e) => { ed[k + 'fabric'] = e.target.value; A.render(); } }, fabrics.map((f) => h('option', { key: f, value: f }, f)))),
        F.row('DHCP', h('select', { className: 'dz-in', style: F.INPUT, value: v('dhcp', 'stack'), onChange: (e) => { ed[k + 'dhcp'] = e.target.value; A.render(); } }, ['stack', 'twin', 'none'].map((r) => h('option', { key: r, value: r }, r)))),
        F.row('CIDR', h('input', { className: 'dz-in', style: F.INPUT, value: v('cidr', ''), placeholder: 'needed for dhcp twin or none', onInput: (e) => { ed[k + 'cidr'] = e.target.value; } })),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add network', () => {
          const name = (v('name', '') || '').trim(); if (!name) return;
          const net = { role: v('role', 'storage'), fabric: v('fabric', sel.fabric || fabrics[0]) };
          if (v('dhcp', 'stack') !== 'stack') net.dhcp = v('dhcp');
          if (v('cidr', '')) net.cidr = v('cidr');
          for (const f of ['name', 'role', 'fabric', 'dhcp', 'cidr']) delete ed[k + f];
          c.push({ op: 'add', path: P.network(name), value: net }); c.select({ kind: 'network', name });
        }, 'primary')),
      ]));
  }

  Object.assign(Z.inspectors, { fabric: inspectFabric, network: inspectNetwork });
  Z.creators = Object.assign(Z.creators || {}, { fabric: newFabric, network: newNetwork });
  Z.views.fabrics = view;
  Z.fabricFixes = fixes;
})();
