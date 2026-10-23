// app.js — the console's state, its source (the live twin when the page has the estate token and
// the control API answers on this origin, the in-memory demo otherwise) and the render loop.
// Parameters: #token= or ?token= (live), ?view=2d|iso, ?speed=1|2|4 and ?stack=off (demo only).
window.TC = window.TC || {};
(async () => {
  const { patch } = TC.h;
  const V = TC.views;
  const q = new URLSearchParams(location.search);
  const root = document.getElementById('app');

  // The estate token: from the address (#token= never reaches the server, so no proxy logs it),
  // else this tab's session. Once read it is taken out of the address bar and kept for the tab.
  const session = (k, v) => { try { if (v === undefined) return sessionStorage.getItem(k); sessionStorage.setItem(k, v); } catch (e) { return null; } };
  const hash = new URLSearchParams(location.hash.slice(1));
  const given = hash.get('token') || q.get('token');
  const token = given || session('dctwin-token');
  if (given) {
    session('dctwin-token', given); hash.delete('token'); q.delete('token');
    const qs = String(q), hs = String(hash);
    history.replaceState(null, '', location.pathname + (qs ? '?' + qs : '') + (hs ? '#' + hs : ''));
  }
  // a #token= link opened in a tab already on this page changes only the fragment: start again on it
  window.addEventListener('hashchange', () => { if (new URLSearchParams(location.hash.slice(1)).get('token')) location.reload(); });

  let src = null;
  if (token) {
    const live = new TC.Live(token);
    if (await live.connect()) src = live;
    else console.warn('live mode: the control API did not answer with this token');
  }
  // a twin serves this page (its API answers, and wants a token): ask for the token, not the demo
  if (!src && await fetch('/v1/status', { cache: 'no-store' }).then((r) => r.status === 401, () => false)) {
    askToken(root, !!token, (t) => { session('dctwin-token', t); location.reload(); });
    return;
  }
  if (!src) src = new TC.Demo({ speed: Number(q.get('speed') || 1), stackResponds: q.get('stack') !== 'off' });

  const first = src.defaultSelection();
  const S = {
    screen: 'cockpit', view: q.get('view') === '2d' ? '2d' : 'iso', overlay: 'health', selRack: first.rack, selDev: first.dev, tab: 'overview', comp: 'gpu-2', faultId: null, target: 'device', when: 'now', params: {},
    kinds: { fault: 1, state: 1, request: 1, stack: 1, kernel: 1, bms: 1, nos: 1, act: 1, twin: 1, read: 0 }, evScope: 'all', follow: true,
    scenSel: 'nvl72-leak-response', msg: '', draft: [], designSel: null, designFmt: src.designFormats()[0][0], applyLog: null, fabric: null,
    layers: { power: 1, cooling: 1 }, animate: true, floorMax: false, fw: 900, fh: 400, rx: 58, rz: -32, zoom: 1, tx: 0, ty: 0, px: 0, py: 0, dragging: false, infraSel: null,
    // the designer (Design on a live twin): its selection, view, panels and typed-but-uncommitted fields
    dz: { edit: {}, sel: null, view: 'floor', left: 'explorer', expanded: {}, libKind: 'formats', draftTab: 'changes', pick: null, armed: null },
  };

  let evEl = null, ro = null, dragMoved = false, queued = false, msgT = null, twoFinger = 0;
  const rad = (d) => (d * Math.PI) / 180;
  // The floor point under a screen offset (pixels from the view's centre), as an offset from the
  // orbit target: the screen ray through it, met with the floor plane, through the camera's
  // perspective (2400 px, from the centre), scale, tilt and turn. Exact, so a grabbed point stays
  // under the cursor and a zoom keeps the point under the cursor where it is.
  const PERSPECTIVE = 2400;
  function screenToWorld(sx, sy, zoom) {
    const s = ((A._cam && A._cam.fit) || 1) * (zoom ?? S.zoom), th = rad(S.rx), a = rad(S.rz), P = PERSPECTIVE;
    // forward: r = rotateZ(u); v = (s·r.x, s·r.y·cos th, r.y·sin th) (CSS scale() leaves depth
    // alone); screen = v.xy · P / (P − v.z)
    const den = s * P * Math.cos(th) + sy * Math.sin(th);
    const ry = (sy * P) / Math.max(s * P * 0.05, den); // above the horizon the ray misses the floor
    const k = P / Math.max(P * 0.05, P - ry * Math.sin(th)), rx = sx / (s * k);
    return { x: rx * Math.cos(a) + ry * Math.sin(a), y: -rx * Math.sin(a) + ry * Math.cos(a) };
  }
  // where the pointer is, relative to the view's centre
  function fromCentre(e) {
    const r = A._camEl ? A._camEl.parentElement.getBoundingClientRect() : { left: 0, top: 0, width: 0, height: 0 };
    return [e.clientX - (r.left + r.width / 2), e.clientY - (r.top + r.height / 2)];
  }
  // ---------------------------------------------------------------- the camera
  // S holds the camera as drawn (rx tilt, rz turn, zoom, tx/ty orbit target offset). A gesture
  // sets a goal; one animation loop eases the drawn camera toward it, so orbit and zoom glide
  // instead of jumping with every mouse event, and an orbit released while moving coasts to a
  // stop. Pan is not eased: the floor point grabbed stays exactly under the cursor. Nothing but
  // the camera's own transform (and the pivot marker) changes while it moves; the page renders
  // once when it settles.
  const cam = { goal: null, raf: 0, last: 0, vz: 0, vx: 0, anchor: null, samples: [] };
  const clampRx = (v) => Math.max(0, Math.min(80, v));
  function goal() { if (!cam.goal) cam.goal = { rx: S.rx, rz: S.rz, zoom: S.zoom, tx: S.tx, ty: S.ty }; return cam.goal; }
  function applyCam() {
    if (S.view === 'iso' && A._camEl && A._cam) {
      A._camEl.style.transform = V.camTransform(S, A._cam);
    }
    if (S.view !== 'iso' && A._gridEl) { const z = A._zoom2d || 1; A._gridEl.style.transform = `translate(${(S.px / z).toFixed(1)}px,${(S.py / z).toFixed(1)}px)`; }
  }
  function kick() { if (!cam.raf) { cam.last = performance.now(); cam.raf = requestAnimationFrame(step); } }
  function step(now) {
    cam.raf = 0;
    const dt = Math.max(1, Math.min(50, now - cam.last)); cam.last = now;
    const g = cam.goal;
    if (!g) { applyCam(); return; }
    // coasting after an orbit release: the goal keeps turning, slowing down
    if (S.dragging !== 'orbit' && (Math.abs(cam.vz) > 0.002 || Math.abs(cam.vx) > 0.002)) {
      g.rz += cam.vz * dt; g.rx = clampRx(g.rx + cam.vx * dt);
      const f = Math.exp(-dt / 150); // a flick coasts at most about 30° cam.vz *= f; cam.vx *= f;
    } else if (S.dragging !== 'orbit') { cam.vz = cam.vx = 0; }
    const ease = (tau) => 1 - Math.exp(-dt / tau);
    const ao = ease(55), az = ease(85), at = ease(120);
    S.rx += (g.rx - S.rx) * ao; S.rz += (g.rz - S.rz) * ao;
    const z0 = S.zoom; S.zoom += (g.zoom - S.zoom) * az;
    if (cam.anchor && S.zoom !== z0) {
      // keep the floor point under the cursor where it is while the zoom eases in
      const [cx, cy] = cam.anchor, w0 = screenToWorld(cx, cy, z0), w1 = screenToWorld(cx, cy, S.zoom);
      S.tx += w0.x - w1.x; S.ty += w0.y - w1.y; g.tx += w0.x - w1.x; g.ty += w0.y - w1.y;
    }
    if (S.dragging !== 'pan') { S.tx += (g.tx - S.tx) * at; S.ty += (g.ty - S.ty) * at; }
    applyCam();
    const settled = Math.abs(g.rx - S.rx) < 0.01 && Math.abs(g.rz - S.rz) < 0.01 && Math.abs(g.zoom - S.zoom) < 0.0005 && Math.abs(g.tx - S.tx) < 0.05 && Math.abs(g.ty - S.ty) < 0.05 && !cam.vz && !cam.vx;
    if (settled && (S.dragging === 'coast' || !S.dragging)) { Object.assign(S, g); cam.goal = null; cam.anchor = null; applyCam(); gestureEnd(); return; }
    kick();
  }
  function stopCam() {
    if (cam.raf) cancelAnimationFrame(cam.raf);
    cam.raf = 0; cam.goal = null; cam.anchor = null; cam.vz = cam.vx = 0;
  }
  function gestureStart(kind) {
    S.dragging = kind;
    if (A._camEl) A._camEl.style.transition = 'none';
    root.classList.add('tc-moving'); // flows and blinking LEDs hold still; the pivot marker shows
  }
  function gestureEnd() { root.classList.remove('tc-moving'); A.set({ dragging: false }); }
  const A = {
    S, src,
    set(o) { Object.assign(S, typeof o === 'function' ? o(S) : o); A.render(); },
    render() { if (queued) return; queued = true; requestAnimationFrame(draw); },
    dragMoved: () => dragMoved,
    flash(m) { S.msg = m; A.render(); clearTimeout(msgT); msgT = setTimeout(() => { S.msg = ''; A.render(); }, 2500); },
    setKind(k, v) { S.kinds = { ...S.kinds, [k]: v }; if (src.onKinds) src.onKinds(S.kinds); A.render(); },
    selectDev(p) {
      const d = src.devs[p]; if (!d) return;
      const acc = d.comps.filter((x) => x.kind === 'accelerator');
      const list = src.faultsFor(d);
      A.set({ selDev: p, selRack: d.rack, comp: acc.length ? (acc.some((x) => x.name === S.comp) ? S.comp : acc[0].name) : S.comp, target: 'device', faultId: list[0] ? list[0].id : null, params: {} });
      if (src.onSelect) src.onSelect(p);
    },
    selectRack(n) {
      const r = src.racks[n]; if (!r) return;
      const first = r.slots.find((p) => /compute-tray-07|server-01|spine-0?1$/.test(p)) || r.slots[0];
      if (first) A.selectDev(first); else A.set({ selRack: n });
    },
    gotoEvent(e) { const t = src.eventTarget(e); if (!t) return; if (t.dev) A.selectDev(t.dev); else if (t.rack) A.selectRack(t.rack); },
    floorRef(el) {
      if (A._floorEl === el) return;
      if (ro) { ro.disconnect(); ro = null; }
      A._floorEl = el; if (!el) return;
      ro = new ResizeObserver(([e]) => { const { width, height } = e.contentRect; if (Math.abs(width - S.fw) > 4 || Math.abs(height - S.fh) > 4) A.set({ fw: width, fh: height }); });
      ro.observe(el);
    },
    evRef(el) { evEl = el; },
    camRef(el) { A._camEl = el; },
    gridRef(el) { A._gridEl = el; },
    // Drag on the floor: left-drag orbits (ISO); shift-, alt- or cmd-drag, a right or middle
    // drag, or any drag on the 2D floor pans.
    orbitStart(e) {
      const iso = S.view === 'iso';
      const pan = !iso || e.button === 1 || e.button === 2 || e.shiftKey || e.altKey || e.metaKey;
      if (!pan && e.button !== 0) return;
      if (pan) e.preventDefault();
      cam.vz = cam.vx = 0; cam.anchor = null;
      const g = iso ? goal() : null;
      if (iso && pan) { S.rx = g.rx; S.rz = g.rz; S.zoom = g.zoom; } // pan on a still camera, so the grab is exact
      const [c0x, c0y] = fromCentre(e);
      const d0 = { x: e.clientX, y: e.clientY, rx: g ? g.rx : S.rx, rz: g ? g.rz : S.rz, tx: S.tx, ty: S.ty, px: S.px, py: S.py, gp: iso ? screenToWorld(c0x, c0y) : null }; dragMoved = false;
      cam.samples = [];
      const mv = (ev) => {
        const dx = ev.clientX - d0.x, dy = ev.clientY - d0.y;
        if (!dragMoved) { if (Math.abs(dx) + Math.abs(dy) <= 4) return; dragMoved = true; gestureStart(pan ? 'pan' : 'orbit'); }
        if (!iso) { S.px = d0.px + dx; S.py = d0.py + dy; applyCam(); return; }
        if (pan) {
          // the floor point grabbed stays under the cursor: target = grabbed − (point under cursor)
          const [cx, cy] = fromCentre(ev), w = screenToWorld(cx, cy);
          S.tx = g.tx = d0.tx + d0.gp.x - w.x; S.ty = g.ty = d0.ty + d0.gp.y - w.y;
        } else {
          // the near side of the floor follows the cursor; dragging down looks more from above
          g.rz = d0.rz - dx * 0.3; g.rx = clampRx(d0.rx - dy * 0.25);
          const t = performance.now(); cam.samples.push([t, g.rz, g.rx]); while (cam.samples.length > 2 && t - cam.samples[0][0] > 90) cam.samples.shift();
        }
        kick();
      };
      const up = () => {
        window.removeEventListener('mousemove', mv); window.removeEventListener('mouseup', up);
        if (dragMoved && iso) {
          // released while turning: coast at the pointer's speed over its last few samples
          const sm = cam.samples, last = sm[sm.length - 1];
          if (!pan && sm.length > 1 && performance.now() - last[0] < 60) { const dt = last[0] - sm[0][0] || 1, cap = (v) => Math.max(-0.2, Math.min(0.2, v)); cam.vz = cap((last[1] - sm[0][1]) / dt); cam.vx = cap((last[2] - sm[0][2]) / dt); }
          S.dragging = 'coast'; kick();
        } else if (dragMoved) gestureEnd();
        setTimeout(() => { dragMoved = false; }, 0);
      };
      window.addEventListener('mousemove', mv); window.addEventListener('mouseup', up);
    },
    noMenu(e) { e.preventDefault(); },
    // Double-click: orbit around the floor point under the cursor from now on.
    focus(e) {
      if (S.view !== 'iso' || !A._camEl) return;
      const [cx, cy] = fromCentre(e), g = goal(), w = screenToWorld(cx, cy);
      g.tx = S.tx + w.x; g.ty = S.ty + w.y;
      gestureStart('coast'); kick();
    },
    // A camera preset (or a reset): animated by the page's own transition.
    camTo(o) { stopCam(); root.classList.remove('tc-moving'); A.set({ ...o, dragging: false }); },
    // Wheel: zoom about the floor point under the cursor (a mouse wheel, or a pinch, which the
    // browser sends as a ctrl-wheel); a trackpad's two-finger drag (it carries a sideways
    // component) or shift-wheel pans.
    onWheel(e) {
      if (S.view !== 'iso' || !A._camEl) return;
      e.preventDefault();
      const unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? 400 : 1, now = performance.now();
      const dx = e.deltaX * unit, dy = e.deltaY * unit, g = goal();
      if (!e.ctrlKey && (e.shiftKey || Math.abs(dx) > 0.5 || now < twoFinger)) {
        if (Math.abs(dx) > 0.5) twoFinger = now + 300;
        const w = e.shiftKey && !dx ? screenToWorld(dy, 0) : screenToWorld(dx, dy);
        S.tx += w.x; S.ty += w.y; g.tx += w.x; g.ty += w.y;
      } else {
        g.zoom = Math.max(0.35, Math.min(4, g.zoom * Math.exp(-dy * (e.ctrlKey ? 0.01 : 0.0015))));
        cam.anchor = fromCentre(e);
      }
      if (!S.dragging) gestureStart('coast');
      kick();
    },
  };

  // the designer edits a draft of the estate file, so it needs a live twin started from one
  if (src.kind === 'live' && src.estateTree && TC.dz) A.draft = new TC.dz.Draft(src, () => A.render());

  function draw() {
    queued = false;
    const h = V.h, M = V.model(A);
    let body;
    if (S.screen === 'topology') body = V.topology(A, M);
    else if (S.screen === 'facility') body = V.facility(A, M);
    else if (S.screen === 'scenarios') body = V.scenarios(A, M);
    else if (S.screen === 'design') body = A.draft ? V.designer(A, M) : V.design(A, M);
    else body = V.cockpit(A, M);
    patch(root, h('div', { style: 'display:grid;grid-template-rows:44px minmax(0,1fr);height:100vh;background:#0a0a0a' },
      V.topBar(A, M),
      h('div', { style: 'display:grid;grid-template-columns:minmax(0,1fr);min-height:0' }, body)));
    if (S.screen !== 'cockpit' && S.screen !== 'design' && A._floorEl && !A._floorEl.isConnected) A.floorRef(null);
    if (evEl && S.follow) evEl.scrollTop = evEl.scrollHeight;
  }

  // data changes re-render the page, except during a camera gesture: then once it ends
  src.onChange = () => { if (!S.dragging) A.render(); };
  window.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (e.target && /^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName)) return;
    if (S.infraSel) A.set({ infraSel: null }); else if (S.designSel) A.set({ designSel: null });
    else if (S.dz.pick) A.set({ dz: { ...S.dz, pick: null } }); else if (S.dz.armed) A.set({ dz: { ...S.dz, armed: null } }); else if (S.dz.sel) A.set({ dz: { ...S.dz, sel: null } });
  });
  src.start();
  A.render();
  window.TC.app = A; // for debugging from the console

  // The sign-in form a twin's page shows without a valid token.
  function askToken(root, refused, done) {
    const el = (tag, props, ...kids) => { const n = Object.assign(document.createElement(tag), props); n.append(...kids); return n; };
    const input = el('input', { type: 'password', className: 'dz-in tc-signin-in', placeholder: 'estate token', autocomplete: 'off', required: true, autofocus: true });
    const form = el('form', { className: 'tc-signin' },
      el('img', { src: 'img/bud-gpu-foundry-white.png', alt: 'Bud GPU Foundry', className: 'tc-signin-logo' }),
      el('div', { className: 'tc-signin-title' }, 'dc-twin'),
      el('p', {}, refused ? 'That token was refused. Enter the estate token.' : 'This twin needs its estate token.'),
      input, el('button', { type: 'submit', className: 'ds-btn ds-primary' }, 'Connect'),
      el('p', { className: 'tc-signin-hint' }, 'dctwin ui, where the twin runs, prints the console address with it. In Kubernetes, the chart keeps it in a Secret.'));
    form.onsubmit = (e) => { e.preventDefault(); if (input.value.trim()) done(input.value.trim()); };
    root.replaceChildren(form);
    input.focus();
  }
})();
