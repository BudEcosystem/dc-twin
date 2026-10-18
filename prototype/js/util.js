// util.js — small helpers shared by the prototype. No dependencies.
window.DCT = window.DCT || {};
DCT.util = (() => {
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  function hash(str) {
    let h = 2166136261;
    for (let i = 0; i < str.length; i++) { h ^= str.charCodeAt(i); h = Math.imul(h, 16777619); }
    return h >>> 0;
  }

  // Deterministic PRNG (mulberry32) seeded by a string or number. Identities derive from it (03 — Identity).
  function rng(seed) {
    let a = typeof seed === 'string' ? hash(seed) : seed >>> 0;
    return function () {
      a |= 0; a = (a + 0x6D2B79F5) | 0;
      let t = Math.imul(a ^ (a >>> 15), 1 | a);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }

  function pick(r, arr) { return arr[Math.floor(r() * arr.length)]; }
  function hex(r, n) { let s = ''; for (let i = 0; i < n; i++) s += '0123456789abcdef'[Math.floor(r() * 16)]; return s; }
  function digits(r, n) { let s = ''; for (let i = 0; i < n; i++) s += Math.floor(r() * 10); return s; }
  function mac(r, prefix) { const b = []; for (let i = 0; i < 3; i++) b.push(hex(r, 2)); return prefix + ':' + b.join(':'); }
  function uuid(r) { const h = hex(r, 32); return `${h.slice(0, 8)}-${h.slice(8, 12)}-4${h.slice(13, 16)}-a${h.slice(17, 20)}-${h.slice(20, 32)}`; }

  // Brace expansion: {a,b} and {01..18}. Also `$param` substitution happens before this in the compiler.
  function expandBraces(s) {
    const m = s.match(/\{([^{}]*)\}/);
    if (!m) return [s];
    const [full, inner] = m;
    let parts;
    const r = inner.match(/^(\d+)\.\.(\d+)$/);
    if (r) {
      const a = parseInt(r[1], 10), b = parseInt(r[2], 10), w = r[1].length;
      parts = [];
      for (let i = a; i <= b; i++) parts.push(String(i).padStart(w, '0'));
    } else parts = inner.split(',');
    return parts.flatMap((p) => expandBraces(s.replace(full, p)));
  }

  // Selector globs (03 — Paths and selectors): `*` one segment, `**` any depth. A selector without the
  // site prefix matches from any segment boundary, so `a03/**` matches `site-1/hall-a/a03/compute-tray-07`.
  function globToRegex(g) {
    let re = '';
    for (let i = 0; i < g.length; i++) {
      const c = g[i];
      if (c === '*') { if (g[i + 1] === '*') { re += '.*'; i++; } else re += '[^/]*'; }
      else if ('.+?^$()[]|\\'.includes(c)) re += '\\' + c;
      else re += c;
    }
    return new RegExp('(^|/)' + re + '$');
  }
  function matchSelector(sel, path) {
    if (!sel || !sel.trim()) return true;
    return sel.split(/\s+/).some((part) => expandBraces(part).some((g) => globToRegex(g).test(path)));
  }

  const pad2 = (n) => String(n).padStart(2, '0');
  function fmtClock(d) { return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`; }
  function fmtKW(w) { return w >= 1000 ? (w / 1000).toFixed(w >= 100000 ? 0 : 1) + ' kW' : Math.round(w) + ' W'; }
  function fmtDur(s) { s = Math.max(0, Math.round(s)); if (s < 60) return s + ' s'; const m = Math.floor(s / 60); return m < 60 ? `${m} m ${s % 60} s` : `${Math.floor(m / 60)} h ${m % 60} m`; }

  const SEV_ORDER = { OK: 0, Info: 0, Warning: 1, Critical: 2, Unknown: 1 };
  function maxSeverity(list) { let best = 'OK'; for (const s of list) if ((SEV_ORDER[s] ?? 0) > (SEV_ORDER[best] ?? 0)) best = s; return best; }
  // Colour is paired with a shape so severity never relies on colour alone (12 — Rules, 7).
  const SEV_MARK = { OK: '●', Info: '●', Warning: '◆', Critical: '▲', Unknown: '?' };
  const sevClass = (s) => ({ OK: 'nominal', Info: 'info', Warning: 'warn', Critical: 'crit', Unknown: 'unknown' }[s] || 'nominal');
  function sevHTML(s, label) { return `<span class="sev ${sevClass(s)}" title="${esc(s)}"><span class="mark">${SEV_MARK[s] || '●'}</span>${label ? ' ' + esc(label) : ''}</span>`; }
  function fidBadge(f) { return `<span class="fid fid-${esc(f)}" title="fidelity: ${esc(f)}">${esc(f)}</span>`; }

  function lastSeg(p) { return p.slice(p.lastIndexOf('/') + 1); }
  function parentPath(p) { return p.slice(0, p.lastIndexOf('/')); }
  function shortPath(p) { // drop the site/hall prefix for display
    const segs = p.split('/'); return segs.length > 2 ? segs.slice(2).join('/') : p;
  }

  function download(name, text, type = 'text/yaml') {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([text], { type }));
    a.download = name; a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }

  function store(key, val) { try { if (val === undefined) return localStorage.getItem(key); localStorage.setItem(key, val); } catch (e) { return null; } }

  return { esc, hash, rng, pick, hex, digits, mac, uuid, expandBraces, matchSelector, fmtClock, fmtKW, fmtDur, maxSeverity, sevHTML, sevClass, fidBadge, lastSeg, parentPath, shortPath, download, store, SEV_MARK };
})();
