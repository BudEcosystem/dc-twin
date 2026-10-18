// views-floor.js — the Floor screen (hall plan with overlays, design-mode positions) and the shared
// events drawer (spec 12 — Floor, Events).
window.DCT = window.DCT || {};
DCT.views = DCT.views || {};
(() => {
  const U = DCT.util, esc = U.esc;
  const E = () => DCT.engine;

  const TILE = 76, GAP = 10, ROWH = 118, LABELW = 44;

  function tempRamp(t) { // 20 °C → neutral, 45 °C → warn, 55 → crit
    if (t === null || t === undefined) return 'var(--bg1)';
    const x = Math.max(0, Math.min(1, (t - 24) / 30));
    return `color-mix(in srgb, var(--bg1) ${Math.round(100 - x * 100)}%, var(--warn) ${Math.round(x * 100)}%)`;
  }

  DCT.views.floor = (st) => {
    const En = E(), m = En.model; const design = st.mode === 'design';
    const maxPos = Math.max(...m.hall.rows.map((r) => r.positions.length));
    const W = LABELW + maxPos * (TILE + GAP) + 20, H = m.hall.rows.length * ROWH + 16;
    let svg = '';
    const loopRacks = (m.facility.loops[0]?.racks || []).map((p) => m.racks[p]).filter(Boolean);
    m.hall.rows.forEach((row, ri) => {
      const y = ri * ROWH + 14;
      svg += `<text class="rowlabel" x="10" y="${y + TILE / 2 + 4}">row ${esc(row.name)}</text>`;
      row.positions.forEach((pos, pi) => {
        const x = LABELW + pi * (TILE + GAP);
        const draftAdd = st.draft.adds.find((a) => a.row === row.name && a.position === pos.index);
        if (!pos.rack) {
          if (draftAdd) { svg += `<g class="tile add" data-act="design-edit-add" data-arg="${esc(draftAdd.name)}" transform="translate(${x},${y})"><rect class="body" width="${TILE}" height="${TILE}"/><text x="8" y="18" font-weight="600">${esc(draftAdd.name)}</text><text class="tag" x="8" y="34">${esc(DCT.packs.formats[draftAdd.format].tag)} · draft</text><text class="val" x="8" y="${TILE - 10}">+ not applied</text></g>`; return; }
          svg += `<g class="empty ${design ? 'design' : ''}" ${design ? `data-act="design-add" data-arg="${esc(row.name)}:${pos.index}"` : ''} transform="translate(${x},${y})"><rect width="${TILE}" height="${TILE}"/><text x="${TILE / 2 - 5}" y="${TILE / 2 + 6}">${design ? '+' : ''}</text></g>`;
          return;
        }
        const rack = m.racks[pos.rack];
        const sel = st.selection === rack.path; const removing = st.draft.removes.includes(rack.path);
        const hcls = rack.health === 'Critical' ? 'h-crit' : rack.health === 'Warning' ? 'h-warn' : '';
        const vmCount = rack.devices.filter((p) => m.devices[p].fidelity === 'vm').length;
        const unreach = rack.devices.filter((p) => m.devices[p].bmc && !m.devices[p].bmc.reachable).length;
        let fill = 'var(--bg1)', val = '';
        if (st.overlay === 'power') { const pct = rack.watts / rack.capacityW; fill = `color-mix(in srgb, var(--bg1) ${Math.round(100 - pct * 90)}%, var(--info) ${Math.round(pct * 90)}%)`; val = `${U.fmtKW(rack.watts)} · ${Math.round(pct * 100)}%`; }
        if (st.overlay === 'temperature') { const temps = rack.devices.map((p) => m.devices[p].sensors.find((s) => ['coolant-in', 'inlet'].includes(s.name))?.value).filter((v) => v != null); const avg = temps.length ? temps.reduce((a, b) => a + b, 0) / temps.length : null; fill = tempRamp(avg); val = avg === null ? '' : `${avg.toFixed(1)} °C ${rack.cooling === 'liquid' ? 'coolant' : 'inlet'}`; }
        if (st.overlay === 'health') { const n = rack.devices.filter((p) => m.devices[p].health !== 'OK').length; val = n ? `${n} flagged` : 'nominal'; }
        if (st.overlay === 'domains') { const dom = rack.domains.find((d) => d.kind === 'nvlink'); val = dom ? `NVLink ${dom.state}` : (rack.loop ? '' : 'air'); }
        svg += `<g class="tile ${hcls} ${sel ? 'sel' : ''} ${removing ? 'remove' : ''}" data-act="select" data-arg="${esc(rack.path)}" data-open="${esc(rack.path)}" transform="translate(${x},${y})" tabindex="0">
          <rect class="body" width="${TILE}" height="${TILE}" style="fill:${fill}"/>
          <text x="8" y="18" font-weight="600">${esc(rack.name)}</text>
          <text class="tag" x="8" y="33">${esc(rack.tag)}${vmCount ? ` · ${vmCount} vm` : ''}</text>
          ${rack.health !== 'OK' ? `<text class="mark ${U.sevClass(rack.health)}" x="${TILE - 16}" y="18">${U.SEV_MARK[rack.health]}</text>` : ''}
          ${unreach ? `<text class="mark warn" x="${TILE - 30}" y="18" title="BMC unreachable">⊘</text>` : ''}
          <text class="val" x="8" y="${TILE - 10}">${esc(val.length > 13 ? val.slice(0, 12) + '…' : val)}</text><title>${esc(rack.name)} · ${esc(rack.title)} · ${esc(val)}</title>
          ${removing ? `<text class="mark crit" x="8" y="${TILE - 24}">remove (draft)</text>` : ''}
        </g>`;
      });
    });
    if (st.overlay === 'domains') {
      for (const rack of Object.values(m.racks)) {
        if (!rack.domains.some((d) => d.kind === 'nvlink')) continue;
        const ri = m.hall.rows.findIndex((r) => r.name === rack.row), pi = rack.position - 1;
        svg += `<rect class="dom" x="${LABELW + pi * (TILE + GAP) - 4}" y="${ri * ROWH + 10}" width="${TILE + 8}" height="${TILE + 8}"/>`;
      }
      if (loopRacks.length) { const ri = m.hall.rows.findIndex((r) => r.name === loopRacks[0].row); const y = ri * ROWH + 14 + TILE + 10; const x1 = LABELW + (loopRacks[0].position - 1) * (TILE + GAP), x2 = LABELW + (loopRacks[loopRacks.length - 1].position - 1) * (TILE + GAP) + TILE; svg += `<line class="loop" x1="${x1}" y1="${y}" x2="${x2}" y2="${y}"/><text class="axis" x="${x1}" y="${y + 13}">cooling loop ${esc(m.facility.loops[0].name)}</text>`; }
    }
    const feeds = m.facility.feeds.map((f) => `<span>feed ${esc(f.name)} <b>${U.fmtKW(f.watts)}</b> / ${U.fmtKW(f.capacityW)}</span>`).join(' · ');
    const loops = m.facility.loops.map((l) => { const cdu = m.devices[l.device]; const sup = cdu.sensors.find((s) => s.name === 'supply').value, ret = cdu.sensors.find((s) => s.name === 'return').value; return `<span>${esc(l.name)} ${U.sevHTML(cdu.health)} supply <b>${sup?.toFixed(1)} °C</b> return <b>${ret?.toFixed(1)} °C</b> · ${l.racks.map((r) => esc(m.racks[r]?.name)).join(' ')}</span>`; }).join(' · ');
    const selRack = st.selection && m.racks[st.selection];
    return `
      <div class="toolbar">
        <span class="muted">overlay</span>
        <div class="seg">${['health', 'power', 'temperature', 'domains'].map((o) => `<button class="${st.overlay === o ? 'on' : ''}" data-act="overlay" data-arg="${o}">${o}</button>`).join('')}</div>
        <span class="grow"></span>
        ${selRack ? `<span class="path">${esc(selRack.name)} · ${esc(selRack.title)} · ${U.sevHTML(selRack.health)}</span> <button class="btn sm primary" data-act="open-rack" data-arg="${esc(selRack.path)}">Open rack</button> <button class="btn sm" data-act="inject-modal" data-arg="${esc(selRack.path)}">Fault ▾</button>${design ? ` <button class="btn sm danger" data-act="design-remove" data-arg="${esc(selRack.path)}">${st.draft.removes.includes(selRack.path) ? 'Keep' : 'Remove (draft)'}</button>` : ''} <button class="btn sm" data-act="select" data-arg="">clear</button>` : `<span class="faint">click a rack to select it, double-click to open${design ? '; click an empty position to add a rack' : ''}</span>`}
      </div>
      <div class="card" style="padding:10px 12px;overflow:auto">
        <svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" role="img" aria-label="hall floor plan">${svg}</svg>
      </div>
      <div class="toolbar" style="margin-top:10px;font-size:12px">${feeds} <span class="faint">·</span> ${loops}</div>
      <div class="legend">${st.overlay === 'health' ? `<span>${U.sevHTML('OK', 'nominal (grey, not green)')}</span><span>${U.sevHTML('Warning', 'warning')}</span><span>${U.sevHTML('Critical', 'critical')}</span><span class="sev warn"><span class="mark">⊘</span> BMC unreachable</span>` : st.overlay === 'power' ? '<span>fill = load against the rack\'s capacity</span>' : st.overlay === 'temperature' ? '<span>fill = coolant-in (liquid) or inlet (air) temperature</span>' : '<span><i style="border:1.5px dashed var(--unknown)"></i>NVLink domain</span><span><i style="background:var(--info)"></i>cooling loop</span>'}</div>`;
  };

  // ------------------------------------------------------------- events drawer / list (shared)
  const KINDS = ['fault', 'state', 'request', 'stack', 'k8s', 'dhcp', 'bms', 'nos', 'kernel', 'twin', 'scenario'];
  DCT.views.KINDS = KINDS;
  DCT.views.eventRows = (st, opts = {}) => {
    const En = E(); const f = st.ev;
    const sel = opts.selector !== undefined ? opts.selector : f.sel;
    const selRe = sel && sel.trim() ? sel.trim() : null;
    const match = (e) => (!selRe || U.matchSelector(selRe, e.path) || U.matchSelector(selRe + '/**', e.path)) && f.kinds.has(e.kind) && (f.minor || !e.minor);
    const evs = En.events.filter(match);
    const limit = opts.limit || 400;
    const shown = evs.slice(-limit);
    const depth = new Map();
    const bySeq = new Map(En.events.map((e) => [e.seq, e]));
    for (const e of shown) { let d = 0, c = e.cause; while (c && d < 2) { const p = bySeq.get(c); if (!p || !match(p)) break; d++; c = p.cause; } depth.set(e.seq, d); }
    const rows = shown.map((e) => {
      const sev = e.kind === 'fault' && !/cleared/.test(e.msg) ? 'sev-crit' : /Critical|refused|REFUSED|failed|lost|blocked|undervoltage/.test(e.msg) ? 'sev-crit' : /Warning|Degraded|Quarantined|Unreachable|timeout/.test(e.msg) ? 'sev-warn' : '';
      const p = e.path.startsWith('scenario/') ? e.path : U.shortPath(e.path);
      return `<div class="evrow d${depth.get(e.seq)} ${e.minor ? 'minor' : ''} ${sev}" data-seq="${e.seq}"><span class="t">${U.fmtClock(e.t)}</span><span class="kind ${e.kind}">${e.kind}${e.what ? '·' + e.what : ''}</span><span class="p"><a href="#" data-act="goto" data-arg="${esc(e.path)}" title="${esc(e.path)}">${esc(p)}</a></span><span class="m">${esc(e.msg)}</span><span class="src" title="${esc(e.source)}">${esc(e.source)}</span></div>`;
    });
    return { html: rows.join('') || '<div class="evrow"><span></span><span></span><span class="faint">no events match</span></div>', total: evs.length };
  };
  DCT.views.drawer = (st, selector) => {
    const { html, total } = DCT.views.eventRows(st, { selector, limit: 150 });
    return `<div class="dhead"><b>events</b> <span class="faint">${total} · ${selector ? esc(selector) : 'whole estate'}</span>
      <span class="grow"></span>
      <label class="chk"><input type="checkbox" data-act="ev-follow" ${st.ev.follow ? 'checked' : ''}> follow</label>
      <label class="chk"><input type="checkbox" data-act="ev-minor" ${st.ev.minor ? 'checked' : ''}> minor</label>
      <a href="#/events">open events →</a></div>
      <div class="dbody" id="drawer-body">${html}</div>`;
  };
})();
