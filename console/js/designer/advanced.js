// advanced.js — the rest of the estate: fidelity overrides (what each costs against this host,
// and the constraints the compiler would refuse), hand cabling, stacks, the estate's settings,
// the Library inspector, and the forms that create halls, rows and overrides. It also routes a
// {kind: 'new'} selection to the creator its area registered.
window.TC = window.TC || {};
(() => {
  const { h } = TC.h;
  const F = TC.forms, Z = TC.dz, { P, Q } = Z;
  const MONO = F.MONO;
  const LEVELS = [['model', 'model · behaviour only'], ['vm', 'vm · a real OS in a VM'], ['hardware', 'hardware · a real device on the bridge']];

  // ---------------------------------------------------------------- selectors on device paths
  // The selector grammar on paths, client side: brace expansion, * within a segment, ** across
  // segments, anchored as a suffix on a segment boundary; ?filters are not applied here.
  const glob = (p, s) => p === '*' || new RegExp('^' + p.split('*').map((x) => x.replace(/[.+?^${}()|[\]\\]/g, '\\$&')).join('.*') + '$').test(s);
  function segs(p, s) {
    if (!p.length) return !s.length;
    if (p[0] === '**') { for (let i = 0; i <= s.length; i++) if (segs(p.slice(1), s.slice(i))) return true; return false; }
    return s.length > 0 && glob(p[0], s[0]) && segs(p.slice(1), s.slice(1));
  }
  // ?key=a|b&key2=c filters are applied for the attributes the layout carries (class, profile,
  // fidelity, rack); others are left to the twin.
  function matchPath(sel, path, dev) {
    const toks = String(sel || '').trim().split(/\s+/).filter(Boolean);
    const filters = toks.flatMap((x) => (x.includes('?') ? x.slice(x.indexOf('?') + 1).split('&') : [])).map((kv) => kv.split('=')).filter((kv) => kv.length === 2);
    const pats = toks.map((x) => x.split('?')[0]).filter(Boolean).flatMap(Z.expand);
    const s = String(path).split('/').filter(Boolean);
    if (!pats.some((p) => { const ps = p.split('/').filter(Boolean); return segs(p.startsWith('/') ? ps : ['**', ...ps], s); })) return false;
    return !dev || filters.every(([k, v]) => !(k in ATTR) || v.split('|').some((alt) => ATTR[k](dev).some((have) => glob(alt, have || ''))));
  }
  const ATTR = { class: (d) => [d.class], profile: (d) => [d.profile], fidelity: (d) => [d.fidelity], rack: (d) => [d.rack] };
  // A selector for the devices of some racks that can run at a level: the racks' devices, filtered
  // to the profiles that support it.
  function overrideSelector(c, racks, level) {
    if (!racks.length) return '';
    const by = {}; for (const p of c.catalogue.profiles || []) by[p.name] = p;
    const on = new Set(racks), profs = [...new Set(devicesOf(c).filter((d) => on.has(d.rack) && by[d.profile] && (by[d.profile].fidelity || []).includes(level)).map((d) => d.profile))].sort();
    return `${Z.braceList(racks)}/**${level === 'model' || !profs.length ? '' : ` ?profile=${profs.join('|')}`}`;
  }
  function devicesOf(c) {
    const out = [];
    for (const hl of (c.layout || {}).halls || []) for (const r of hl.rows || []) for (const k of r.racks || []) for (const d of k.devices || []) out.push({ ...d, rack: k.name, rel: `${k.name}/${d.name}` });
    return out;
  }
  // What a set of devices costs at vm fidelity: vCPU and RAM from each profile's vm cost.
  function vmCost(c, devs) {
    const by = {}; for (const p of c.catalogue.profiles || []) by[p.name] = p;
    let vcpu = 0, ram = 0; const arches = {};
    for (const d of devs) { const p = by[d.profile]; if (!p || !p.vm) continue; const cost = p.vm.cost || {}; vcpu += Number(cost.vcpu || 0); ram += Number(cost.ramGB || 0); if (p.vm.arch) arches[p.vm.arch] = (arches[p.vm.arch] || 0) + 1; }
    return { vcpu, ram, arches };
  }

  // ---------------------------------------------------------------- fidelity overrides
  function inspectOverride(A, c, sel) {
    const list = Q.overrides(c.tree), o = list[sel.i];
    if (!o) return h('div', { style: 'padding:14px' }, F.note('This override is not in the draft.'));
    const path = P.override(sel.i), all = devicesOf(c), hit = all.filter((d) => matchPath(o.select, d.path, d));
    const by = {}; for (const p of c.catalogue.profiles || []) by[p.name] = p;
    const unsupported = hit.filter((d) => by[d.profile] && !(by[d.profile].fidelity || []).includes(o.level));
    const cost = o.level === 'vm' ? vmCost(c, hit) : null, host = c.host || {};
    const qemu = {}; for (const q of host.qemu || []) qemu[q.arch] = q;
    const tcg = cost ? Object.keys(cost.arches).filter((a) => host.arch && a !== host.arch) : [];
    const virtual = Q.spec(c.tree).clock === 'virtual' && o.level !== 'model';
    const move = (to) => { const l = list.slice(); const [x] = l.splice(sel.i, 1); l.splice(to, 0, x); c.push({ op: 'set', path: ['spec', 'fidelity', 'overrides'], value: l }); c.select({ kind: 'override', i: to }); };
    const setK = (k, v) => c.push(v === '' || v === undefined ? { op: 'remove', path: [...path, k] } : { op: 'set', path: [...path, k], value: v });
    const byRack = {}; for (const d of hit) byRack[d.rack] = (byRack[d.rack] || 0) + 1;
    return h('div', null, Z.head(`override ${sel.i + 1} of ${list.length}`, `${o.select} → ${o.level}${o.os ? ' · os ' + o.os : ''}`),
      F.section('Override', [
        F.row('select', F.text(A, 'ov-sel-' + sel.i, o.select, (v) => setK('select', v)), 'a device selector: a01/compute-tray-01, a0{1..4}/**, **/gpu-*'),
        h('div', { style: 'display:flex;gap:6px;align-items:center;padding-left:104px' },
          F.btn('Pick racks on the floor', () => A.set({ dz: { ...A.S.dz, view: 'floor', pick: { label: `override ${sel.i + 1}`, names: Object.keys(byRack), commit: (names) => names.length && setK('select', overrideSelector(c, names, o.level)) } } })),
          F.note(`${hit.length} devices`)),
        F.row('level', F.select(A, 'ov-lv-' + sel.i, o.level, LEVELS, (v) => { setK('level', v); if (v !== 'vm' && o.os) setK('os', undefined); })),
        o.level === 'vm' ? F.row('os', F.select(A, 'ov-os-' + sel.i, o.os || 'twin', [['twin', 'twin · the twin installs the OS'], ['stack', 'stack · the stack installs it (PXE)']], (v) => setK('os', v)), 'D23: two in-host modes for vm hosts') : null,
        F.note('Overrides are evaluated in order; the last match wins.'),
        h('div', { style: 'display:flex;gap:6px' }, sel.i > 0 ? F.btn('↑ earlier', () => move(sel.i - 1)) : null, sel.i < list.length - 1 ? F.btn('↓ later', () => move(sel.i + 1)) : null),
      ]),
      F.section('Matches', [
        hit.length ? h('div', { style: `font:11px ${MONO};color:#b3b3b3;line-height:1.6` }, Object.entries(byRack).map(([r, n]) => `${r} ×${n}`).join(' · ')) : F.note('No device in the draft matches this selector.', '#d1b854'),
        hit.length && hit.length <= 12 ? h('div', { style: `font:10.5px ${MONO};color:#757575;line-height:1.6` }, hit.map((d) => d.rel).join('  ')) : null,
        unsupported.length ? F.note(`${unsupported.length} of them do not support ${o.level} fidelity (${[...new Set(unsupported.map((d) => d.profile))].join(', ')}): the compiler refuses this.`, '#ec7575') : null,
      ]),
      cost ? F.section('Cost on this host', [
        F.kv('these devices', `${cost.vcpu} vCPU · ${cost.ram} GB RAM`),
        host.cpus ? F.kv('the host', `${host.arch} · ${host.cpus} CPUs · ${Math.round(host.memoryGB || 0)} GB`, cost.ram > (host.memoryGB || Infinity) ? '#ec7575' : null) : null,
        c.D.impact() ? F.kv('whole draft', `${c.D.impact().vm.devices} VMs · ${c.D.impact().vm.vcpu} vCPU · ${c.D.impact().vm.ramGB} GB`) : null,
        Object.entries(cost.arches).map(([a, n]) => F.kv(`${a} guests`, `${n} · ${qemu[a] && qemu[a].available ? `qemu ${qemu[a].version || ''} · ${qemu[a].accel || 'tcg'}` : 'no qemu for this architecture'}`, qemu[a] && qemu[a].available ? null : '#ec7575')),
        tcg.length ? F.note(`${tcg.join(', ')} guests on a ${host.arch} host run under TCG: slow, and refused unless the twin was started with --allow-tcg (Q17).`, '#d1b854') : null,
        F.note('The adapters can scale a guest down (spec.adapters.vm: memoryMB, cpus); the fidelity report prints the scaling.'),
      ]) : null,
      virtual ? F.section('Clock', [F.note('spec.clock is virtual: the compiler refuses vm and hardware devices (D22). Set the clock to wall in Settings.', '#ec7575')]) : null,
      F.section('Remove', [F.btn('Remove this override', () => { c.push({ op: 'remove', path }); c.select(null); }, 'danger')]));
  }
  function newOverride(A, c, sel) {
    const ed = A.S.dz.edit, k = 'new-ov-';
    const pickRacks = sel.racks || [];
    const v = (f, d) => (ed[k + f] !== undefined ? ed[k + f] : d);
    const selector = v('sel', overrideSelector(c, pickRacks, v('lv', 'vm')));
    const hit = selector ? devicesOf(c).filter((d) => matchPath(selector, d.path, d)) : [];
    return h('div', null, Z.head('New fidelity override', 'raise or lower the fidelity of some devices'),
      F.section('Override', [
        F.row('select', h('input', { className: 'dz-in', style: F.INPUT, value: selector, placeholder: 'e.g. a01/compute-tray-01', onInput: (e) => { ed[k + 'sel'] = e.target.value; }, onBlur: () => A.render() })),
        F.row('level', h('select', { className: 'dz-in', style: F.INPUT, value: v('lv', 'vm'), onChange: (e) => { ed[k + 'lv'] = e.target.value; A.render(); } }, LEVELS.map(([l, t]) => h('option', { key: l, value: l }, t)))),
        v('lv', 'vm') === 'vm' ? F.row('os', h('select', { className: 'dz-in', style: F.INPUT, value: v('os', 'twin'), onChange: (e) => { ed[k + 'os'] = e.target.value; } }, h('option', { value: 'twin' }, 'twin'), h('option', { value: 'stack' }, 'stack'))) : null,
        F.note(selector ? `${hit.length} devices match${hit.length && v('lv', 'vm') === 'vm' ? ` · ${vmCost(c, hit).vcpu} vCPU, ${vmCost(c, hit).ram} GB at vm` : ''}` : 'Pick racks on the floor, or write a selector.'),
        h('div', { style: 'display:flex;gap:6px;justify-content:flex-end' },
          F.btn('Pick racks on the floor', () => A.set({ dz: { ...A.S.dz, view: 'floor', pick: { label: 'new override', names: pickRacks, commit: (names) => { delete ed[k + 'sel']; A.set({ dz: { ...A.S.dz, sel: { kind: 'new', what: 'override', racks: names } } }); } } } })),
          F.btn('Add override', () => {
            const s = String(selector).trim(); if (!s) return;
            const o = { select: s, level: v('lv', 'vm') }; if (o.level === 'vm') o.os = v('os', 'twin');
            const fid = Q.spec(c.tree).fidelity;
            c.push(fid && fid.overrides ? { op: 'add', path: ['spec', 'fidelity', 'overrides', '-'], value: o } : { op: 'set', path: ['spec', 'fidelity', 'overrides'], value: [o] });
            for (const f of ['sel', 'lv', 'os']) delete ed[k + f];
            c.select({ kind: 'override', i: Q.overrides(c.tree).length });
          }, 'primary')),
      ]));
  }

  // ---------------------------------------------------------------- hand cabling
  function inspectCabling(A, c) {
    const cab = Q.cabling(c.tree), adds = cab.add || [], rems = cab.remove || [];
    const ed = A.S.dz.edit, links = c.src.links || [];
    const q = (ed['cab-q'] || '').toLowerCase();
    const found = q.length >= 2 ? links.filter((l) => `${l.a.ref} ${l.b.ref} ${l.fabric || ''}`.toLowerCase().includes(q)).slice(0, 30) : [];
    const ensure = (key, list) => (cab[key] ? { op: 'add', path: ['spec', 'cabling', key, '-'], value: list[0] } : { op: 'set', path: ['spec', 'cabling', key], value: list });
    const portRefs = [...new Set(links.flatMap((l) => [l.a.ref, l.b.ref]))].sort();
    return h('div', null, Z.head('Cabling', 'cables added or taken out by hand, after the fabrics are generated'),
      h('datalist', { id: 'tc-port-refs' }, portRefs.slice(0, 600).map((r) => h('option', { key: r, value: r }))),
      F.section(`Added · ${adds.length}`, [
        adds.map((x, i) => h('div', { key: 'a' + i, style: `display:flex;gap:6px;align-items:center;font:11px ${MONO};color:#cfcfcf` }, h('span', { style: 'flex:1;overflow-wrap:anywhere' }, `${x.a} ⟷ ${x.b}${x.kind ? ' · ' + x.kind : ''}`),
          h('button', { onClick: () => c.push({ op: 'remove', path: ['spec', 'cabling', 'add', i] }), className: 'hv-text', style: 'background:none;border:0;color:#757575;cursor:pointer' }, '×'))),
        F.row('port A', h('input', { className: 'dz-in', style: F.INPUT, list: 'tc-port-refs', value: ed['cab-a'] || '', placeholder: 'rack/device:port, e.g. b01/server-01:ib0', onInput: (e) => { ed['cab-a'] = e.target.value; } })),
        F.row('port B', h('input', { className: 'dz-in', style: F.INPUT, list: 'tc-port-refs', value: ed['cab-b'] || '', placeholder: 'c02/leaf-ib-1:p1', onInput: (e) => { ed['cab-b'] = e.target.value; } })),
        F.row('kind', h('input', { className: 'dz-in', style: F.INPUT, value: ed['cab-k'] || '', placeholder: 'cable (default), dac, aoc…', onInput: (e) => { ed['cab-k'] = e.target.value; } })),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add cable', () => {
          const a = (ed['cab-a'] || '').trim(), b = (ed['cab-b'] || '').trim(); if (!a || !b) return;
          const x = { a, b }; if ((ed['cab-k'] || '').trim()) x.kind = ed['cab-k'].trim();
          c.push(ensure('add', [x])); delete ed['cab-a']; delete ed['cab-b']; delete ed['cab-k'];
        }, 'primary')),
        F.note('Each end must match exactly one port; a port already cabled by a fabric must be taken out first.'),
      ]),
      F.section(`Taken out · ${rems.length}`, [
        rems.map((r, i) => h('div', { key: 'r' + i, style: `display:flex;gap:6px;align-items:center;font:11px ${MONO};color:#cfcfcf` }, h('span', { style: 'flex:1;overflow-wrap:anywhere' }, r),
          h('button', { onClick: () => c.push({ op: 'remove', path: ['spec', 'cabling', 'remove', i] }), className: 'hv-text', style: 'background:none;border:0;color:#757575;cursor:pointer' }, '×'))),
        F.row('find a cable', h('input', { className: 'dz-in', style: F.INPUT, value: ed['cab-q'] || '', placeholder: 'a port, device or fabric (running estate)', onInput: (e) => { ed['cab-q'] = e.target.value; A.render(); } })),
        found.map((l) => h('div', { key: l.id, style: `display:flex;gap:6px;align-items:center;font:10.5px ${MONO};color:#b3b3b3` },
          h('span', { style: 'flex:1;overflow-wrap:anywhere' }, `${c.src.short(l.a.ref)} ⟷ ${c.src.short(l.b.ref)}`), h('span', { style: `color:${(TC.data.fabricStyle(l.fabric || '', 0) || {}).color || '#757575'}` }, l.fabric || l.kind),
          rems.includes(l.a.ref) ? h('span', { style: 'color:#757575' }, 'out') : F.btn('take out', () => c.push(ensure('remove', [l.a.ref]))))),
      ]));
  }

  // ---------------------------------------------------------------- stacks
  function inspectStacks(A, c) {
    const list = Q.stacks(c.tree), cat = c.catalogue.stacks || [];
    const catOf = (n) => cat.find((x) => x.name === n || String(n).startsWith(x.name + '@') || String(x.name).startsWith(String(n).split('@')[0] + '@'));
    const clock = Q.spec(c.tree).clock;
    const ed = A.S.dz.edit;
    const missing = cat.filter((x) => !list.some((s) => catOf(s.name) === x));
    return h('div', null, Z.head('Stacks', 'the software installed on the twin, against which it is tested'),
      list.map((s, i) => {
        const def = catOf(s.name) || { components: [], options: {} }, path = ['spec', 'stacks', i];
        const comps = s.components || def.components || [];
        const opts = Object.entries(def.options || {});
        return F.section(s.name, [
          def.title ? F.note(def.title) : null,
          F.row('components', F.chips((def.components || []).map((k) => [k, !s.components || s.components.includes(k), k]), (k, on) => {
            const now = new Set(comps); if (on) now.add(k); else now.delete(k);
            const v = (def.components || []).filter((x) => now.has(x));
            c.push(v.length === (def.components || []).length ? { op: 'remove', path: [...path, 'components'] } : { op: 'set', path: [...path, 'components'], value: v });
          }), 'all, unless the estate names some'),
          opts.map(([k, d]) => {
            const cur = (s.options || {})[k];
            if (d.type === 'bool') return F.row(k, F.toggle(cur !== undefined ? !!cur : !!d.default, () => c.push({ op: 'set', path: [...path, 'options', k], value: !(cur !== undefined ? !!cur : !!d.default) }), cur === undefined ? 'default' : ''), d.description || '');
            return F.param(A, `st-${i}-${k}`, k, d, cur, (v) => c.push(v === undefined ? { op: 'remove', path: [...path, 'options', k] } : { op: 'set', path: [...path, 'options', k], value: v }), { catalogue: c.catalogue, racks: c.racks });
          }),
          Object.keys(s.outputs || {}).length ? F.kv('outputs', Object.entries(s.outputs).map(([k, v]) => `${k}=${v}`).join(' · ')) : null,
          clock === 'virtual' ? F.note('A stack forces clock: wall (D22); the compiler refuses this with a virtual clock.', '#ec7575') : null,
          h('div', null, F.btn(`Remove ${s.name}`, () => c.push({ op: 'remove', path }), 'danger')),
        ]);
      }),
      F.section('Add a stack', [
        cat.length ? F.row('stack', h('select', { className: 'dz-in', style: F.INPUT, value: ed['st-new'] || '', onChange: (e) => { ed['st-new'] = e.target.value; A.render(); } }, h('option', { value: '' }, '— choose —'), missing.map((x) => h('option', { key: x.name, value: x.name }, `${x.name}${x.title ? ' · ' + x.title : ''}`)))) : F.note('No stack is in the pinned packs.'),
        ed['st-new'] ? h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add stack', () => { const n = ed['st-new']; delete ed['st-new']; c.push(Q.spec(c.tree).stacks ? { op: 'add', path: ['spec', 'stacks', '-'], value: { name: n } } : { op: 'set', path: ['spec', 'stacks'], value: [{ name: n }] }); }, 'primary')) : null,
        F.note('A stack is installed when the estate comes up; its outputs (broker, DHCP server) are wired into the BMS and the networks.'),
      ]));
  }

  // ---------------------------------------------------------------- settings
  function inspectSettings(A, c) {
    const t = c.tree, sp = Q.spec(t), packs = sp.packs || [], cat = c.catalogue.packs || [];
    const setK = (path, v) => c.push(v === '' || v === undefined || Number.isNaN(v) ? { op: 'remove', path } : { op: 'set', path, value: v });
    const cold = () => h('span', { title: 'a cold change: the estate restarts on apply', style: `font:9.5px ${MONO};color:#d1b854;border:1px solid #d1b85466;border-radius:3px;padding:0 4px` }, 'cold');
    const leaves = (obj, base) => Object.entries(obj || {}).flatMap(([k, v]) => (v && typeof v === 'object' && !Array.isArray(v) ? leaves(v, [...base, k]) : [[[...base, k], v]]));
    const vmGuests = devicesOf(c).filter((d) => d.fidelity === 'vm' || d.fidelity === 'hardware').length;
    return h('div', null, Z.head(`${(t.metadata || {}).name || 'estate'}`, 'the estate\'s settings'),
      F.section('Estate', [
        F.row('name', F.text(A, 'set-name', (t.metadata || {}).name, (v) => v && setK(['metadata', 'name'], v)), 'also the applied file\'s name'),
        F.row('seed', F.number(A, 'set-seed', sp.seed, (v) => setK(['spec', 'seed'], Math.round(v))), 'the root of every derived identity (serials, MACs) and of fault randomness'),
        F.note('Changing the seed changes every serial and MAC address in the estate.'),
      ], cold()),
      F.section('Clock', [
        F.row('clock', F.select(A, 'set-clock', sp.clock || 'wall', [['wall', 'wall · real time'], ['virtual', 'virtual · exact replay']], (v) => setK(['spec', 'clock'], v === 'wall' ? undefined : v))),
        sp.clock === 'virtual' && vmGuests ? F.note(`${vmGuests} devices are at vm or hardware fidelity: a virtual clock is refused (D22).`, '#ec7575') : null,
        sp.clock === 'virtual' && (sp.stacks || []).length ? F.note('A stack is installed: a virtual clock is refused (D22).', '#ec7575') : null,
        F.row('time scale', F.number(A, 'set-ts', sp.timeScale, (v) => setK(['spec', 'timeScale'], v), { step: 0.1, placeholder: '1' }), 'how fast the model clock runs against the wall'),
        F.row('initial state', F.select(A, 'set-init', sp.initialState || 'on', [['on', 'on · every device powered at up'], ['off', 'off · BMCs on standby']], (v) => setK(['spec', 'initialState'], v === 'on' ? undefined : v))),
        F.row('fidelity', F.select(A, 'set-fid', (sp.fidelity || {}).default || 'model', LEVELS, (v) => setK(['spec', 'fidelity', 'default'], v === 'model' ? undefined : v)), 'the default level; overrides raise or lower it'),
      ], cold()),
      F.section('Packs', [
        F.chips(cat.map((p) => { const pin = `${p.id}@${p.version}`, on = packs.some((x) => x === pin || x.split('@')[0] === p.id); return [p.id, on, `${p.id}@${p.version}`]; }), (id, on) => {
          const p = cat.find((x) => x.id === id), pin = `${p.id}@${p.version}`;
          setK(['spec', 'packs'], on ? [...packs, pin] : packs.filter((x) => x.split('@')[0] !== id));
        }),
        packs.filter((x) => !cat.some((p) => p.id === x.split('@')[0])).length ? F.note(`not in this twin's registry: ${packs.filter((x) => !cat.some((p) => p.id === x.split('@')[0])).join(', ')}`, '#ec7575') : null,
        F.note('Only formats, profiles and templates of pinned packs can be used; the Library dims the rest.'),
      ], cold()),
      F.section('Adapters', [
        leaves(sp.adapters, []).length ? leaves(sp.adapters, []).map(([p, v]) => F.row(p.join('.'), F.text(A, 'set-ad-' + p.join('.'), v, (x) => setK(['spec', 'adapters', ...p], typeof v === 'number' ? Number(x) : typeof v === 'boolean' ? x === 'true' : x)))) : F.note('No adapter options: every adapter runs with its defaults.'),
        F.row('vm memory MB', F.number(A, 'set-ad-vm-mem', ((sp.adapters || {}).vm || {}).memoryMB, (v) => setK(['spec', 'adapters', 'vm', 'memoryMB'], v)), 'scale every vm guest down to this'),
        F.row('vm cpus', F.number(A, 'set-ad-vm-cpu', ((sp.adapters || {}).vm || {}).cpus, (v) => setK(['spec', 'adapters', 'vm', 'cpus'], v))),
      ], cold()));
  }

  // ---------------------------------------------------------------- the library inspector
  function inspectLib(A, c, sel) {
    const x = (c.catalogue[sel.lib] || []).find((y) => y.name === sel.name);
    if (!x) return h('div', { style: 'padding:14px' }, F.note(`${sel.name} is not in the catalogue.`));
    const isNot = (x.isNot || []).length ? F.section('Not modelled', (x.isNot || []).map((n, i) => F.note(typeof n === 'string' ? n : JSON.stringify(n), '#757575'))) : null;
    const pinned = x.pinned === false ? F.note('Its pack is not pinned by this estate: pin it in Settings to use it.', '#d1b854') : null;
    if (sel.lib === 'formats') {
      const used = Q.racks(c.tree).filter((r) => r.rack.format === x.name).map((r) => r.rack.name);
      return h('div', null, Z.head(x.name, `rack format · ${x.archetype}`), pinned,
        F.section('Format', [F.kv('height', `${x.layout.height}U${(x.layout.reserved || []).length ? ` · ${(x.layout.reserved || []).length} reserved ranges` : ''}`), F.kv('power', `${Math.round((x.power.capacityW || 0) / 1000)} kW · ${(x.power.inputs || []).map((i) => `${i.name} ${i.kind}${i.redundancy ? ' ' + i.redundancy : ''}`).join(', ') || '—'}`),
          F.kv('cooling', (x.cooling && x.cooling.kind) || '—'), F.kv('domains', (x.domains || []).map((d) => `${d.kind}${d.technology ? ' ' + d.technology : ''}`).join(', ') || '—'), F.kv('in this draft', used.join(' ') || 'none')]),
        F.section('Parameters', Object.entries(x.params || {}).map(([k, d]) => F.kv(k, [d.type, d.range ? `${d.range[0]}–${d.range[1]}` : '', d.allowed ? d.allowed.join('|') : '', d.default !== undefined ? `default ${d.default}` : ''].filter(Boolean).join(' · ')))),
        F.section('Slots', (x.slots || []).map((s, i) => h('div', { key: i, style: `font:10.5px ${MONO};color:#b3b3b3;line-height:1.5` }, `${s.names}${s.profile ? ' · ' + s.profile : ''}${s.when ? ' · when ' + s.when : ''}${s.accepts ? ' · accepts ' + Object.entries(s.accepts).map(([a, b]) => `${a} ${b}`).join(', ') : ''}`))),
        h('div', { style: 'padding:10px 12px' }, F.btn(A.S.dz.armed === x.name ? 'Disarm' : 'Arm: click an empty position to add one', () => A.set({ dz: { ...A.S.dz, armed: A.S.dz.armed === x.name ? null : x.name, view: 'floor' } }), 'primary')),
        isNot);
    }
    if (sel.lib === 'profiles') return h('div', null, Z.head(x.name, `device profile · ${x.class}`), pinned,
      F.section('Device', [F.kv('form', `${x.form && x.form.units ? x.form.units + 'U' : ''} ${(x.form && x.form.mounting) || ''}`), F.kv('power', `${(x.power || {}).idleW} W idle · ${(x.power || {}).maxW} W max`), F.kv('cooling', x.cooling || '—'),
        F.kv('ports', (x.ports || []).map((p) => `${p.count}× ${p.kind}${p.role ? ' ' + p.role : ''}${p.speed ? ' ' + p.speed : ''}`).join(' · ') || '—'),
        F.kv('components', (x.components || []).map((k) => `${k.count || 1}× ${k.kind}${k.model ? ' ' + k.model : ''}`).join(' · ') || '—'),
        F.kv('fidelity', (x.fidelity || []).join(', ') + (x.vm ? ` · vm ${x.vm.arch}, ${(x.vm.cost || {}).vcpu || '?'} vCPU, ${(x.vm.cost || {}).ramGB || '?'} GB` : '')),
        x.dialect ? F.kv('BMC', x.dialect) : null, x.manufacturer ? F.kv('made by', `${x.manufacturer} ${x.model || ''}`) : null, (x.provides || []).length ? F.kv('provides', x.provides.join(', ')) : null]), isNot);
    const fabs = Q.fabrics(c.tree).filter((f) => f.template === x.name || (c.catalogue.templateAliases || {})[f.template] === x.name).map((f) => f.name);
    return h('div', null, Z.head(x.name, `fabric template · ${x.generator}`), pinned,
      F.section('Template', [F.kv('generator', x.generator), F.kv('tiers', ((x.layout || {}).tiers || []).map((t) => t.name || JSON.stringify(t)).join(' → ') || '—'), F.kv('in this draft', fabs.join(', ') || 'none')]),
      F.section('Parameters', Object.entries(x.params || {}).map(([k, d]) => F.kv(k, [d.type, d.range ? `${d.range[0]}–${d.range[1]}` : '', d.allowed ? d.allowed.join('|') : '', d.default !== undefined ? `default ${d.default}` : ''].filter(Boolean).join(' · ')))),
      h('div', { style: 'padding:10px 12px' }, F.btn('New fabric from this template', () => c.select({ kind: 'new', what: 'fabric', template: x.name }), 'primary')), isNot);
  }

  // ---------------------------------------------------------------- halls and rows
  function newHall(A, c) {
    const ed = A.S.dz.edit, used = new Set(Q.halls(c.tree).map((x) => x.name));
    let n = 'hall-b'; for (let i = 0; used.has(n); i++) n = 'hall-' + String.fromCharCode(98 + i + 1);
    const name = ed['nh-name'] ?? n;
    return h('div', null, Z.head('New hall', `in ${Q.site(c.tree).name}`),
      F.section('Hall', [F.row('name', h('input', { className: 'dz-in', style: F.INPUT, value: name, onInput: (e) => { ed['nh-name'] = e.target.value; } })),
        F.row('first row', h('input', { className: 'dz-in', style: F.INPUT, value: ed['nh-row'] ?? 'a', onInput: (e) => { ed['nh-row'] = e.target.value; } })),
        F.row('positions', h('input', { className: 'dz-in', type: 'number', min: 1, max: 16, style: F.INPUT, value: ed['nh-pos'] ?? '8', onInput: (e) => { ed['nh-pos'] = e.target.value; } })),
        F.note('Rack names are unique in the estate: start the new hall\'s rows with letters the other halls do not use.'),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add hall', () => {
          const hn = String(ed['nh-name'] ?? n).trim(), rn = String(ed['nh-row'] ?? 'a').trim(), pos = Math.min(16, Math.max(1, Number(ed['nh-pos'] ?? 8)));
          if (!hn) return;
          c.push({ op: 'hall.add', name: hn }, ...(rn ? [{ op: 'row.add', hall: hn, name: rn, positions: pos }] : []));
          for (const f of ['nh-name', 'nh-row', 'nh-pos']) delete ed[f];
          A.set({ dz: { ...A.S.dz, hall: hn, view: 'floor', sel: { kind: 'hall', hall: hn } } });
        }, 'primary'))]));
  }
  function newRow(A, c, sel) {
    const ed = A.S.dz.edit, hall = Q.hall(c.tree, sel.hall);
    const used = new Set(Q.halls(c.tree).flatMap((hl) => (hl.rows || []).map((r) => r.name)));
    let n = 'a'; for (let i = 0; used.has(n) && i < 26; i++) n = String.fromCharCode(97 + i + 1);
    const name = ed['nr-name'] ?? n;
    return h('div', null, Z.head('New row', `in ${hall.name}`),
      F.section('Row', [F.row('name', h('input', { className: 'dz-in', style: F.INPUT, value: name, onInput: (e) => { ed['nr-name'] = e.target.value; } }), 'rack names are made from it: row d gives d01, d02…'),
        F.row('positions', h('input', { className: 'dz-in', type: 'number', min: 1, max: 16, style: F.INPUT, value: ed['nr-pos'] ?? '8', onInput: (e) => { ed['nr-pos'] = e.target.value; } }), 'at most 16'),
        h('div', { style: 'display:flex;justify-content:flex-end' }, F.btn('Add row', () => {
          const rn = String(ed['nr-name'] ?? n).trim(), pos = Math.min(16, Math.max(1, Number(ed['nr-pos'] ?? 8)));
          if (!rn) return;
          c.push({ op: 'row.add', hall: hall.name, name: rn, positions: pos });
          delete ed['nr-name']; delete ed['nr-pos'];
          c.select({ kind: 'row', hall: hall.name, row: rn });
        }, 'primary'))]));
  }

  function inspectNew(A, c, sel) {
    const make = (Z.creators || {})[sel.what];
    return make ? make(A, c, sel) : h('div', { style: 'padding:14px' }, F.note(`Nothing creates a ${sel.what} yet.`));
  }

  Object.assign(Z.inspectors, { override: inspectOverride, cabling: inspectCabling, stacks: inspectStacks, settings: inspectSettings, lib: inspectLib, new: inspectNew });
  Z.creators = Object.assign(Z.creators || {}, { override: newOverride, hall: newHall, row: newRow });
  Z.matchPath = matchPath;
})();
