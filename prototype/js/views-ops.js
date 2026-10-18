// views-ops.js — Topology, Facility, Faults, Events, Scenarios, Stack, the design drawer and the modals
// (spec 12). Every action here is something the control API would expose (D16).
window.DCT = window.DCT || {};
DCT.views = DCT.views || {};
(() => {
  const U = DCT.util, esc = U.esc, P = DCT.packs;
  const E = () => DCT.engine, S = () => DCT.stack;
  const devLink = (p) => `<a href="#/device/${encodeURIComponent(p)}" class="mono">${esc(U.lastSeg(p))}</a>`;

  // ------------------------------------------------------------- topology
  DCT.views.topology = (st) => {
    const En = E(), m = En.model;
    const net = Object.values(m.racks).find((r) => r.format === 'generic/network-rack');
    const racks = Object.values(m.racks).filter((r) => r.archetype !== 'network').sort((a, b) => a.path.localeCompare(b.path));
    const spines = net ? net.devices.filter((p) => /\/spine-/.test(p)) : [];
    const rails = net ? net.devices.filter((p) => /\/(rail|roce)-leaf-/.test(p)) : [];
    const colW = 170, W = Math.max(700, 70 + racks.length * colW), H = 330;
    let svg = '';
    const pos = {};
    spines.forEach((p, i) => { const x = W / 2 - (spines.length * 130) / 2 + i * 130 + 65; pos[p] = { x, y: 40 }; });
    racks.forEach((r, i) => { const x0 = 70 + i * colW; (r.leaves || []).forEach((l, j) => { pos[l] = { x: x0 + 40 + j * 82, y: 170 }; }); pos[r.path] = { x: x0 + 81, y: 280 }; });
    for (const l of m.links.filter((l) => l.kind === 'frontend-uplink')) { const a = pos[l.a.dev], b = pos[l.b.dev]; if (!a || !b) continue; svg += `<line class="cable ${l.state}" data-act="cable" data-arg="${esc(l.id)}|${l.state === 'up' ? 'down' : 'up'}" x1="${a.x}" y1="${a.y + 14}" x2="${b.x}" y2="${b.y - 14}"><title>${esc(l.a.dev.split('/').pop())}:${esc(l.a.port)} ↔ ${esc(l.b.dev.split('/').pop())}:${esc(l.b.port)} — ${l.state} (click to ${l.state === 'up' ? 'pull' : 'replace'})</title></line>`; }
    for (const r of racks) { for (const l of r.leaves || []) { const a = pos[l], b = pos[r.path]; const links = m.links.filter((x) => x.kind === 'frontend' && (x.b.dev === l)); const up = links.filter((x) => x.state === 'up').length; svg += `<line class="cable ${up < links.length ? 'down' : ''}" x1="${a.x}" y1="${a.y + 14}" x2="${b.x}" y2="${b.y - 14}" style="cursor:default"><title>${up}/${links.length} host links up</title></line>`; } }
    const node = (p, label, sub, w = 110) => { const d = m.devices[p]; const q = pos[p]; const h = d.health === 'Critical' ? 'h-crit' : d.health === 'Warning' ? 'h-warn' : ''; return `<g class="node ${h} ${st.selection === p ? 'sel' : ''}" data-act="select" data-arg="${esc(p)}" data-open="${esc(p)}" style="cursor:pointer" transform="translate(${q.x - w / 2},${q.y - 14})"><rect width="${w}" height="28"/><text x="${w / 2}" y="12" text-anchor="middle">${esc(label)}</text><text x="${w / 2}" y="23" text-anchor="middle" style="fill:var(--fg-faint)">${esc(sub)}</text><title>${esc(p)}</title></g>`; };
    for (const p of spines) svg += node(p, U.lastSeg(p), `${m.devices[p].ports.filter((x) => x.peer).length} links · ${m.devices[p].power.state.toLowerCase()}`);
    for (const r of racks) { for (const l of r.leaves || []) svg += node(l, m.devices[l].slot.generated ? U.lastSeg(l).replace(/^leaf-/, '') : `${r.name}/${U.lastSeg(l)}`, m.devices[l].slot.generated ? 'generated' : 'in rack', 78); const q = pos[r.path]; const hosts = r.devices.filter((p) => ['compute-tray', 'server'].includes(m.devices[p].class)).length; svg += `<g class="node ${r.health === 'Critical' ? 'h-crit' : r.health === 'Warning' ? 'h-warn' : ''}" data-act="open-rack" data-arg="${esc(r.path)}" style="cursor:pointer" transform="translate(${q.x - 55},${q.y - 14})"><rect width="110" height="28"/><text x="55" y="12" text-anchor="middle">${esc(r.name)} · ${esc(r.tag)}</text><text x="55" y="23" text-anchor="middle" style="fill:var(--fg-faint)">${hosts} hosts</text></g>`; }
    svg += `<text class="axis" x="8" y="44">spines</text><text class="axis" x="8" y="174">leaves</text><text class="axis" x="8" y="284">racks</text>`;
    const nvlRacks = racks.filter((r) => r.domains.some((d) => d.kind === 'nvlink'));
    const tr = m.racks[st.topoRack] && nvlRacks.includes(m.racks[st.topoRack]) ? m.racks[st.topoRack] : nvlRacks[0];
    const dom = tr && tr.domains.find((d) => d.kind === 'nvlink');
    const sw = tr ? tr.devices.map((p) => m.devices[p]).filter((d) => d.class === 'switch-tray') : [];
    const ct = tr ? tr.devices.map((p) => m.devices[p]).filter((d) => d.class === 'compute-tray') : [];
    return `
      <div class="toolbar"><h2>Topology</h2><span class="muted">frontend fabric <code>clos-2tier</code> · SONiC-VS · BGP unnumbered + EVPN · click a leaf–spine cable to pull or replace it</span></div>
      <div class="card" style="overflow:auto"><svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" role="img" aria-label="frontend fabric">${svg}</svg></div>
      <div class="grid c2" style="margin-top:12px">
        <div class="card"><h3>backend rails</h3><table><thead><tr><th>rail leaf</th><th>technology</th><th class="num">host links</th><th class="num">up</th><th>health</th></tr></thead><tbody>
          ${rails.map((p) => { const d = m.devices[p]; const links = m.links.filter((l) => l.b.dev === p); return `<tr><td>${devLink(p)}</td><td>${/rail-leaf/.test(p) ? 'InfiniBand (behind UFM REST; no packet path)' : 'Ethernet RoCE'}</td><td class="num">${links.length}</td><td class="num">${links.filter((l) => l.state === 'up').length}</td><td>${U.sevHTML(d.health)}</td></tr>`; }).join('')}
        </tbody></table></div>
        <div class="card"><h3>NVLink domain <select data-act="topo-rack">${nvlRacks.map((r) => `<option value="${esc(r.path)}" ${tr === r ? 'selected' : ''}>${esc(r.name)}</option>`).join('')}</select> ${dom ? `<span class="pill ${dom.state === 'Ready' ? '' : dom.state === 'Degraded' ? 'warn' : 'crit'}">${esc(dom.state)}</span>` : ''}</h3>
          ${tr ? `<div class="small muted">manager: ${devLink(dom.manager)} (NMX-C) · formation: ${esc(dom.formation)}${dom.spans ? ` · spans ${esc(dom.spans)}` : ''}</div>
          <table style="margin-top:6px"><thead><tr><th>switch tray</th><th class="num">lanes up</th><th>power</th><th>health</th></tr></thead><tbody>${sw.map((d) => `<tr><td>${devLink(d.path)}</td><td class="num ${d.nvlinkUp < d.nvlinkLanes ? 'bad' : ''}">${d.nvlinkUp}/${d.nvlinkLanes}</td><td>${esc(d.power.state)}</td><td>${U.sevHTML(d.health)}</td></tr>`).join('')}</tbody></table>
          <div class="small muted" style="margin-top:8px">compute trays: ${ct.length} · GPU lanes up ${ct.reduce((s, d) => s + d.components.filter((c) => c.kind === 'accelerator').reduce((a, c) => a + c.lanesUp, 0), 0)}/${ct.reduce((s, d) => s + d.components.filter((c) => c.kind === 'accelerator').reduce((a, c) => a + c.lanes, 0), 0)}</div>` : '<span class="faint">no NVLink racks</span>'}
        </div></div>`;
  };

  // ------------------------------------------------------------- facility
  DCT.views.facility = (st) => {
    const En = E(), m = En.model, F = m.facility;
    const racks = Object.values(m.racks).sort((a, b) => a.path.localeCompare(b.path));
    const bms = [];
    for (const [k, v] of Object.entries(F.bms.points)) bms.push([k, String(v.value), v.t]);
    for (const l of F.loops) { const c = m.devices[l.device]; for (const [pt, s] of [['SupplyTemp', 0], ['ReturnTemp', 1], ['Flow', 2], ['Pressure', 3]]) bms.push([En.bmsTopic(c, pt), c.sensors[s].value?.toFixed(pt === 'Pressure' ? 2 : 1), En.now]); }
    for (const r of racks) bms.push([`BMS/v1/PUB/Value/Rack/Power/${r.path.split('/').slice(1).join('.')}`, Math.round(r.watts), En.now]);
    return `
      <div class="toolbar"><h2>Facility</h2><span class="muted">power one-line, cooling loops, and BMS points as published (dialect <code>${esc(F.bms.dialect)}</code>)</span></div>
      <div class="grid c2">
        <div class="card"><h3>power one-line</h3>
          ${F.feeds.map((f) => { const pct = f.watts / f.capacityW; return `<div style="margin-bottom:6px"><b>feed ${esc(f.name)}</b> <span class="mono small">${U.fmtKW(f.watts)} / ${U.fmtKW(f.capacityW)} · ${Math.round(pct * 100)}%</span><div class="bar ${pct > .9 ? 'crit' : pct > .8 ? 'warn' : ''}"><i style="width:${Math.min(100, pct * 100)}%"></i></div></div>`; }).join('')}
          <table style="margin-top:8px"><thead><tr><th>rack</th><th>distribution</th><th class="num">kW</th><th class="num">cap</th><th class="num">%</th><th>health</th></tr></thead><tbody>
            ${racks.map((r) => { const pct = r.watts / r.capacityW; const shelves = r.devices.map((p) => m.devices[p]).filter((d) => d.class === 'power-shelf'); const pdus = r.zeroU.map((p) => m.devices[p]).filter((d) => d && d.class === 'pdu'); return `<tr class="click" data-act="open-rack" data-arg="${esc(r.path)}"><td>${esc(r.name)} <span class="faint">${esc(r.tag)}</span></td><td class="small">${shelves.length ? `${shelves.filter((s) => !s.outputLost && s.power.state !== 'Off').length}/${shelves.length} shelves → busbar ${r.busbarOk === false ? '<span class="bad">undervoltage</span>' : ''}` : `${pdus.length} PDUs (A/B)`}</td><td class="num">${(r.watts / 1000).toFixed(1)}</td><td class="num">${(r.capacityW / 1000).toFixed(0)}</td><td class="num ${pct > .9 ? 'bad' : pct > .8 ? 'warnc' : ''}">${Math.round(pct * 100)}</td><td>${U.sevHTML(r.health)}</td></tr>`; }).join('')}
          </tbody></table></div>
        <div>
          <div class="card"><h3>cooling loops</h3>
            ${F.loops.map((l) => { const c = m.devices[l.device]; const heat = (l.heatW || 0) / 1000; const pct = heat / l.capacityKW; return `<div><b>${esc(l.name)}</b> ${devLink(l.device)} ${U.sevHTML(c.health)} <span class="small mono">supply ${c.sensors[0].value?.toFixed(1)} °C · return ${c.sensors[1].value?.toFixed(1)} °C · flow ${c.sensors[2].value?.toFixed(0)} L/min · ${heat.toFixed(0)} / ${l.capacityKW} kW</span><div class="bar ${pct > .9 ? 'crit' : pct > .8 ? 'warn' : ''}"><i style="width:${Math.min(100, pct * 100)}%"></i></div>
              <table style="margin-top:6px"><tbody>${l.racks.map((rp) => { const r = m.racks[rp]; if (!r) return ''; const temps = r.devices.map((p) => m.devices[p].sensors.find((s) => s.name === 'coolant-in')?.value).filter((v) => v != null); const avg = temps.length ? temps.reduce((a, b) => a + b, 0) / temps.length : null; const leaks = r.sensors.map((p) => m.devices[p]).filter((d) => d.leak); return `<tr class="click" data-act="open-rack" data-arg="${esc(rp)}"><td>${esc(r.name)}</td><td class="num">${avg === null ? '—' : avg.toFixed(1) + ' °C in'}</td><td>${leaks.length ? `<span class="pill crit">LEAK ${leaks.map((d) => esc(d.name)).join(', ')}</span>` : `<span class="faint">${r.sensors.length} leak sensors dry</span>`}</td></tr>`; }).join('')}</tbody></table></div>`; }).join('')}
            <p class="note">Air-cooled racks (${racks.filter((r) => r.cooling === 'air').map((r) => esc(r.name)).join(', ')}) sit on room air at 22 °C.</p></div>
          <div class="card"><h3>BMS points <span class="faint">(${bms.length})</span></h3><table><thead><tr><th>topic</th><th class="num">value</th><th>at</th></tr></thead><tbody>${bms.slice(0, 40).map(([k, v, t]) => `<tr><td class="mono small">${esc(k)}</td><td class="num">${esc(String(v))}</td><td class="mono small">${U.fmtClock(t)}</td></tr>`).join('')}</tbody></table></div>
        </div></div>`;
  };

  // ------------------------------------------------------------- faults
  function targetsFor(sel) {
    const En = E(); if (!sel || !sel.trim()) return [];
    const out = [];
    for (const p of En.model.order) { const d = En.dev(p); const hit = U.matchSelector(sel, p) || (d.rack && U.matchSelector(sel, d.rack)); if (hit && P.applicable(d, null, null).length) out.push({ path: p, label: U.shortPath(p), kind: d.class }); for (const c of d.components) if ((hit || U.matchSelector(sel, c.path)) && P.applicable(d, c, null).length) out.push({ path: c.path, label: U.shortPath(c.path), kind: c.kind }); }
    return out.slice(0, 200);
  }
  DCT.views.targetsFor = targetsFor;
  DCT.views.faults = (st) => {
    const En = E(); const active = En.activeFaults();
    const fx = st.fx; const targets = targetsFor(fx.sel);
    const T = fx.target && resolve(fx.target);
    const list = T ? P.applicable(T.device, T.comp, null) : [];
    const f = list.find((x) => x.id === fx.faultId) || list[0];
    return `<div class="toolbar"><h2>Faults</h2><span class="muted">faults go into the model, not into one interface; every effect below shows up on every interface that would reveal it</span></div>
      <div class="grid c2">
        <div>
          <div class="card"><h3>inject</h3>
            <div class="dialog" style="padding:0;border:0;width:auto;max-height:none">
              <div class="row"><label>target</label><div><input type="text" class="mono" style="width:100%" data-act="fx-sel" value="${esc(fx.sel)}" placeholder="selector, e.g. a03/compute-tray-07 or a0*/switch-tray-*"> <div class="small faint" style="margin-top:4px">${targets.length} matching device(s) and component(s)</div>
                ${targets.length ? `<select style="width:100%;margin-top:4px" data-act="fx-target"><option value="">pick one…</option>${targets.map((t) => `<option value="${esc(t.path)}" ${fx.target === t.path ? 'selected' : ''}>${esc(t.label)} (${esc(t.kind)})</option>`).join('')}</select>` : ''}</div></div>
              ${T ? `<div class="row"><label>fault</label><select style="width:100%" data-act="fx-fault">${list.map((x) => `<option value="${esc(x.id)}" ${f && f.id === x.id ? 'selected' : ''}>${esc(x.title)} — ${esc(x.id)}</option>`).join('')}</select></div>
              ${f ? `<div class="row"><label>effects</label><div class="effects"><ul style="margin:0">${f.effects.map((e) => `<li>${esc(e.replace('{pci}', T.comp?.pci || '…'))}</li>`).join('')}</ul><div class="small muted" style="margin-top:4px">clears: ${esc(f.clears)}</div></div></div>` : ''}
              <div class="row"><label>when</label><div class="seg"><button class="${fx.when === 'now' ? 'on' : ''}" data-act="fx-when" data-arg="now">now</button><button class="${fx.when === 'in' ? 'on' : ''}" data-act="fx-when" data-arg="in">in <input type="number" min="1" style="width:60px" data-act="fx-in" value="${fx.inSec}"> s</button><button class="${fx.when === 'every' ? 'on' : ''}" data-act="fx-when" data-arg="every">every <input type="number" min="10" style="width:60px" data-act="fx-every" value="${fx.everySec}"> s</button></div></div>
              <div class="row"><label>clear after</label><div><input type="number" min="0" style="width:80px" data-act="fx-clear" value="${fx.clearAfter}"> s <span class="faint small">(0 = stays until cleared)</span></div></div>
              <div class="actions"><button class="btn primary" data-act="fx-inject" ${f ? '' : 'disabled'}>Inject</button></div>` : '<p class="faint">enter a selector and pick a target</p>'}
            </div></div>
          <div class="card"><h3>chaos</h3><label class="chk"><input type="checkbox" data-act="chaos" ${En.chaos.enabled ? 'checked' : ''}> random faults, drawn from the estate's seed</label> <span class="small">rate <input type="number" min="0.1" step="0.1" style="width:70px" data-act="chaos-rate" value="${En.chaos.ratePerDeviceHour}"> per device per hour</span></div>
        </div>
        <div>
          <div class="card"><h3>active (${active.length})</h3>${active.length ? `<table><thead><tr><th>since</th><th>fault</th><th>target</th><th>by</th><th></th></tr></thead><tbody>${active.slice().reverse().map((i) => `<tr><td class="mono small">${U.fmtClock(i.since)}</td><td><span class="pill crit">${esc(i.title)}</span> <span class="faint mono small">${esc(i.fault)}</span></td><td class="path"><a href="#/device/${encodeURIComponent(i.device)}">${esc(U.shortPath(i.target))}</a></td><td class="small">${esc(i.by)}</td><td><button class="btn sm" data-act="clear-fault" data-arg="${i.id}">clear</button></td></tr>`).join('')}</tbody></table>` : '<span class="faint">none — the estate is nominal</span>'}</div>
          <div class="card"><h3>scheduled (${En.scheduled.length})</h3>${En.scheduled.length ? `<table><tbody>${En.scheduled.map((s) => `<tr><td class="mono small">${U.fmtClock(s.at)}</td><td>${s.clear ? `clear #${s.clear}` : `${esc(s.fault)} → ${esc(U.shortPath(s.target))}${s.every ? ` every ${s.every} s` : ''}`}</td></tr>`).join('')}</tbody></table>` : '<span class="faint">nothing scheduled</span>'}</div>
          <div class="card"><h3>technician acts</h3><p class="small muted">The same as a person at the rack: power at the BMC, shelf or PDU; pull or replace a cable (Ports tab, Topology); reseat (AC cycle). All are events, never estate changes.</p>${T ? `<div class="toolbar">${T.device.class !== 'sensor' ? DCT.views.powerMenu(T.device.path, T.device) : ''} <a class="btn sm" href="#/device/${encodeURIComponent(T.device.path)}">open ${esc(T.device.name)}</a></div>` : ''}</div>
        </div></div>`;
  };
  function resolve(path) { const En = E(); const d = En.dev(path); if (d) return { device: d, comp: null }; const pd = En.dev(U.parentPath(path)); if (!pd) return null; const c = pd.components.find((x) => x.path === path); return c ? { device: pd, comp: c } : null; }
  DCT.views.resolveTarget = resolve;

  // ------------------------------------------------------------- events
  DCT.views.events = (st) => {
    const En = E(); const { html, total } = DCT.views.eventRows(st, { limit: 600 });
    return `<div class="toolbar"><h2>Events</h2><span class="muted">${En.events.length} in the log · clock <b>${esc(En.model.estate.spec.clock)}</b> ${U.fmtClock(En.now)} <span class="faint">(wall clock: live only; scrub by scrolling)</span></span><span class="grow"></span><button class="btn sm" data-act="save-scenario">Save session as scenario</button></div>
      <div class="toolbar"><input type="text" class="mono" style="width:260px" data-act="ev-sel" value="${esc(st.ev.sel)}" placeholder="selector filter, e.g. a03/**"> <button class="btn sm" data-act="ev-apply">apply</button>
        <span class="faint">kinds</span> ${DCT.views.KINDS.map((k) => `<button class="btn sm ${st.ev.kinds.has(k) ? 'primary' : ''}" data-act="ev-kind" data-arg="${k}">${k}</button>`).join('')}
        <label class="chk"><input type="checkbox" data-act="ev-minor" ${st.ev.minor ? 'checked' : ''}> minor</label><label class="chk"><input type="checkbox" data-act="ev-follow" ${st.ev.follow ? 'checked' : ''}> follow</label></div>
      <div class="card" style="padding:0"><div class="dbody" id="events-body" style="max-height:calc(100vh - 230px);overflow:auto">${html}</div><div class="small faint" style="padding:6px 12px">${total} matching; showing the last ${Math.min(600, total)}. Indented rows are effects of the row above (cause chains).</div></div>`;
  };

  // ------------------------------------------------------------- scenarios
  function report(rep) {
    return `<pre>fidelity report
  devices: ${rep.counts.vm} vm, ${rep.counts.model} model, ${rep.counts.hardware} hardware
  impersonated: ${rep.impersonated.length ? rep.impersonated.map(esc).join('; ') : 'none'}
  stubs: ${rep.stubs.map(esc).join('; ')}
  unsynchronised adapters: ${rep.unsynchronised.length ? rep.unsynchronised.join('; ') : 'none'}
  addressing: ${esc(rep.addressing)}
  not modelled, and used by this run:
${rep.notModelled.map((n) => '    ' + esc(n)).join('\n')}
  unverified profiles (no hardware-run ledger entry):
${rep.unverified.map((n) => '    ' + esc(n)).join('\n')}</pre>`;
  }
  DCT.views.report = report;
  DCT.views.scenarios = (st) => {
    const runs = DCT.scenarios.runs;
    return `<div class="toolbar"><h2>Scenarios</h2><span class="muted">a stack's conformance suite; a result is never shown without its fidelity report (D11)</span></div>
      ${DCT.scenarios.list.map((sc) => { const R = runs[sc.name]; return `<div class="card"><div class="toolbar" style="margin-bottom:6px"><b>${esc(sc.name)}</b> <span class="pill">${esc(sc.milestone)}</span> <span class="muted">${esc(sc.title)}</span><span class="grow"></span>${R ? `<span class="pill ${R.status === 'passed' ? '' : R.status === 'failed' ? 'crit' : R.status === 'impersonated' ? 'warn' : 'info'}">${esc(R.status)}${R.status === 'passed' ? ' (conditional)' : R.status === 'impersonated' ? ' — not conformance' : ''}</span>` : ''} <button class="btn sm primary" data-act="scenario-run" data-arg="${esc(sc.name)}" ${R && R.status === 'running' ? 'disabled' : ''}>${R ? 'Run again' : 'Run'}</button></div>
        <p class="small muted">${esc(sc.description)}</p>
        <details><summary class="small">scenario file</summary><pre>${esc(sc.yaml)}</pre></details>
        ${R ? `<ol class="steps">${R.steps.map((s) => `<li><span class="${s.status === 'passed' ? 'ok' : s.status === 'failed' ? 'bad' : s.status === 'running' ? 'warnc' : 'faint'}">${s.status === 'passed' ? '✓' : s.status === 'failed' ? '✗' : s.status === 'running' ? '…' : '·'}</span> ${esc(s.desc)}${s.detail ? ` <span class="bad small">${esc(s.detail)}</span>` : ''}${s.checks.length ? `<ul class="small" style="margin:2px 0 0 14px;padding:0;list-style:none">${s.checks.map((c) => `<li><span class="${c.ok === true ? 'ok' : c.ok === false ? 'faint' : 'faint'}">${c.ok === true ? '✓' : c.ok === false ? '○' : '·'}</span> ${esc(c.desc)}</li>`).join('')}</ul>` : ''}</li>`).join('')}</ol>
        ${R.report ? report(R.report) : ''}` : ''}</div>`; }).join('')}`;
  };

  // ------------------------------------------------------------- stack
  DCT.views.stack = (st) => {
    const En = E(), St = S(); const m = En.model;
    const machines = Object.entries(St.machines).sort(([a], [b]) => a.localeCompare(b));
    const states = {}; for (const [, v] of machines) states[v.state] = (states[v.state] || 0) + 1;
    const flt = st.stackFilter;
    return `<div class="toolbar"><h2>Stack <span class="muted">· nvidia-dsx-os</span></h2><span class="sim">simulated</span><span class="muted">what the twin handed it, what it says about its machines, and the capability check</span></div>
      <div class="sim-note">This stack is a <b>simulation</b> standing in for NICo, NVSentinel and DSX Exchange so the console has something to watch respond. It reaches devices only through their protocols (Redfish, DHCP, BMS topics), the way a real stack would, and its policies are illustrative. In the real twin this page shows the stack definition's declared accessors only (12 — Rules, 4).</div>
      <div class="grid c2">
        <div class="card"><h3>components</h3><table><tbody>
          <tr><td><b>nico</b></td><td class="small">site controller (3-node service zone): discovery through relayed DHCP, Redfish inventory, firmware baseline, scout ${St.options.scoutImpersonated ? '<span class="pill warn">impersonated on model hosts</span>' : ''}, leak response, break-fix</td></tr>
          <tr><td><b>nvsentinel</b></td><td class="small">on the tenant Kubernetes cluster built from the ${Object.keys(St.k8s).length} vm hosts: DCGM + journald → cordon, drain, reboot, quarantine</td></tr>
          <tr><td><b>dsx-exchange</b></td><td class="small">MQTT/NATS bus; subscribed to <code>BMS/v1/PUB/#</code>; forwards leak points to NICo</td></tr>
          <tr><td><b>fleet-intelligence</b></td><td class="small">offline mode (no enrolment); reads DCGM through Mokka on vm hosts</td></tr>
        </tbody></table></div>
        <div class="card"><h3>inputs handed to the stack <span class="faint">(what a site would hand it, nothing more)</span></h3><dl class="kv">${St.inputs().map(([k, v]) => `<dt class="mono">${esc(k)}</dt><dd class="small">${esc(v)}</dd>`).join('')}</dl></div>
      </div>
      <div class="card" style="margin-top:12px"><h3>machines (${machines.length}) <span class="small">${Object.entries(states).map(([s, n]) => `<button class="btn sm ${flt === s ? 'primary' : ''}" data-act="stack-filter" data-arg="${esc(s)}">${esc(s)} ${n}</button>`).join(' ')} ${flt ? `<button class="btn sm" data-act="stack-filter" data-arg="">all</button>` : ''}</span></h3>
        <table><thead><tr><th>machine</th><th>class</th><th>fidelity</th><th>stack state</th><th>since</th><th>last note</th><th>k8s node</th></tr></thead><tbody>
        ${machines.filter(([, v]) => !flt || v.state === flt).slice(0, 120).map(([p, v]) => { const d = m.devices[p]; if (!d) return ''; const n = St.k8s[p]; return `<tr><td class="path"><a href="#/device/${encodeURIComponent(p)}">${esc(U.shortPath(p))}</a></td><td>${esc(d.class)}</td><td>${U.fidBadge(d.fidelity)}</td><td><span class="pill ${/Ready/.test(v.state) ? '' : /Discover|Validat/.test(v.state) ? 'info' : 'crit'}">${esc(v.state)}</span></td><td class="mono small">${U.fmtClock(v.since)}</td><td class="small">${esc(v.notes[0]?.note || '')}</td><td class="mono small">${n ? `${esc(n.node)}${n.cordoned ? ' <span class="bad">cordoned</span>' : ''}` : ''}</td></tr>`; }).join('')}
        </tbody></table></div>
      <div class="card" style="margin-top:12px"><h3>capability check — <code>dctwin check --stack nvidia-dsx-os</code></h3><table><thead><tr><th>capability</th><th>answer</th><th>by</th></tr></thead><tbody>
        ${St.capabilities().map(([c, a, by]) => `<tr><td class="mono">${esc(c)}</td><td><span class="pill ${a === 'met' ? '' : a === 'reduced' ? 'info' : a === 'stub' ? 'warn' : 'crit'}">${esc(a)}</span></td><td class="small">${esc(by)}</td></tr>`).join('')}
      </tbody></table></div>`;
  };

  // ------------------------------------------------------------- design drawer
  DCT.views.validateDraft = (st) => {
    const En = E(), m = En.model, out = [];
    const d = st.draft;
    if (!d.adds.length && !d.removes.length && !d.fidelity.length) return out;
    out.push({ level: 'ok', msg: 'schema dc-twin/v1alpha1' });
    out.push({ level: 'ok', msg: `packs resolve (${m.estate.spec.packs.length} pinned)` });
    let feedAdd = 0, loopAdd = 0, vmAdd = 0, spinePorts = 0;
    for (const a of d.adds) {
      const fmt = P.formats[a.format];
      for (const [k, v] of Object.entries(fmt.params)) { const val = a.params[k]; if (v.range && (val < v.range[0] || val > v.range[1])) out.push({ level: 'error', msg: `${a.name}: ${k}=${val} outside [${v.range.join(', ')}]` }); if (v.allowed && !v.allowed.includes(val) && !v.text) out.push({ level: 'error', msg: `${a.name}: ${k}=${val} not in {${v.allowed.join(', ')}}` }); }
      if (Object.values(m.racks).some((r) => r.name === a.name) || d.adds.filter((x) => x.name === a.name).length > 1) out.push({ level: 'error', msg: `${a.name}: name already used` });
      feedAdd += fmt.power.capacityW * 0.7 / 2; if (fmt.cooling === 'liquid') loopAdd += fmt.power.capacityW * 0.7 / 1000;
      if (fmt.archetype === 'rack-scale') { out.push({ level: 'ok', msg: `${a.name}: cabling — 2 leaves generated in a10, 4 spine uplinks, ${fmt.params.computeTrays.default * 4} rail links` }); spinePorts += 4; } else out.push({ level: 'ok', msg: `${a.name}: cabling — in-rack leaves, 4 spine uplinks` }), spinePorts += 4;
      if (a.vmHosts) vmAdd += a.vmHosts;
    }
    const net = Object.values(m.racks).find((r) => r.archetype === 'network');
    if (net) { const free = net.devices.filter((p) => /spine-/.test(p)).reduce((s, p) => s + m.devices[p].ports.filter((x) => /^eth/.test(x.name) && !x.peer).length, 0); out.push({ level: spinePorts <= free ? 'ok' : 'error', msg: `spine ports: ${spinePorts} needed, ${free} free` }); }
    for (const f of m.facility.feeds) { const pct = (f.watts + feedAdd) / f.capacityW; out.push({ level: pct > 1 ? 'error' : pct > .85 ? 'warn' : 'ok', msg: `feed ${f.name} at ${Math.round(pct * 100)}% after apply` }); }
    for (const l of m.facility.loops) { const kW = (l.heatW || 0) / 1000 + loopAdd; const pct = kW / l.capacityKW; if (loopAdd) out.push({ level: pct > 1 ? 'error' : pct > .85 ? 'warn' : 'ok', msg: `${l.name} at ${Math.round(pct * 100)}% (${kW.toFixed(0)} / ${l.capacityKW} kW) after apply` }); }
    for (const rp of d.removes) { const r = m.racks[rp]; if (!r) continue; const ready = r.devices.filter((p) => S().machines[p] && /Ready/.test(S().machines[p].state)).length; const faults = En.activeFaults().filter((f) => r.devices.includes(f.device)).length; out.push({ level: ready || faults ? 'warn' : 'ok', msg: `${r.name}: de-rack ${r.devices.length} devices${ready ? `; ${ready} machines are Ready in the stack` : ''}${faults ? `; ${faults} active faults will be lost` : ''}` }); }
    const vmNow = m.order.filter((p) => m.devices[p].fidelity === 'vm').length;
    const vmSel = d.fidelity.filter((f) => f.level === 'vm').reduce((s, f) => s + En.select(f.select).filter((p) => m.devices[p]).length, 0);
    const ram = (vmNow + vmSel + vmAdd) * 8 + 24; if (vmSel || vmAdd) out.push({ level: ram > 256 ? 'error' : ram > 200 ? 'warn' : 'ok', msg: `vm fidelity: ${vmNow + vmSel + vmAdd} hosts ≈ ${ram} GB RAM of a 256 GB host (estimate, 09 — Sizing)` });
    for (const f of d.fidelity) { const n = En.select(f.select).filter((p) => m.devices[p] && P.profiles[m.devices[p].profile].fidelity[f.level]).length; const miss = En.select(f.select).filter((p) => m.devices[p] && !P.profiles[m.devices[p].profile].fidelity[f.level]).length; if (miss) out.push({ level: 'error', msg: `${f.select}: ${miss} device(s) have no ${f.level} support in their profile` }); else out.push({ level: 'ok', msg: `${f.select}: ${n} device(s) → ${f.level}` }); }
    out.push({ level: m.estate.spec.clock === 'wall' ? 'ok' : 'warn', msg: `clock stays wall (vm devices and real NOS present) — D9` });
    return out;
  };
  DCT.views.draftDiff = (st) => {
    const d = st.draft; const lines = [];
    for (const rp of d.removes) { const r = E().model.racks[rp]; if (r) lines.push(`<span class="del">- sites[site-1].halls[hall-a].rows[${esc(r.row)}].racks[${esc(r.name)}]   # ${esc(r.format)}</span>`); }
    for (const a of d.adds) lines.push(`<span class="add">+ sites[site-1].halls[hall-a].rows[${esc(a.row)}].racks[${esc(a.name)}]:\n+     { position: ${a.position}, format: ${esc(a.format)}, params: ${esc(JSON.stringify(a.params))} }</span>`);
    for (const f of d.fidelity) lines.push(`<span class="add">+ fidelity.overrides: { select: ${esc(f.select)}, level: ${esc(f.level)} }</span>`);
    return lines.join('\n') || '<span class="faint">no changes</span>';
  };
  DCT.views.design = (st) => {
    const d = st.draft; const n = d.adds.length + d.removes.length + d.fidelity.length;
    const checks = DCT.views.validateDraft(st); const errors = checks.filter((c) => c.level === 'error').length;
    const groups = {}; for (const [id, f] of Object.entries(P.formats)) (groups[f.vendor] = groups[f.vendor] || []).push([id, f]);
    return `<div class="card"><h3>draft <span class="faint">(${n} change${n === 1 ? '' : 's'})</span></h3>
      <div class="toolbar" style="margin-bottom:8px"><button class="btn sm primary" data-act="draft-apply" ${!n || errors || st.applying ? 'disabled' : ''}>Apply</button><button class="btn sm" data-act="draft-export">Export YAML</button><button class="btn sm" data-act="draft-discard" ${n ? '' : 'disabled'}>Discard</button></div>
      ${n ? `<ul class="small" style="margin:0 0 8px;padding-left:16px">${d.adds.map((a, i) => `<li>+ <b>${esc(a.name)}</b> ${esc(P.formats[a.format].title)} <span class="faint mono">${esc(JSON.stringify(a.params))}</span> <a href="#" data-act="design-edit-add" data-arg="${esc(a.name)}">edit</a> · <a href="#" data-act="draft-drop" data-arg="add:${i}">drop</a></li>`).join('')}${d.removes.map((r, i) => `<li>− <b>${esc(U.lastSeg(r))}</b> remove <a href="#" data-act="draft-drop" data-arg="remove:${i}">keep</a></li>`).join('')}${d.fidelity.map((f, i) => `<li>~ <span class="mono">${esc(f.select)}</span> → ${U.fidBadge(f.level)} <a href="#" data-act="draft-drop" data-arg="fidelity:${i}">drop</a></li>`).join('')}</ul>
      <h3>validate</h3><ul class="small" style="margin:0 0 8px;padding-left:16px">${checks.map((c) => `<li class="${c.level === 'ok' ? 'ok' : c.level === 'warn' ? 'warnc' : 'bad'}">${c.level === 'ok' ? '✓' : c.level === 'warn' ? '⚠' : '✗'} ${esc(c.msg)}</li>`).join('')}</ul>
      <h3>diff</h3><pre>${DCT.views.draftDiff(st)}</pre>` : '<p class="small muted">Click an empty position on the floor to add a rack from a format. Select a rack to remove it, or a device (in its rack) to change its fidelity. Nothing changes until you apply.</p>'}
      </div>
      <div class="card"><h3>palette · rack formats</h3><div class="palette">${Object.entries(groups).map(([v, list]) => `<div class="small faint">${esc(v)}</div>${list.map(([id, f]) => `<div class="fmt" data-act="design-pick" data-arg="${esc(id)}"><b>${esc(f.title)}</b><span>${esc(id)} · ${f.height}U · ${esc(f.cooling)} · ${Object.keys(f.params).length} params</span></div>`).join('')}`).join('')}</div><p class="small faint" style="margin-top:6px">${st.designPick ? `selected <b>${esc(st.designPick)}</b>: now click an empty position` : 'pick a format, then click an empty position; or click the position first'}</p></div>`;
  };

  // ------------------------------------------------------------- modals
  DCT.views.modal = (st) => {
    const M = st.modal; if (!M) return '';
    const En = E();
    if (M.type === 'inject') {
      const base = resolve(M.target) || (En.rack(M.target) ? { rack: En.rack(M.target) } : null);
      let T = base && base.device ? base : null;
      const rack = base && base.rack;
      const devs = rack ? rack.devices.map((p) => En.dev(p)) : T ? [T.device] : [];
      if (rack && M.sub) T = resolve(M.sub);
      const comps = T ? [[T.device.path, 'device'], ...T.device.components.filter((c) => P.applicable(T.device, c).length).map((c) => [c.path, `${c.name} (${c.kind})`])] : [];
      const chosen = T ? (M.sub && comps.some(([p]) => p === M.sub) ? resolve(M.sub) : T) : null;
      const list = chosen ? P.applicable(chosen.device, chosen.comp, null) : [];
      const f = list.find((x) => x.id === M.faultId) || list[0];
      return `<div class="dialog"><h2>Inject fault</h2>
        <div class="row"><label>target</label><div><span class="mono">${esc(U.shortPath(M.target))}</span>
          ${rack ? `<select style="width:100%;margin-top:4px" data-act="m-sub"><option value="">pick a device in ${esc(rack.name)}…</option>${devs.filter((d) => P.applicable(d, null).length || d.components.some((c) => P.applicable(d, c).length)).map((d) => `<option value="${esc(d.path)}" ${M.sub && M.sub.startsWith(d.path) ? 'selected' : ''}>${esc(d.name)} (${esc(d.class)})</option>`).join('')}</select>` : ''}
          ${T && comps.length > 1 ? `<select style="width:100%;margin-top:4px" data-act="m-comp">${comps.map(([p, l]) => `<option value="${esc(p)}" ${(chosen.comp ? chosen.comp.path : chosen.device.path) === p ? 'selected' : ''}>${esc(l)}</option>`).join('')}</select>` : ''}</div></div>
        ${chosen ? `<div class="row"><label>fault</label><select style="width:100%" data-act="m-fault">${list.map((x) => `<option value="${esc(x.id)}" ${f && f.id === x.id ? 'selected' : ''}>${esc(x.title)} — ${esc(x.id)}</option>`).join('')}</select></div>
        ${f ? `<div class="row"><label>effects</label><div class="effects"><ul style="margin:0">${f.effects.map((e) => `<li>${esc(e.replace('{pci}', chosen.comp?.pci || '…'))}</li>`).join('')}</ul><div class="small muted" style="margin-top:4px">clears: ${esc(f.clears)}</div></div></div>` : ''}
        <div class="row"><label>when</label><div class="seg"><button class="${M.when !== 'in' ? 'on' : ''}" data-act="m-when" data-arg="now">now</button><button class="${M.when === 'in' ? 'on' : ''}" data-act="m-when" data-arg="in">in <input type="number" min="1" style="width:60px" data-act="m-in" value="${M.inSec || 30}"> s</button></div></div>
        <div class="row"><label>clear after</label><div><input type="number" min="0" style="width:80px" data-act="m-clear" value="${M.clearAfter || 0}"> s <span class="faint small">(0 = until cleared)</span></div></div>` : '<p class="faint">pick a device</p>'}
        <div class="actions"><button class="btn" data-act="modal-close">Cancel</button><button class="btn primary" data-act="m-inject" ${f ? '' : 'disabled'}>Inject</button></div></div>`;
    }
    if (M.type === 'add-rack') {
      const fmt = P.formats[M.format];
      const params = DCT.estate.paramsFor(fmt, M.params);
      return `<div class="dialog"><h2>Add rack at row ${esc(M.row)}, position ${M.position}</h2>
        <div class="row"><label>name</label><input type="text" class="mono" data-act="m-name" value="${esc(M.name)}"></div>
        <div class="row"><label>format</label><select data-act="m-format" style="width:100%">${Object.entries(P.formats).map(([id, f]) => `<option value="${esc(id)}" ${id === M.format ? 'selected' : ''}>${esc(f.vendor)} · ${esc(f.title)} (${esc(id)})</option>`).join('')}</select></div>
        ${Object.entries(fmt.params).map(([k, v]) => `<div class="row"><label>${esc(v.label || k)}</label><div>${v.range ? `<input type="number" min="${v.range[0]}" max="${v.range[1]}" data-act="m-param" data-arg="${esc(k)}" value="${esc(params[k])}"> <span class="faint small">range ${v.range[0]}–${v.range[1]}</span>` : v.text ? `<input type="text" class="mono" data-act="m-param" data-arg="${esc(k)}" value="${esc(params[k])}">` : `<select data-act="m-param" data-arg="${esc(k)}">${v.allowed.map((a) => `<option ${a == params[k] ? 'selected' : ''}>${esc(a)}</option>`).join('')}</select>`}</div></div>`).join('')}
        <div class="row"><label>fidelity</label><div><select data-act="m-vm"><option value="0" ${!M.vmHosts ? 'selected' : ''}>all devices at model</option><option value="1" ${M.vmHosts === 1 ? 'selected' : ''}>first host at vm</option><option value="2" ${M.vmHosts === 2 ? 'selected' : ''}>first two hosts at vm</option></select> <span class="faint small">a vm host ≈ 8 GB RAM; forces the wall clock (D9)</span></div></div>
        <p class="note">Applying racks it, cables it per the fabric template, connects power${fmt.cooling === 'liquid' ? ' and coolant' : ''}, and leaves every device off with its BMC on standby, so the stack discovers it the way it would on a site (03 — Changing a running estate).</p>
        <div class="actions">${M.editing ? `<button class="btn danger" data-act="m-drop-add">Drop from draft</button>` : ''}<button class="btn" data-act="modal-close">Cancel</button><button class="btn primary" data-act="m-add">${M.editing ? 'Update draft' : 'Add to draft'}</button></div></div>`;
    }
    if (M.type === 'text') return `<div class="dialog"><h2>${esc(M.title)}</h2>${M.note ? `<p class="small muted">${esc(M.note)}</p>` : ''}<textarea rows="22" readonly>${esc(M.text)}</textarea><div class="actions"><button class="btn" data-act="copy-text">Copy</button><button class="btn" data-act="download-text">Download</button><button class="btn primary" data-act="modal-close">Close</button></div></div>`;
    if (M.type === 'report') return `<div class="dialog"><h2>Fidelity report — ${esc(M.title)}</h2>${report(M.report)}<div class="actions"><button class="btn primary" data-act="modal-close">Close</button></div></div>`;
    if (M.type === 'apply') return `<div class="dialog"><h2>Applying draft</h2><ol class="steps">${M.log.map((l) => `<li>${esc(l)}</li>`).join('')}</ol>${M.done ? '<div class="actions"><button class="btn primary" data-act="modal-close">Close</button></div>' : '<p class="faint small">…</p>'}</div>`;
    return '';
  };
})();
