// views-rack.js — the Rack screen (elevation drawn from the format's layout data, D18) and the Device
// screen with its tabs, including the protocol inspector (spec 12 — Rack, Device).
window.DCT = window.DCT || {};
DCT.views = DCT.views || {};
(() => {
  const U = DCT.util, esc = U.esc, P = DCT.packs;
  const E = () => DCT.engine, S = () => DCT.stack;

  const UH = 13, RW = 320, LX = 70;

  function powerMenu(path, d) {
    const items = d.power.state === 'Off' ? [['On', 'Power on']] : [['GracefulShutdown', 'Graceful shutdown'], ['ForceOff', 'Force off'], ['ForceRestart', 'Force restart'], ['ACCycle', 'AC cycle (technician)']];
    return `<span class="menu"><button class="btn sm" data-act="menu">Power ▾</button><div class="dd"><div class="hint">acts as a technician at the BMC / shelf would</div>${items.map(([a, l]) => `<button data-act="power" data-arg="${esc(path)}|${a}">${l}</button>`).join('')}</div></span>`;
  }
  DCT.views.powerMenu = powerMenu;

  function elevation(st, rack, m) {
    const H = rack.height * UH + 24;
    let svg = `<rect x="${LX}" y="12" width="${RW}" height="${rack.height * UH}" fill="none" stroke="var(--line-strong)"/>`;
    for (let u = 1; u <= rack.height; u += (rack.height > 42 ? 6 : 6)) svg += `<text class="axis" x="${LX - 6}" y="${12 + (u - 1) * UH + 8}" text-anchor="end">U${rack.height - u + 1}</text>`;
    // group bands
    const groups = {}; for (const s of rack.slots) if (s.group) { (groups[s.group + s.u] = groups[s.group + s.u] || { name: s.group, from: s.u, to: s.u }); }
    const bands = []; let cur = null;
    for (const s of rack.slots) { if (s.group && cur && cur.name === s.group && s.u === cur.to + cur.units) { cur.to = s.u; cur.units = s.units; } else if (s.group) { cur = { name: s.group, from: s.u, to: s.u, units: s.units }; bands.push(cur); } else cur = null; }
    for (const b of bands) svg += `<g class="band"><rect x="${LX - 2}" y="${12 + (b.from - 1) * UH - 1}" width="${RW + 4}" height="${(b.to - b.from + b.units) * UH + 2}"/><text x="${LX + RW + 8}" y="${12 + (b.from - 1) * UH + ((b.to - b.from + b.units) * UH) / 2 + 3}">${esc(b.name)}</text></g>`;
    for (const s of rack.slots) {
      const d = m.devices[`${rack.path}/${s.name}`]; if (!d) continue;
      const y = 12 + (s.u - 1) * UH, h = s.units * UH;
      const sel = st.selection === d.path;
      const back = st.rackSide === 'back';
      let label = s.name;
      if (back) { const fe = d.ports.filter((p) => p.network === 'frontend' && p.peer).map((p) => p.peer.split(':')[0]); const be = d.ports.filter((p) => p.network === 'backend' && p.peer).length; label = `${s.name}  ${fe.length ? 'fe→' + [...new Set(fe)].join(',') : ''}${be ? `  be→${be} rails` : ''}${d.class === 'compute-tray' ? '  nvlink→spine' : ''}`; }
      svg += `<g class="slot ${sel ? 'sel' : ''} ${d.power.state === 'Off' ? 'off' : ''}" data-act="select" data-arg="${esc(d.path)}" data-open="${esc(d.path)}"><rect x="${LX}" y="${y}" width="${RW}" height="${h}"/>
        <text class="hm ${U.sevClass(d.health)}" x="${LX + 4}" y="${y + h / 2 + 3}">${d.health !== 'OK' ? U.SEV_MARK[d.health] : '·'}</text>
        <text x="${LX + 16}" y="${y + h / 2 + 3.5}" style="font-size:10px">${esc(label.slice(0, back ? 60 : 40))}</text>
        ${d.fidelity !== 'model' ? `<text class="fb" x="${LX + RW - 22}" y="${y + h / 2 + 3}">${d.fidelity}</text>` : ''}
        ${d.power.state !== 'On' ? `<text class="hm" x="${LX + RW - 50}" y="${y + h / 2 + 3}">${esc(d.power.state.toLowerCase())}</text>` : ''}</g>`;
    }
    // leak sensors at their level
    for (const sp of rack.sensors) { const d = m.devices[sp]; if (!d || d.slot.u === null) continue; const y = 12 + (d.slot.u - 1) * UH; svg += `<polygon class="leak ${d.leak ? 'on' : ''}" data-act="select" data-arg="${esc(d.path)}" style="cursor:pointer" points="${LX - 14},${y} ${LX - 4},${y + 4.5} ${LX - 14},${y + 9}"><title>${esc(d.name)}${d.leak ? ' — LEAK' : ''}</title></polygon>`; }
    // zero-U (PDUs) as strips on the right
    rack.zeroU.filter((p) => m.devices[p] && !m.devices[p].slot.generated).forEach((p, i) => { const d = m.devices[p]; svg += `<g class="zero ${st.selection === d.path ? 'sel' : ''}" data-act="select" data-arg="${esc(d.path)}" style="cursor:pointer"><rect x="${LX + RW + 90 + i * 16}" y="12" width="12" height="${rack.height * UH}"/><text x="${LX + RW + 99 + i * 16}" y="20">${esc(d.name)}</text></g>`; });
    if (st.rackSide === 'back' && rack.busbar) svg += `<rect x="${LX + RW + 70}" y="12" width="8" height="${rack.height * UH}" fill="var(--warn)" opacity=".5"/><text class="axis" x="${LX + RW + 66}" y="${H - 2}">busbar</text>`;
    return `<svg viewBox="0 0 ${LX + RW + 150} ${H}" width="${LX + RW + 150}" height="${H}" role="img" aria-label="rack elevation">${svg}</svg>`;
  }

  DCT.views.rack = (st, rackPath) => {
    const En = E(), m = En.model, rack = m.racks[rackPath];
    if (!rack) return `<div class="card">rack not found: <code>${esc(rackPath)}</code></div>`;
    const dom = rack.domains.find((d) => d.kind === 'nvlink');
    const loop = m.facility.loops.find((l) => l.racks.includes(rack.path)); const cdu = loop && m.devices[loop.device];
    const pct = rack.watts / rack.capacityW;
    const selDev = st.selection && m.devices[st.selection] && m.devices[st.selection].rack === rack.path ? m.devices[st.selection] : null;
    const design = st.mode === 'design';
    const fmt = P.formats[rack.format];
    return `
      <div class="toolbar"><h2>${esc(rack.name)} <span class="muted">· ${esc(rack.title)} <code>${esc(rack.format)}</code></span></h2>
        ${dom ? `<span class="pill ${dom.state === 'Ready' ? '' : dom.state === 'Degraded' ? 'warn' : 'crit'}">NVLink domain: ${esc(dom.state)} (${dom.traysOk ?? '?'}/${dom.traysTotal ?? '?'} switch trays)</span>` : ''}
        <span class="grow"></span>
        <div class="seg"><button class="${st.rackSide !== 'back' ? 'on' : ''}" data-act="rack-side" data-arg="front">front</button><button class="${st.rackSide === 'back' ? 'on' : ''}" data-act="rack-side" data-arg="back">back</button></div>
        <button class="btn sm" data-act="inject-modal" data-arg="${esc(rack.path)}">Fault ▾</button>
        ${design ? `<button class="btn sm danger" data-act="design-remove" data-arg="${esc(rack.path)}">${st.draft.removes.includes(rack.path) ? 'Keep rack' : 'Remove rack (draft)'}</button>` : ''}
      </div>
      <div class="grid side">
        <div class="card" style="overflow:auto">${elevation(st, rack, m)}</div>
        <div>
          <div class="card">
            <h3>rack</h3>
            <dl class="kv">
              <dt>power</dt><dd><div class="bar ${pct > .9 ? 'crit' : pct > .8 ? 'warn' : ''}"><i style="width:${Math.min(100, pct * 100)}%"></i></div><span class="small mono">${U.fmtKW(rack.watts)} / ${U.fmtKW(rack.capacityW)} · feeds ${rack.feeds.join(' + ')}${rack.busbar ? ` · busbar ${rack.busbarOk === false ? '<span class="bad">undervoltage</span>' : 'ok'}` : ''}</span></dd>
              ${cdu ? `<dt>coolant</dt><dd>${esc(loop.name)}: supply ${cdu.sensors[0].value?.toFixed(1)} °C · return ${cdu.sensors[1].value?.toFixed(1)} °C · ${U.sevHTML(cdu.health)}</dd>` : '<dt>cooling</dt><dd>air</dd>'}
              <dt>devices</dt><dd>${rack.devices.length} · ${rack.devices.filter((p) => m.devices[p].fidelity === 'vm').length} at ${U.fidBadge('vm')}, ${rack.devices.filter((p) => m.devices[p].fidelity === 'model').length} at ${U.fidBadge('model')}</dd>
              <dt>health</dt><dd>${U.sevHTML(rack.health)} ${rack.devices.filter((p) => m.devices[p].health !== 'OK').map((p) => `<a href="#/device/${encodeURIComponent(p)}">${esc(U.lastSeg(p))}</a>`).join(', ')}</dd>
              <dt>frontend</dt><dd>${(rack.leaves || []).map((l) => `<a href="#/device/${encodeURIComponent(l)}">${esc(U.lastSeg(l))}</a>`).join(', ') || '—'}</dd>
              <dt>params</dt><dd class="mono small">${esc(JSON.stringify(rack.params))}</dd>
            </dl>
          </div>
          ${selDev ? deviceCard(st, selDev, design) : `<div class="card"><span class="faint">select a slot to see the device; double-click to open it</span></div>`}
          <div class="card"><h3>is not</h3><ul class="small muted" style="margin:0;padding-left:16px">${(fmt.isNot || []).map((n) => `<li>${esc(n)}</li>`).join('')}</ul></div>
        </div>
      </div>`;
  };

  function deviceCard(st, d, design) {
    const m = S().machines[d.path];
    const top = d.sensors.slice(0, 3).map((s) => `${esc(s.name)} <b class="mono">${s.value === null ? '—' : s.quantity === 'leak' ? (d.leak ? 'LEAK' : 'dry') : s.value.toFixed(s.unit === 'W' ? 0 : 1)}</b>${s.unit ? ' ' + s.unit : ''}`).join(' · ');
    const faults = E().activeFaults().filter((f) => f.device === d.path);
    return `<div class="card">
      <h3>${esc(d.name)} <span class="faint">· ${esc(d.title)}</span></h3>
      <dl class="kv">
        <dt>health</dt><dd>${U.sevHTML(d.health, d.health)} ${faults.map((f) => `<span class="pill crit">${esc(f.title)}</span>`).join(' ')}</dd>
        <dt>power</dt><dd>${esc(d.power.state)} · ${U.fmtKW(d.power.watts)}</dd>
        <dt>fidelity</dt><dd>${U.fidBadge(d.fidelity)} ${design && E().profile(d).fidelity.vm ? `<select data-act="design-fid" data-arg="${esc(d.path)}"><option value="">change (draft)…</option><option value="model">model</option><option value="vm">vm</option></select>` : ''}</dd>
        ${d.bmc ? `<dt>bmc</dt><dd>${d.bmc.reachable ? 'reachable' : '<span class="bad">unreachable</span>'} · lease <span class="mono">${esc(d.bmc.lease || '—')}</span></dd>` : ''}
        ${m ? `<dt>stack</dt><dd><span class="pill ${/Ready/.test(m.state) ? '' : /Unhealthy|Quarantined|Unreachable|Refused|Leak/.test(m.state) ? 'crit' : 'info'}">${esc(m.state)}</span> <span class="faint small">simulated</span></dd>` : ''}
        <dt>sensors</dt><dd class="small">${top || '—'}</dd>
      </dl>
      <div class="toolbar" style="margin:10px 0 0">
        <a class="btn sm primary" href="#/device/${encodeURIComponent(d.path)}">Open</a>
        ${d.class !== 'sensor' ? powerMenu(d.path, d) : ''}
        <button class="btn sm" data-act="inject-modal" data-arg="${esc(d.path)}">Fault ▾</button>
      </div></div>`;
  }

  // ------------------------------------------------------------- device
  const TABS = ['overview', 'components', 'ports', 'sensors', 'firmware', 'logs', 'interfaces', 'console', 'faults', 'fidelity'];
  DCT.views.device = (st, path) => {
    const En = E(), m = En.model, d = m.devices[path];
    if (!d) return `<div class="card">device not found: <code>${esc(path)}</code></div>`;
    const rack = d.rack && m.racks[d.rack]; const pr = En.profile(d); const tab = TABS.includes(st.tab) ? st.tab : 'overview';
    const mach = S().machines[path];
    return `
      <div class="toolbar">
        <div><div class="path">${rack ? `<a href="#/rack/${encodeURIComponent(rack.path)}">${esc(rack.name)}</a> / ` : ''}<b>${esc(d.name)}</b> <span class="faint">${esc(path)}</span></div>
          <h2>${esc(d.title)} <span class="muted">· <code>${esc(d.profile)}</code> · ${esc(d.class)}</span></h2></div>
        <span class="grow"></span>
        ${U.fidBadge(d.fidelity)} ${U.sevHTML(d.health, d.health)} <span class="pill">${esc(d.power.state)} · ${U.fmtKW(d.power.watts)}</span>
        ${mach ? `<span class="pill ${/Ready/.test(mach.state) ? '' : 'crit'}" title="stack machine state (simulated)">stack: ${esc(mach.state)}</span>` : ''}
        ${d.class !== 'sensor' ? powerMenu(path, d) : ''}
        <button class="btn sm" data-act="inject-modal" data-arg="${esc(path)}">Fault ▾</button>
      </div>
      <div class="tabs">${TABS.map((t) => `<button class="${t === tab ? 'on' : ''}" data-act="tab" data-arg="${t}">${t === 'ports' ? 'ports & cables' : t}</button>`).join('')}</div>
      ${tabRender[tab](st, d, pr, rack)}`;
  };

  const val = (s) => (s.value === null ? '—' : s.quantity === 'leak' ? (s.value ? 'LEAK' : 'dry') : s.value.toFixed(s.unit === 'W' || s.unit === 'L/min' ? 0 : 1));
  const sStatus = (s) => (s.value !== null && s.crit != null && s.value >= s.crit ? 'Critical' : s.value !== null && s.warn != null && s.value >= s.warn ? 'Warning' : 'OK');

  const tabRender = {
    overview(st, d, pr, rack) {
      const mach = S().machines[d.path]; const node = S().k8s[d.path];
      const faults = E().activeFaults().filter((f) => f.device === d.path);
      return `<div class="grid c2">
        <div class="card"><h3>identity & place</h3><dl class="kv">
          <dt>serial</dt><dd class="mono">${esc(d.identity.serial)}</dd><dt>uuid</dt><dd class="mono">${esc(d.identity.uuid)}</dd>
          ${d.bmc ? `<dt>bmc mac</dt><dd class="mono">${esc(d.bmc.mac)}</dd><dt>bmc lease</dt><dd class="mono">${esc(d.bmc.lease || '— (no lease)')} <span class="faint">from the stack's DHCP via the NOS relay</span></dd><dt>dialect</dt><dd class="mono">${esc(d.bmc.dialect)}</dd>` : '<dt>bmc</dt><dd>none</dd>'}
          <dt>rack / slot</dt><dd>${rack ? `${esc(rack.name)} · ${d.slot.u !== null ? `U${rack.height - d.slot.u + 1}${d.units > 1 ? `–${rack.height - d.slot.u - d.units + 2}` : ''}` : d.slot.zeroU ? 'zero-U' : d.level ? `at the level of ${esc(d.level)}` : 'generated'}` : 'facility'}</dd>
          <dt>fidelity</dt><dd>${U.fidBadge(d.fidelity)} <span class="small muted">${d.fidelity === 'vm' ? `QEMU/KVM ${pr.fidelity.vm?.arch || ''} with OVMF and swtpm; in-host: ${(pr.fidelity.vm?.inHost || []).join(', ') || '—'}` : 'protocols only; no OS runs; boot is a boot-walk'}</span></dd>
          <dt>deterministic identity</dt><dd class="small muted">derived from seed ${E().model.estate.spec.seed} and the path: the same on every run</dd>
        </dl></div>
        <div class="card"><h3>state</h3><dl class="kv">
          <dt>health</dt><dd>${U.sevHTML(d.health, d.health)} <span class="small muted">roll-up of ${d.components.length} components, ${d.sensors.length} sensors and device-level faults</span></dd>
          <dt>power</dt><dd>${esc(d.power.state)} · ${U.fmtKW(d.power.watts)} · load ${Math.round(d.power.load * 100)}%${d.power.transition ? ` · → ${esc(d.power.transition.to)} in ${U.fmtDur((d.power.transition.endsAt - E().now) / 1000)}` : ''}</dd>
          <dt>active faults</dt><dd>${faults.length ? faults.map((f) => `<div><span class="pill crit">${esc(f.title)}</span> on <span class="mono">${esc(f.comp || f.link || 'device')}</span> since ${U.fmtClock(f.since)} by ${esc(f.by)} <button class="btn sm" data-act="clear-fault" data-arg="${f.id}">clear</button></div>`).join('') : '<span class="faint">none</span>'}</dd>
          ${mach ? `<dt>stack (simulated)</dt><dd><span class="pill ${/Ready/.test(mach.state) ? '' : 'crit'}">${esc(mach.state)}</span> since ${U.fmtClock(mach.since)}${mach.notes.length ? `<div class="small muted">${mach.notes.slice(0, 3).map((n) => `${U.fmtClock(n.t)} ${esc(n.note)}`).join('<br>')}</div>` : ''}</dd>` : ''}
          ${node ? `<dt>kubernetes node</dt><dd class="mono">${esc(node.node)} · ${node.cordoned ? '<span class="bad">cordoned</span>' : 'schedulable'} · ${node.pods} pods</dd>` : ''}
          ${d.attestation ? `<dt>attestation</dt><dd>${d.attestation === 'ok' ? 'last quote accepted' : '<span class="bad">PCR mismatch — refused</span>'}</dd>` : ''}
        </dl></div>
        <div class="card" style="grid-column:1/-1"><h3>is not (${esc(d.profile)})</h3><ul class="small muted" style="margin:0;padding-left:16px">${(pr.isNot || []).map((n) => `<li>${esc(n)}</li>`).join('') || '<li>nothing declared</li>'}</ul></div>
      </div>`;
    },
    components(st, d) {
      return `<div class="card"><table><thead><tr><th>component</th><th>kind</th><th>model</th><th>health</th><th>state</th><th>details</th><th></th></tr></thead><tbody>
        ${d.components.map((c) => `<tr><td class="mono">${esc(c.name)}</td><td>${esc(c.kind)}</td><td>${esc(c.model)}</td><td>${U.sevHTML(c.health, c.health)}</td><td>${esc(c.state)}</td><td class="mono small">${c.pci ? esc(c.pci) + ' ' : ''}${c.lanes ? `nvlink ${c.lanesUp}/${c.lanes} ` : ''}${Object.keys(c.counters).length ? esc(JSON.stringify(c.counters)) : ''}</td><td>${P.applicable(d, c).length ? `<button class="btn sm" data-act="inject-modal" data-arg="${esc(c.path)}">Fault ▾</button>` : ''}</td></tr>`).join('')}
      </tbody></table></div>`;
    },
    ports(st, d) {
      const m = E().model;
      return `<div class="card"><table><thead><tr><th>port</th><th>kind</th><th>speed</th><th>network</th><th>state</th><th>peer (LLDP)</th><th></th></tr></thead><tbody>
        ${d.ports.map((p) => { const l = p.link && m.linkById[p.link]; return `<tr><td class="mono">${esc(p.name)}</td><td>${esc(p.kind)}</td><td>${esc(p.speed || '')}</td><td>${esc(p.network || (p.kind === 'power-in' ? 'power' : ''))}</td><td>${p.kind.startsWith('power') ? '' : p.state === 'up' ? 'up' : '<span class="bad">down</span>'}</td><td class="mono small">${esc(p.peer || (p.kind === 'nvlink' ? 'spine (fixed)' : p.note || ''))}</td><td>${l ? `<button class="btn sm" data-act="cable" data-arg="${esc(l.id)}|${l.state === 'up' ? 'down' : 'up'}">${l.state === 'up' ? 'pull cable' : 'replace cable'}</button>` : ''}</td></tr>`; }).join('')}
      </tbody></table></div>`;
    },
    sensors(st, d) {
      return `<div class="card"><table><thead><tr><th>sensor</th><th>quantity</th><th class="num">reading</th><th>unit</th><th class="num">warn</th><th class="num">crit</th><th>status</th></tr></thead><tbody>
        ${d.sensors.map((s) => `<tr><td class="mono">${esc(s.name)}</td><td>${esc(s.quantity)}</td><td class="num">${val(s)}</td><td>${esc(s.unit)}</td><td class="num">${s.warn ?? ''}</td><td class="num">${s.crit ?? ''}</td><td>${U.sevHTML(sStatus(s), sStatus(s))}</td></tr>`).join('') || '<tr><td colspan="7" class="faint">no sensors</td></tr>'}
      </tbody></table><p class="note">Readings are first-order models driven by power, the coolant loop and injected faults; they exist to cross thresholds, not to predict a real device.</p></div>`;
    },
    firmware(st, d) {
      return `<div class="card"><table><thead><tr><th>component</th><th>version</th><th>baseline</th><th>status</th></tr></thead><tbody>
        ${d.firmware.map((f) => `<tr><td class="mono">${esc(f.component)}</td><td class="mono">${esc(f.version)}${f.staged ? ` <span class="pill info">staged → ${esc(f.staged)} on reset</span>` : ''}</td><td class="mono">${esc(f.baseline)}</td><td>${f.status === 'in-baseline' ? 'in baseline' : f.status === 'staged' ? '<span class="pill info">staged</span>' : '<span class="pill warn">out of baseline</span>'}</td></tr>`).join('') || '<tr><td colspan="4" class="faint">no firmware inventory</td></tr>'}
      </tbody></table></div>`;
    },
    logs(st, d) {
      return `<div class="grid c2"><div class="card"><h3>Redfish log (SEL)</h3>${d.logs.length ? `<table><tbody>${d.logs.slice(0, 40).map((l) => `<tr><td class="mono small">${U.fmtClock(l.t)}</td><td>${U.sevHTML(l.severity)}</td><td>${esc(l.msg)}</td></tr>`).join('')}</tbody></table>` : '<span class="faint">no entries</span>'}</div>
        <div class="card"><h3>kernel log</h3>${d.fidelity === 'vm' ? (d.kmsg.length ? `<pre>${d.kmsg.slice(0, 40).map((k) => `${U.fmtClock(k.t)}  ${esc(k.line)}`).join('\n')}</pre>` : '<span class="faint">nothing logged since boot</span>') : `<p class="note">No kernel log: at ${U.fidBadge(d.fidelity)} fidelity no OS runs. ${d.kmsg.length ? `The model recorded ${d.kmsg.length} line(s) a driver would have printed:` : ''}</p>${d.kmsg.length ? `<pre>${d.kmsg.slice(0, 20).map((k) => `${U.fmtClock(k.t)}  ${esc(k.line)}`).join('\n')}</pre>` : ''}`}</div></div>`;
    },
    interfaces(st, d) {
      const En = E();
      if (!d.bmc) return `<div class="card"><h3>interfaces</h3>${d.class === 'sensor' || d.class === 'cdu' ? `<p>This device has no BMC. It publishes BMS points in the <code>${esc(En.model.facility.bms.dialect)}</code> dialect:</p><table><tbody>${(d.class === 'sensor' ? [['Leak', d.leak]] : [['SupplyTemp', d.sensors[0]?.value?.toFixed(1)], ['ReturnTemp', d.sensors[1]?.value?.toFixed(1)], ['Flow', d.sensors[2]?.value?.toFixed(0)], ['Pressure', d.sensors[3]?.value?.toFixed(2)]]).map(([pt, v]) => `<tr><td class="mono small">${esc(En.bmsTopic(d, pt))}</td><td class="mono">${esc(String(v))}</td></tr>`).join('')}</tbody></table>` : '<span class="faint">no management interface</span>'}</div>`;
      const tree = En.redfishTree(d); const paths = Object.keys(tree);
      const cur = st.rfPath && tree[st.rfPath] ? st.rfPath : '/redfish/v1';
      const r = (d.requests || []);
      return `<div class="grid c2">
        <div class="card"><h3>Redfish — as the stack sees it <span class="faint">(${esc(d.bmc.dialect)}${d.bmc.reachable ? '' : ' — currently timing out'})</span></h3>
          <div class="grid" style="grid-template-columns: 1fr 1fr; gap:8px"><div class="rftree">${paths.map((p) => `<div class="${p === cur ? 'on' : ''}" data-act="rf" data-arg="${esc(p)}">${esc(p.replace('/redfish/v1', '…'))}</div>`).join('')}</div><pre style="max-height:360px">${esc(JSON.stringify(tree[cur], null, 2))}</pre></div>
          <p class="note">Quirks of this dialect: ${(P.dialects[d.bmc.dialect].quirks || []).map(esc).join('; ') || 'none recorded'}. IPMI: chassis power, SEL, FRU and sensors are served from the same model. DHCP: ${d.bmc.lease ? `lease ${esc(d.bmc.lease)} via the NOS relay` : 'no lease yet'}.</p></div>
        <div class="card"><h3>requests the stack made to this device <span class="faint">(${r.length})</span></h3>
          ${r.length ? `<table><thead><tr><th>time</th><th>method</th><th>path</th><th class="num">status</th><th class="num">ms</th><th>from</th></tr></thead><tbody>${r.slice(0, 40).map((q) => `<tr><td class="mono small">${U.fmtClock(q.t)}</td><td class="mono">${esc(q.method)}</td><td class="mono small">${esc(q.rpath)}</td><td class="num ${q.status === 0 ? 'bad' : q.status >= 400 ? 'warnc' : ''}">${q.status || 'timeout'}</td><td class="num">${q.latency}</td><td class="small">${esc(q.from)}</td></tr>`).join('')}</tbody></table>` : '<span class="faint">none yet — the stack polls every 30 s</span>'}
        </div></div>`;
    },
    console(st, d) {
      const lines = d.console.slice(0, 60);
      return `<div class="card"><h3>serial console (SOL)${d.fidelity === 'vm' ? ' — the VM\'s serial port' : ''}</h3>
        <div class="console">${d.fidelity !== 'vm' ? `<div class="t"># model fidelity: no OS runs, so there is no serial output. The BMC's SOL session is open; power events and POST codes only.</div>` : ''}${lines.length ? lines.slice().reverse().map((l) => `<div><span class="t">${U.fmtClock(l.t)}</span> ${esc(l.line)}</div>`).join('') : '<div class="t"># (no output yet)</div>'}</div></div>`;
    },
    faults(st, d) {
      const active = E().activeFaults().filter((f) => f.device === d.path);
      const cat = [[null, P.applicable(d, null, null)], ...d.components.map((c) => [c, P.applicable(d, c, null)])].filter(([, l]) => l.length);
      return `<div class="grid c2">
        <div class="card"><h3>active on this device</h3>${active.length ? `<table><tbody>${active.map((f) => `<tr><td><span class="pill crit">${esc(f.title)}</span></td><td class="mono small">${esc(f.comp || f.link || 'device')}</td><td class="small">${U.fmtClock(f.since)} · ${esc(f.by)}</td><td><button class="btn sm" data-act="clear-fault" data-arg="${f.id}">clear</button></td></tr>`).join('')}</tbody></table>` : '<span class="faint">none</span>'}</div>
        <div class="card"><h3>catalogue, filtered to this device</h3>${cat.map(([c, list]) => `<div class="small muted" style="margin-top:6px">${c ? `<span class="mono">${esc(c.name)}</span> (${esc(c.kind)})` : 'device'}</div>${list.map((f) => `<div><button class="btn sm" data-act="inject-direct" data-arg="${esc(f.id)}|${esc(c ? c.path : d.path)}">inject</button> ${esc(f.title)} <span class="faint mono small">${esc(f.id)}</span></div>`).join('')}`).join('') || '<span class="faint">no applicable faults</span>'}</div></div>`;
    },
    fidelity(st, d, pr, rack) {
      const rep = E().fidelityReport([d.path]);
      return `<div class="grid c2">
        <div class="card"><h3>this device</h3><dl class="kv"><dt>level</dt><dd>${U.fidBadge(d.fidelity)}</dd><dt>means</dt><dd class="small">${d.fidelity === 'vm' ? 'A QEMU/KVM machine behind the emulated BMC: real UEFI, PXE, disk, TPM measurements, and the vendor\'s management library over a shim.' : d.fidelity === 'hardware' ? 'A real device bridged into the twin\'s networks.' : 'Speaks its protocols from the model. No OS runs; boot is a boot-walk; agents cannot run (the stack\'s scout is impersonated).'}</dd>
          <dt>ledger</dt><dd class="small"><span class="pill warn">unverified</span> source: documentation; no hardware-run entry (07 — the fidelity ledger)</dd></dl></div>
        <div class="card"><h3>not modelled, if a run used this device</h3><ul class="small" style="margin:0;padding-left:16px">${rep.notModelled.map((n) => `<li>${esc(n)}</li>`).join('')}</ul>
          ${rep.impersonated.length ? `<p class="note warn">impersonated: ${rep.impersonated.map(esc).join('; ')}</p>` : ''}
          <p class="note">stubs: ${rep.stubs.map(esc).join('; ')}</p></div></div>`;
    },
  };
})();
