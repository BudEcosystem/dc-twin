// h.js — a small virtual DOM: h() builds a tree, patch() brings the page in line with it.
// The console re-renders once a second; diffing (rather than innerHTML) keeps CSS animations,
// transitions, scroll positions and focus intact between renders. Styles are strings, as the
// design writes them, and are only touched when they change.
window.TC = window.TC || {};
TC.h = (() => {
  const SVG = 'http://www.w3.org/2000/svg';
  const TEXT = '#text';

  function flat(out, c) {
    if (c === null || c === undefined || c === false || c === true) return out;
    if (Array.isArray(c)) { for (const x of c) flat(out, x); return out; }
    if (typeof c === 'object' && c.t) { out.push(c); return out; }
    out.push({ t: TEXT, v: String(c) });
    return out;
  }
  function h(t, p, ...c) { p = p || {}; return { t, p, k: p.key, c: flat([], c) }; }

  function setProp(el, k, v, old, svg) {
    if (k === 'key' || k === 'ref') return;
    if (k === 'style') { if (v !== old) el.style.cssText = v || ''; return; }
    if (k.startsWith('on')) {
      const ev = k.slice(2).toLowerCase();
      el._h = el._h || {};
      if (!el._h[ev]) el.addEventListener(ev, (e) => el._h[ev] && el._h[ev](e), ev === 'wheel' ? { passive: false } : undefined);
      el._h[ev] = v || null;
      return;
    }
    if (k === 'value' && !svg) { if (el.value !== String(v ?? '')) el.value = v ?? ''; return; }
    if (k === 'checked' && !svg) { el.checked = !!v; return; }
    if (k === 'html') { if (v !== old) el.innerHTML = v; return; }
    if (v === null || v === undefined || v === false) el.removeAttribute(k === 'className' ? 'class' : k);
    else el.setAttribute(k === 'className' ? 'class' : k, v === true ? '' : v);
  }

  function create(v, svg) {
    if (v.t === TEXT) return (v.el = document.createTextNode(v.v));
    svg = svg || v.t === 'svg';
    const el = (v.el = svg ? document.createElementNS(SVG, v.t) : document.createElement(v.t));
    for (const k in v.p) setProp(el, k, v.p[k], undefined, svg);
    for (const c of v.c) el.appendChild(create(c, svg && v.t !== 'foreignObject'));
    if (v.p.ref) v.p.ref(el);
    return el;
  }

  function update(o, n, svg) {
    const el = (n.el = o.el);
    if (n.t === TEXT) { if (o.v !== n.v) el.nodeValue = n.v; return; }
    svg = svg || n.t === 'svg';
    for (const k in o.p) if (!(k in n.p)) setProp(el, k, null, o.p[k], svg);
    for (const k in n.p) if (k === 'value' || k === 'checked' || n.p[k] !== o.p[k]) setProp(el, k, n.p[k], o.p[k], svg);
    if (n.p.ref && n.p.ref !== o.p.ref) n.p.ref(el);
    children(el, o.c, n.c, svg && n.t !== 'foreignObject');
  }

  const same = (a, b) => a.t === b.t && a.k === b.k;

  function children(el, oc, nc, svg) {
    const keyed = nc.length && nc.some((x) => x.k !== undefined);
    if (!keyed) {
      const n = Math.max(oc.length, nc.length);
      for (let i = 0; i < n; i++) {
        const o = oc[i], v = nc[i];
        if (!v) { el.removeChild(o.el); continue; }
        if (!o) { el.appendChild(create(v, svg)); continue; }
        if (same(o, v)) update(o, v, svg); else el.replaceChild(create(v, svg), o.el);
      }
      return;
    }
    const byKey = new Map();
    oc.forEach((o, i) => byKey.set(o.k !== undefined ? 'k' + o.k : 'i' + i, o));
    const used = new Set();
    nc.forEach((v, i) => {
      const key = v.k !== undefined ? 'k' + v.k : 'i' + i;
      const o = byKey.get(key);
      if (o && same(o, v) && !used.has(o)) { used.add(o); update(o, v, svg); } else create(v, svg);
    });
    for (const o of oc) if (!used.has(o) && o.el.parentNode === el) el.removeChild(o.el);
    let ref = el.firstChild;
    for (const v of nc) { if (v.el !== ref) el.insertBefore(v.el, ref); else ref = ref.nextSibling; }
  }

  // patch renders vnode into container, diffing against what it rendered there last.
  function patch(container, vnode) {
    const old = container._v;
    if (!old) { container.textContent = ''; container.appendChild(create(vnode, false)); }
    else if (same(old, vnode)) update(old, vnode, false);
    else { const el = create(vnode, false); container.replaceChild(el, old.el); }
    container._v = vnode;
  }

  return { h, patch };
})();
