// draft.js — the designer's draft: a list of edits (ops) to the estate file, with undo and redo,
// planned by the twin after every change (POST /v1/estate/plan). The twin is the truth: it applies
// the ops to the file, compiles the result and returns problems, warnings, the diff, the impact,
// the compiled layout and the edited file as a tree. The draft is kept per estate and file etag in
// this browser's storage, so a reload does not lose it.
window.TC = window.TC || {};
(() => {
  // ---------------------------------------------------------------- paths into the estate
  const named = (name) => ({ name });
  const P = {
    spec: (...p) => ['spec', ...p],
    site: (tree) => ['spec', 'sites', named(((tree.spec.sites || [])[0] || {}).name)],
    hall: (tree, hall) => [...P.site(tree), 'halls', named(hall)],
    row: (tree, hall, row) => [...P.hall(tree, hall), 'rows', named(row)],
    rack: (tree, hall, row, rack) => [...P.row(tree, hall, row), 'racks', named(rack)],
    fabric: (name) => ['spec', 'fabrics', named(name)],
    network: (name) => ['spec', 'networks', name],
    feed: (name) => ['spec', 'facility', 'power', 'feeds', named(name)],
    node: (name) => ['spec', 'facility', 'power', 'topology', named(name)],
    loop: (name) => ['spec', 'facility', 'cooling', 'loops', named(name)],
    fdev: (name) => ['spec', 'facility', 'devices', named(name)],
    override: (i) => ['spec', 'fidelity', 'overrides', i],
    stack: (name) => ['spec', 'stacks', named(name)],
  };

  // ---------------------------------------------------------------- reading the tree
  const Q = {
    spec: (t) => (t && t.spec) || {},
    site: (t) => (Q.spec(t).sites || [])[0] || { halls: [] },
    halls: (t) => Q.site(t).halls || [],
    hall: (t, name) => Q.halls(t).find((h) => h.name === name) || Q.halls(t)[0] || { rows: [] },
    racks: (t) => { const out = []; for (const h of Q.halls(t)) for (const r of h.rows || []) for (const k of r.racks || []) out.push({ hall: h.name, row: r.name, rack: k }); return out; },
    rack: (t, name) => Q.racks(t).find((x) => x.rack.name === name) || null,
    fabrics: (t) => Q.spec(t).fabrics || [],
    networks: (t) => Q.spec(t).networks || {},
    facility: (t) => Q.spec(t).facility || {},
    feeds: (t) => ((Q.facility(t).power || {}).feeds) || [],
    topology: (t) => ((Q.facility(t).power || {}).topology) || [],
    loops: (t) => ((Q.facility(t).cooling || {}).loops) || [],
    fdevs: (t) => Q.facility(t).devices || [],
    overrides: (t) => ((Q.spec(t).fidelity || {}).overrides) || [],
    cabling: (t) => Q.spec(t).cabling || {},
    stacks: (t) => Q.spec(t).stacks || [],
    get(t, path) {
      let n = t;
      for (const s of path) {
        if (n == null) return undefined;
        if (s && typeof s === 'object') n = Array.isArray(n) ? n.find((x) => x && x.name === s.name) : undefined;
        else n = n[s];
      }
      return n;
    },
  };

  // A name not yet used by any rack (rack names are unique in a hall; the designer keeps them
  // unique in the estate).
  function freeRackName(tree, base) {
    const used = new Set(Q.racks(tree).map((x) => x.rack.name));
    if (!used.has(base)) return base;
    for (let i = 2; ; i++) if (!used.has(`${base}-${i}`)) return `${base}-${i}`;
  }

  // ---------------------------------------------------------------- the draft
  class Draft {
    constructor(src, onChange) {
      this.src = src; this.onChange = onChange;
      this.ops = []; this.redoStack = []; this.plan = null; this.planning = false; this.good = null; this.basePlan = null; this.n = 0;
      this.loadBase(); this.restore();
    }
    key() { return `tc.draft.${this.src.estate.name}.${this.src.etag}`; }
    save() { try { if (this.ops.length) localStorage.setItem(this.key(), JSON.stringify(this.ops)); else localStorage.removeItem(this.key()); } catch (e) { /* storage may be unavailable */ } }
    restore() {
      try { const v = JSON.parse(localStorage.getItem(this.key()) || '[]'); if (Array.isArray(v) && v.length) { this.ops = v; this.replan(); } } catch (e) { /* ignore */ }
    }
    // the estate as the draft has it (the base file until the first plan answers)
    tree() { return (this.plan && this.plan.tree) || this.src.estateTree; }
    base() { return this.src.estateTree; }
    // the running file planned with no ops: the impact and layout before any change
    loadBase() { this.src.planOps([]).then((p) => { if (p && p.ok) { this.basePlan = p; this.onChange(); } }); }
    // the compiled layout to draw: the draft's when it compiles, else the last one that did
    layout() { return (this.plan && this.plan.layout) || (this.good && this.good.layout) || (this.basePlan && this.basePlan.layout) || this.src.layout; }
    // power, cooling, fabric and VM numbers of the draft (or of the file, with no changes)
    impact() { return (this.plan && this.plan.impact) || (this.good && this.good.impact) || (this.basePlan && this.basePlan.impact) || null; }
    push(...ops) { this.ops.push(...ops.flat()); this.redoStack = []; this.changed(); }
    undo() { if (!this.ops.length) return; this.redoStack.push(this.ops.pop()); this.changed(); }
    redo() { if (!this.redoStack.length) return; this.ops.push(this.redoStack.pop()); this.changed(); }
    removeAt(i) { this.ops.splice(i, 1); this.redoStack = []; this.changed(); }
    clear() { this.ops = []; this.redoStack = []; this.plan = null; this.changed(); }
    changed() { this.save(); this.replan(); this.onChange(); }
    rebase() { this.plan = null; this.good = null; this.basePlan = null; this.loadBase(); this.restore(); this.onChange(); }
    replan() {
      clearTimeout(this.t);
      if (!this.ops.length) { this.plan = null; this.planning = false; return; }
      this.planning = true;
      const n = ++this.n, ops = this.ops.slice();
      this.t = setTimeout(async () => {
        const p = await this.src.planOps(ops);
        if (n !== this.n) return; // a newer change is being planned
        this.plan = p; this.planning = false;
        if (p && p.ok) this.good = p;
        this.onChange();
      }, 220);
    }
    // what an op does, in words, for the change list
    describe(o) {
      const p = (o.path || []).map((s) => (s && typeof s === 'object' ? s.name : s));
      const last = p[p.length - 1], where = p.slice(1, -1).filter((x) => !['sites', 'halls', 'rows', 'racks', 'site-1'].includes(x)).join(' › ');
      const v = (x) => (x === undefined ? '' : typeof x === 'object' ? JSON.stringify(x) : String(x));
      switch (o.op) {
        case 'rack.add': return `add rack ${o.name} (${o.format}) at ${o.row}${String(o.position).padStart(2, '0')}`;
        case 'rack.remove': return `remove rack ${o.name}`;
        case 'rack.move': return `move rack ${o.name} to row ${o.toRow || '·'} position ${o.toPosition || '·'}`;
        case 'rack.rename': return `rename rack ${o.name} → ${o.newName}`;
        case 'row.add': return `add row ${o.name} (${o.positions} positions)`;
        case 'row.remove': return `remove row ${o.name} and its racks`;
        case 'hall.add': return `add hall ${o.name}`;
        case 'hall.remove': return `remove hall ${o.name}`;
        case 'set': return `${where ? where + ' › ' : ''}${last} = ${v(o.value)}`;
        case 'add': return `add ${where ? where + ' › ' : ''}${last === '-' ? v(o.value && o.value.name) || 'item' : last}`;
        case 'remove': return `remove ${where ? where + ' › ' : ''}${last}`;
        case 'move': return `move ${p.join(' › ')}`;
      }
      return o.op;
    }
  }

  // ---------------------------------------------------------------- selectors, client side
  // Enough of the selector grammar to preview which racks a fabric or override would select:
  // whitespace-separated alternatives, brace lists and ranges, and * within a segment, matched as
  // a suffix on the rack's path. The twin's compile is the truth.
  function expand(s) {
    const i = s.indexOf('{'); if (i < 0) return [s];
    let depth = 0, j = i;
    for (; j < s.length; j++) { if (s[j] === '{') depth++; if (s[j] === '}' && --depth === 0) break; }
    if (j >= s.length) return [s];
    const body = s.slice(i + 1, j), head = s.slice(0, i), tail = s.slice(j + 1);
    let alts = [];
    const m = /^(-?\d+)\.\.(-?\d+)$/.exec(body);
    if (m) {
      const a = +m[1], b = +m[2], w = /^0\d/.test(m[1]) || /^0\d/.test(m[2]) ? Math.max(m[1].length, m[2].length) : 0;
      for (let k = Math.min(a, b); k <= Math.max(a, b); k++) alts.push(String(k).padStart(w, '0'));
    } else {
      depth = 0; let cur = '';
      for (const ch of body) { if (ch === '{') depth++; if (ch === '}') depth--; if (ch === ',' && !depth) { alts.push(cur); cur = ''; } else cur += ch; }
      alts.push(cur); alts = alts.flatMap(expand);
    }
    return alts.flatMap((a) => expand(head + a + tail));
  }
  const glob = (pat, s) => new RegExp('^' + pat.split('*').map((x) => x.replace(/[.+?^${}()|[\]\\]/g, '\\$&')).join('.*') + '$').test(s);
  function selectRacks(sel, racks) {
    const pats = String(sel || '').trim().split(/\s+/).filter((x) => x && !x.startsWith('?')).flatMap(expand);
    if (!pats.length) return racks.slice();
    return racks.filter((r) => pats.some((p) => { const seg = p.split('/').filter((x) => x && x !== '**'); return seg.length && glob(seg[seg.length - 1], r); }));
  }
  // A brace list for a set of rack names: compact, and read back by selectRacks.
  function braceList(names) {
    const ns = [...new Set(names)].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
    if (!ns.length) return '';
    if (ns.length === 1) return ns[0];
    return `{${ns.join(',')}}`;
  }

  TC.dz = { P, Q, Draft, freeRackName, selectRacks, braceList, expand };
})();
