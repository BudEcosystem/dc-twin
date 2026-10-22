// views-screens.js — Topology, Facility, Scenarios and Design.
window.TC = window.TC || {};
(() => {
  const V = TC.views;
  const { h, segBox, seg, primary, secondary, MONO, SUB, GLASS } = V;
  const { SEVN, SEVC, kw } = TC.data;
  const CARD = 'background:#101010;border:1px solid #1f1f1f;border-radius:8px';
  const title = (t, sub) => h('div', { style: 'display:flex;align-items:baseline;gap:12px' }, h('span', { style: 'font-size:18px;font-weight:600;color:#edeef0' }, t), h('span', { style: 'font-size:12px;color:#757575' }, sub));

  // ------------------------------------------------------------------ topology
  function topology(A, M) {
    const src = A.src, tm = src.topoModel(A.S.fabric);
    const nvl = M.R && M.R.f === 'nvl' ? M.R : Object.values(src.racks).find((r) => r.f === 'nvl');
    const agg = nvl ? M.aggs[nvl.name] : null;
    const sts = nvl ? nvl.slots.filter((p) => src.devs[p] && src.devs[p].cls === 'switch-tray') : [];
    const cts = nvl ? nvl.slots.filter((p) => src.devs[p] && src.devs[p].cls === 'compute-tray') : [];
    const fabrics = tm.fabrics || null;
    return h('div', { style: 'overflow:auto;padding:16px;display:flex;flex-direction:column;gap:14px;min-width:0' },
      h('div', { style: 'display:flex;align-items:baseline;gap:12px' },
        h('span', { style: 'font-size:18px;font-weight:600;color:#edeef0' }, 'Topology'),
        h('span', { style: 'font-size:12px;color:#757575' }, tm.title),
        fabrics && fabrics.length > 1 ? h('div', { style: 'margin-left:auto' }, segBox(seg(fabrics.map((f) => [f]), tm.fabric, (v) => A.set({ fabric: v }), `padding:2px 8px;font:11px ${MONO};cursor:pointer`))) : null),
      h('div', { style: `${CARD};padding:12px;overflow:auto;flex:none` },
        tm.empty ? h('div', { style: 'color:#757575;font-size:12px;padding:20px' }, tm.empty) :
          h('svg', { viewBox: '0 0 960 330', style: 'width:100%;min-width:820px;height:auto;aspect-ratio:960 / 330;display:block' },
            tm.links.map((l) => h('g', { key: l.id, onClick: () => src.pullLink(l.id), style: 'cursor:pointer' },
              h('title', null, l.tip || l.id),
              h('line', { x1: l.x1, y1: l.y1, x2: l.x2, y2: l.y2, stroke: 'transparent', 'stroke-width': 8 }),
              h('line', { x1: l.x1, y1: l.y1, x2: l.x2, y2: l.y2, stroke: l.down ? '#ec7575' : l.part ? '#d1b854' : '#2f2f2f', 'stroke-width': l.down || l.part ? 2 : 1, 'stroke-dasharray': l.down ? '5 4' : 'none' }))),
            tm.nodes.map((n) => h('g', { key: n.id },
              h('title', null, n.tip || n.id),
              h('rect', { x: n.x - n.w / 2, y: n.y - 11, width: n.w, height: 22, rx: 4, fill: '#18191b', stroke: n.health && SEVN[n.health] ? SEVC[n.health] : n.down ? '#d1b854' : '#2a2a2a' }),
              h('text', { x: n.x, y: n.y + 4, 'text-anchor': 'middle', fill: '#cfcfcf', 'font-size': 10, 'font-family': MONO }, n.label))),
            h('text', { x: 8, y: 44, fill: '#4d4d4d', 'font-size': 10, 'font-family': MONO }, 'spine'),
            h('text', { x: 8, y: 174, fill: '#4d4d4d', 'font-size': 10, 'font-family': MONO }, 'leaf'),
            h('text', { x: 8, y: 304, fill: '#4d4d4d', 'font-size': 10, 'font-family': MONO }, 'rack'))),
      h('div', { style: 'display:grid;grid-template-columns:minmax(0,1fr) 320px;gap:14px;flex:none' },
        h('div', { style: `${CARD};padding:14px` },
          h('div', { style: 'display:flex;align-items:baseline;gap:10px;margin-bottom:10px' },
            h('span', { style: 'font-weight:600;color:#edeef0' }, `NVLink domain · ${nvl ? nvl.name : '—'}`),
            h('span', { style: `font-size:12px;color:${agg && agg.dom === 'Ready' ? '#b3b3b3' : '#d1b854'}` }, agg ? agg.dom || '—' : 'no rack-scale rack in this estate')),
          nvl ? h('div', { style: `display:grid;grid-template-columns:90px repeat(${Math.max(1, sts.length)},minmax(0,1fr));gap:2px;font:10px ${MONO};color:#757575` },
            h('span', null),
            sts.map((p) => h('span', { key: p, style: 'text-align:center' }, 'st' + src.devs[p].name.slice(-2))),
            cts.map((p) => { const d = src.devs[p]; const dead = d.comps.some((c) => c.state === 'offline' || (c.kind === 'accelerator' && c.health === 'Critical'));
              return [h('span', { key: p + 'n', style: `color:${dead ? '#ec7575' : '#757575'}` }, d.name.replace('compute-tray', 'ct')),
                sts.map((sp) => { const sd = src.devs[sp]; return h('span', { key: p + sp, style: `height:12px;border-radius:2px;background:${dead ? '#ec757599' : d.power !== 'On' ? '#1a1a1a' : sd.health !== 'OK' ? '#d1b85499' : '#2f2f2f'}` }); })]; })) : null),
        h('div', { style: `${CARD};padding:14px;display:flex;flex-direction:column;gap:8px;font-size:12px` },
          h('span', { style: 'font-weight:600;color:#edeef0' }, 'Pulled cables'),
          tm.pulled.map((p) => h('div', { key: p.id, style: 'display:flex;align-items:center;gap:8px;padding:6px 8px;background:#861a1a22;border:1px solid #861a1a66;border-radius:6px' },
            h('span', { style: `font:11px ${MONO};color:#ec7575;flex:1;overflow-wrap:anywhere`, title: p.id }, p.label),
            h('button', { onClick: () => src.pullLink(p.id, true), style: 'background:none;border:1px solid #3a3a3a;border-radius:5px;color:#cfcfcf;font-size:11px;padding:2px 7px;cursor:pointer' }, 'Replace'))),
          h('span', { style: 'color:#4d4d4d' }, tm.pulled.length ? '' : 'None. Click any link in the graph to pull it.'),
          h('span', { style: 'color:#757575;font-size:11.5px;line-height:1.5;margin-top:6px' }, src.kind === 'live' ? 'Links from GET /v1/links; a pull or replace is POST /v1/cable and takes both ends down. A rack line stands for every host link of that leaf into the rack.' : 'LLDP neighbours and BGP sessions are read from the NOS. A pulled cable takes both ends down.'))));
  }

  // ------------------------------------------------------------------ facility
  function facility(A, M) {
    const src = A.src, aggs = M.aggs;
    const feeds = Object.keys(src.feeds);
    const rowsNames = src.rows.map((r) => r.name);
    const cdu = src.cdu;
    const feedCard = (n) => {
      // up: the feed as the racks see it (after a UPS); ut: the utility feed itself
      const f = src.feeds[n], w = src.feedLoad(n, aggs), up = f.up, ut = src.kind === 'live' && src.utility && src.utility[n] ? src.utility[n].up : up;
      return h('div', { key: n, style: 'display:flex;flex-direction:column;gap:6px' },
        h('div', { style: `padding:10px;border:1px solid ${ut ? '#1f1f1f' : '#ec757588'};border-radius:6px;background:#18191b;display:flex;flex-direction:column;gap:6px` },
          h('div', { style: 'display:flex;justify-content:space-between' }, h('span', { style: 'color:#edeef0;font-weight:600' }, `feed ${n}`), h('span', { style: `font-size:11px;color:${ut ? '#b3b3b3' : '#ec7575'}` }, ut ? 'energised' : '▲ lost')),
          h('div', { style: 'height:4px;background:#0a0a0a;border-radius:2px;overflow:hidden' }, h('div', { style: `height:100%;width:${f.capW ? ((w / f.capW) * 100).toFixed(0) : 0}%;background:${up ? '#965cde' : '#ec7575'}` })),
          h('span', { style: `font:11px ${MONO};color:#b3b3b3` }, `${kw(w)} / ${f.capW ? (f.capW / 1000).toFixed(0) + ' kW' : '—'}`),
          h('button', { onClick: () => src.facility(`feed-${n}`), className: 'hv-danger', style: 'align-self:flex-start;background:none;border:1px solid #2a2a2a;border-radius:5px;color:#cfcfcf;font-size:11px;padding:2px 8px;cursor:pointer' }, ut ? `Fail feed ${n}` : `Restore feed ${n}`)),
        h('div', { style: 'width:1px;height:12px;background:#2a2a2a;margin-left:20px' }),
        h('div', { style: 'padding:8px 10px;border:1px solid #1f1f1f;border-radius:6px;font-size:12px;color:#b3b3b3;display:flex;justify-content:space-between' },
          h('span', null, src.caps.infra ? `ups-${n.toLowerCase()}` : 'sources'), h('span', { style: `font-family:${MONO};color:${up ? '#b3b3b3' : '#ec7575'}` }, src.upsLabel(n, aggs))),
        h('div', { style: 'width:1px;height:12px;background:#2a2a2a;margin-left:20px' }),
        h('div', { style: 'display:flex;gap:6px' }, rowsNames.map((row) => h('div', { key: row, style: `flex:1;padding:6px 8px;border:1px solid #1f1f1f;border-radius:6px;font:11px ${MONO};color:#b3b3b3;display:flex;flex-direction:column;gap:2px` },
          h('span', { style: 'color:#757575' }, `busway ${row}`), h('span', null, kw(src.rowFeedW(row, n, aggs)))))));
    };
    const cdus = src.cdus || (cdu ? [{ ...cdu, racks: src.loop }] : []);
    // the twin's power chain (live): every UPS, busway and panel, its state, load and acts
    const MODE_C = { online: '#b3b3b3', closed: '#b3b3b3', battery: '#d1b854', dead: '#d1b854', depleted: '#ec7575', fault: '#ec7575', open: '#ec7575' };
    const chain = src.chain || [];
    const chainList = chain.length ? h('div', { style: 'display:flex;flex-direction:column;gap:1px;margin-top:4px' },
      h('div', { style: 'display:grid;grid-template-columns:88px 70px 112px minmax(0,1fr) 150px 104px;gap:10px;font-size:10.5px;color:#4d4d4d;text-transform:uppercase;letter-spacing:.06em;padding:0 4px 4px' },
        h('span', null, 'power chain'), h('span', null, 'from'), h('span', null, 'state'), h('span', null, 'load vs capacity'), h('span', { style: 'text-align:right' }, 'load · battery'), h('span', null)),
      chain.map((n) => {
        const bad = MODE_C[n.mode] === '#ec7575', faulted = src.nodeFault(n.path, n.kind === 'ups' ? 'generic/power/ups-failure' : 'generic/power/breaker-trip');
        const pct = n.capacityW ? Math.min(100, (n.loadW / n.capacityW) * 100) : 0;
        return h('div', { key: n.name, style: 'display:grid;grid-template-columns:88px 70px 112px minmax(0,1fr) 150px 104px;gap:10px;align-items:center;padding:5px 4px;border-top:1px solid #1a1a1a;font-size:12px' },
          h('span', { style: `font:600 11.5px ${MONO};color:#edeef0`, title: `${n.kind} · serves ${(n.racks || []).join(' ') || '—'}` }, n.name),
          h('span', { style: `font:11px ${MONO};color:#757575` }, n.from),
          h('span', { style: `font:11px ${MONO};color:${!(n.racks || []).length && !chain.some((x) => x.from === n.name) ? '#d1b854' : MODE_C[n.mode] || '#b3b3b3'}`, title: (n.racks || []).length ? `serves ${n.racks.join(' ')}` : 'serves no rack' }, n.mode === 'dead' ? 'no supply' : !(n.racks || []).length && !chain.some((x) => x.from === n.name) ? `${n.mode} · no racks` : n.mode),
          h('div', { style: 'height:6px;background:#1f1f1f;border-radius:3px;overflow:hidden' }, h('div', { style: `height:100%;width:${pct.toFixed(0)}%;background:${bad ? '#ec7575' : pct > 90 ? '#d1b854' : '#965cde'}` })),
          h('span', { style: `font:11px ${MONO};color:#b3b3b3;text-align:right` }, `${kw(n.loadW || 0)}${n.kind === 'ups' ? ` · ${Math.round(n.charge ?? 0)} %${n.mode === 'battery' ? ` · ${Math.round((n.runtimeLeftS || 0) / 60)}m` : ''}` : ''}`),
          h('button', { onClick: () => src.facility(`node-${n.name}`), className: faulted ? 'hv-act' : 'hv-act-danger', style: `justify-self:end;background:${faulted ? '#18191b' : '#861a1a33'};border:1px solid ${faulted ? '#2a2a2a' : '#ec757566'};border-radius:5px;color:${faulted ? '#cfcfcf' : '#ec7575'};font-size:11px;padding:2px 8px;cursor:pointer;white-space:nowrap` },
            n.kind === 'ups' ? (faulted ? 'Restore UPS' : 'Fail UPS') : (faulted ? 'Close breaker' : 'Trip breaker')));
      })) : null;
    return h('div', { style: 'overflow:auto;padding:16px;display:flex;flex-direction:column;gap:14px;min-width:0' },
      title('Facility', src.kind === 'live' ? `power feeds${chain.length ? ' · power chain' : ''} · cooling loops · rack inputs (GET /v1/facility)` : 'power one-line · cooling loops · BMS points (nvidia/dsx-exchange)'),
      h('div', { style: 'display:grid;grid-template-columns:minmax(0,1.4fr) minmax(0,1fr);gap:14px;flex:none' },
        h('div', { style: `${CARD};padding:14px;display:flex;flex-direction:column;gap:12px` },
          h('span', { style: 'font-weight:600;color:#edeef0' }, 'Power'),
          h('div', { style: `display:grid;grid-template-columns:repeat(${Math.max(1, feeds.length)},1fr);gap:12px` }, feeds.map(feedCard)),
          chainList,
          h('div', { style: 'display:flex;flex-direction:column;gap:1px;margin-top:4px' },
            h('div', { style: 'display:grid;grid-template-columns:60px 90px minmax(0,1fr) 120px;gap:10px;font-size:10.5px;color:#4d4d4d;text-transform:uppercase;letter-spacing:.06em;padding:0 4px 4px' }, h('span', null, 'rack'), h('span', null, 'format'), h('span', null, 'load vs capacity'), h('span', { style: 'text-align:right' }, feeds.join(' / '))),
            Object.values(src.racks).map((r) => { const a = aggs[r.name]; const cap = src.rackCap(r);
              return h('div', { key: r.name, style: 'display:grid;grid-template-columns:60px 90px minmax(0,1fr) 120px;gap:10px;align-items:center;padding:5px 4px;border-top:1px solid #1a1a1a;font-size:12px' },
                h('span', { style: 'color:#edeef0;font-weight:600' }, r.name), h('span', { style: 'color:#757575;white-space:nowrap;overflow:hidden;text-overflow:ellipsis' }, r.tag),
                h('div', { style: 'height:6px;background:#1f1f1f;border-radius:3px;overflow:hidden' }, h('div', { style: `height:100%;width:${Math.min(100, (a.w / cap) * 100).toFixed(0)}%;background:${SEVN[a.health] ? SEVC[a.health] : '#965cde'}` })),
                h('span', { style: `font:11px ${MONO};color:#b3b3b3;text-align:right` }, feeds.map((n) => kw(src.rackFeedW(r, a, n))).join(' / '))); }))),
        h('div', { style: 'display:flex;flex-direction:column;gap:14px' },
          h('div', { style: `${CARD};padding:14px;display:flex;flex-direction:column;gap:10px` },
            h('div', { style: 'display:flex;align-items:baseline;gap:10px' }, h('span', { style: 'font-weight:600;color:#edeef0' }, 'Cooling'), !cdus.length ? h('span', { style: 'font-size:12px;color:#b3b3b3' }, 'no cooling loop in this estate') : null),
            cdus.map((c) => {
              const bad = c.state !== 'OK', racks = c.racks.map((n) => src.racks[n]).filter(Boolean);
              return h('div', { key: c.name, style: 'display:flex;flex-direction:column;gap:8px;padding-top:8px;border-top:1px solid #1a1a1a' },
                h('div', { style: 'display:flex;align-items:baseline;gap:10px' }, h('span', { style: `font:600 12px ${MONO};color:#edeef0` }, c.name),
                  h('span', { style: `font-size:12px;color:${bad ? '#ec7575' : !racks.length ? '#d1b854' : '#b3b3b3'}` }, bad ? '▲ flow low' : !racks.length ? 'no racks on this loop' : 'nominal'),
                  c.capKW ? h('span', { style: `margin-left:auto;font:11px ${MONO};color:#757575` }, `${(c.loadKW || 0).toFixed(0)} / ${c.capKW} kW`) : null),
                c.path ? h('div', { style: 'display:grid;grid-template-columns:repeat(3,1fr);gap:8px' },
                  [['supply', c.supplyC.toFixed(1) + ' °C', c.supplyC > 36 ? '#d1b854' : '#edeef0'], ['return', c.returnC.toFixed(1) + ' °C', c.supplyC > 36 ? '#d1b854' : '#edeef0'], ['flow', `${Math.round(c.flow)} ${c.flowUnit}`, bad ? '#ec7575' : '#edeef0']].map(([k, v, col]) =>
                    h('div', { key: k, style: 'padding:8px 10px;background:#18191b;border-radius:6px;display:flex;flex-direction:column;gap:2px' }, h('span', { style: 'font-size:11px;color:#757575' }, k), h('span', { style: `font:600 15px ${MONO};color:${col}` }, v)))) : null,
                !racks.length ? h('span', { style: 'font-size:11.5px;color:#757575;line-height:1.5' }, `${src.kind === 'live' ? 'Put racks on it in Design (its racks), ' : ''}liquid-cooled racks on no loop run on the default supply.`) : null,
                h('div', { style: 'display:flex;flex-direction:column;gap:1px' }, racks.map((r) => { const t = aggs[r.name].temp || 30;
                  return h('div', { key: r.name, style: 'display:grid;grid-template-columns:50px minmax(0,1fr) 110px;gap:10px;align-items:center;padding:5px 0;border-top:1px solid #1a1a1a;font-size:12px' },
                    h('span', { style: 'color:#edeef0;font-weight:600' }, r.name),
                    h('div', { style: 'height:6px;background:#1f1f1f;border-radius:3px;overflow:hidden' }, h('div', { style: `height:100%;width:${Math.min(100, ((t - 20) / 40) * 100).toFixed(0)}%;background:${t > 45 ? '#d1b854' : '#5b9dff'}` })),
                    h('span', { style: `font:11px ${MONO};color:${t > 45 ? '#d1b854' : '#b3b3b3'};text-align:right` }, `in ${t.toFixed(1)} · out ${(t + 11.6).toFixed(1)}`)); })));
            }),
            h('div', { style: 'display:flex;gap:6px;flex-wrap:wrap' }, src.facilityActs().map((x) => h('button', { key: x.kind, onClick: () => src.facility(x.kind), className: 'hv-act-danger', style: 'background:#18191b;border:1px solid #2a2a2a;border-radius:6px;padding:4px 9px;font-size:11.5px;color:#cfcfcf;cursor:pointer' }, x.label)))),
          h('div', { style: `${CARD};padding:14px;display:flex;flex-direction:column;gap:6px` },
            h('span', { style: 'font-weight:600;color:#edeef0' }, src.kind === 'live' ? 'Facility points as the twin derives them' : 'BMS points as published'),
            src.bmsPoints().map(([topic, v]) => h('div', { key: topic, style: `display:grid;grid-template-columns:minmax(0,1fr) auto;gap:8px;font:11px ${MONO};padding:4px 0;border-top:1px solid #1a1a1a` },
              h('span', { style: 'color:#b3b3b3;overflow-wrap:anywhere' }, topic), h('span', { style: `color:${/Lost|true|down/.test(v) ? '#ec7575' : '#cfcfcf'}` }, v)))))));
  }

  // ------------------------------------------------------------------ scenarios
  const STATUS_C = { passed: '#5cde7b', failed: '#ec7575', running: '#b48ae8', impersonated: '#d1b854', 'not-run': '#d1b854' };
  function scenarios(A) {
    return A.src.caps.scenarios === 'live' ? scenariosLive(A) : scenariosDemo(A);
  }
  function stepRow(key, icon, color, anim, kind, text, asserts) {
    return h('div', { key, style: 'display:grid;grid-template-columns:22px 60px minmax(0,1fr);gap:10px;padding:9px 0;border-top:1px solid #1a1a1a' },
      h('span', { style: `color:${color};font-size:13px;animation:${anim}` }, icon),
      h('span', { style: `font:11px ${MONO};color:#965cde` }, kind),
      h('div', { style: 'display:flex;flex-direction:column;gap:3px;min-width:0' },
        h('span', { style: `font:12px ${MONO};color:#cfcfcf;overflow-wrap:anywhere` }, text),
        asserts.map((a, j) => h('span', { key: j, style: `font:11px ${MONO};color:${a.color};overflow-wrap:anywhere` }, `${a.icon} ${a.text}`))));
  }
  function reportBox(lines) {
    return h('div', { style: 'padding:12px;background:#0a0a0a;border:1px solid #1f1f1f;border-radius:6px;display:flex;flex-direction:column;gap:4px' },
      h('span', { style: SUB }, 'Fidelity report'),
      lines.map((r, i) => h('span', { key: i, style: `font:11px ${MONO};color:#b3b3b3;line-height:1.6;white-space:pre-wrap` }, r)));
  }
  function scenariosDemo(A) {
    const S = A.S, src = A.src, SC = src.scenarios(), sn = SC[S.scenSel] ? S.scenSel : Object.keys(SC)[0], cur = SC[sn];
    const rr = src.scenRun && src.scenRun.name === sn ? src.scenRun : src.scenLast && src.scenLast.name === sn ? src.scenLast : null;
    const res = src.scenRes[sn];
    return h('div', { style: 'display:grid;grid-template-columns:280px minmax(0,1fr);gap:14px;padding:16px;min-height:0;overflow:auto' },
      h('div', { style: 'display:flex;flex-direction:column;gap:8px' },
        h('span', { style: 'font-size:18px;font-weight:600;color:#edeef0;margin-bottom:6px' }, 'Scenarios'),
        Object.entries(SC).map(([k, v]) => h('div', { key: k, onClick: () => A.set({ scenSel: k }), style: `padding:10px 12px;background:${k === sn ? '#18191b' : '#101010'};border:1px solid ${k === sn ? '#965cde88' : '#1f1f1f'};border-radius:8px;cursor:pointer;display:flex;flex-direction:column;gap:3px` },
          h('span', { style: `font:600 12.5px ${MONO};color:#edeef0` }, k),
          h('span', { style: 'font-size:11.5px;color:#757575' }, v.desc),
          h('span', { style: `font-size:11px;color:${STATUS_C[src.scenRes[k]] || '#4d4d4d'}` }, src.scenRes[k] || 'not run')))),
      h('div', { style: `${CARD};padding:16px;display:flex;flex-direction:column;gap:12px;min-width:0` },
        h('div', { style: 'display:flex;align-items:center;gap:12px' },
          h('div', { style: 'display:flex;flex-direction:column;gap:2px;flex:1;min-width:0' },
            h('span', { style: `font:600 14px ${MONO};color:#edeef0` }, sn), h('span', { style: 'font-size:12px;color:#757575' }, cur.req)),
          h('span', { style: `font-size:12px;color:${STATUS_C[res] || '#757575'};border:1px solid ${STATUS_C[res] || '#757575'};border-radius:999px;padding:1px 10px` }, res || 'not run'),
          primary('Run scenario', () => src.runScenario(sn), !!src.scenRun)),
        h('div', { style: 'display:flex;flex-direction:column' }, cur.steps.map((st, i) => {
          const r = rr ? rr.res[i] : null; const stt = r ? r.st : 'pending';
          return stepRow(i, { pending: '○', running: '◐', pass: '✓', fail: '✕' }[stt], { pending: '#4d4d4d', running: '#b48ae8', pass: '#5cde7b', fail: '#ec7575' }[stt], stt === 'running' ? 'dcBlink 1s infinite' : 'none', st.kind, st.text,
            (st.asserts || []).map(([t], j) => { const ok = r && r.a.length ? r.a[j] : null; return { text: t, icon: ok === null ? '·' : ok ? '✓' : '✕', color: ok === null ? '#757575' : ok ? '#5cde7b' : '#ec7575' }; }));
        })),
        reportBox(src.scenarioReport())));
  }
  function scenariosLive(A) {
    const src = A.src, sc = src.scn, R = sc.result;
    const verdict = sc.running ? 'running' : R ? R.verdict : 'not run';
    const rep = R && R.report;
    const list = (a) => (a && a.length ? a.join('; ') : 'none');
    const kv = (o) => Object.entries(o || {}).map(([k, v]) => `${v} ${k}`).join(' · ') || 'none';
    const lines = rep ? [`devices used: ${kv(rep.devices)} · ${rep.racks} racks · fabric devices: ${kv(rep.fabricDevices)}`, `clock: ${rep.clock} (time scale ${rep.timeScale})`, `impersonated: ${list(rep.impersonated)} · stubs: ${list(rep.stubs)}`,
      `twin incidents: ${list(rep.twinIncidents)} · unsynchronised adapters: ${list(rep.unsynchronised)}`, `fault effects not applied: ${list(rep.faultEffectsNotApplied)}`,
      `not modelled, by profile (each device's Fidelity tab has the detail): ${(rep.notModelled || []).map((n) => n.subject).join(', ') || 'none'}`] : ['A result is never shown without its fidelity report: run a scenario to see one.'];
    return h('div', { style: 'display:grid;grid-template-columns:minmax(320px,0.9fr) minmax(0,1.1fr);gap:14px;padding:16px;min-height:0;overflow:auto' },
      h('div', { style: 'display:flex;flex-direction:column;gap:8px;min-width:0' },
        h('span', { style: 'font-size:18px;font-weight:600;color:#edeef0;margin-bottom:6px' }, 'Scenarios'),
        h('div', { style: `${CARD};padding:12px;display:flex;flex-direction:column;gap:8px` },
          h('div', { style: 'display:flex;align-items:center;gap:8px' }, h('span', { style: SUB }, 'Scenario file'), h('div', { style: 'flex:1' }),
            h('label', { className: 'ds-btn ds-secondary', style: 'cursor:pointer' }, 'Open file…', h('input', { type: 'file', accept: '.yaml,.yml,.json', style: 'display:none', onChange: (e) => { const f = e.target.files && e.target.files[0]; if (f) f.text().then((t) => { src.scnText = t; A.render(); }); } })),
            secondary('Example', () => { src.scnText = src.example; A.render(); })),
          h('textarea', { className: 'tc-input', rows: 26, spellcheck: 'false', value: src.scnText, onInput: (e) => { src.scnText = e.target.value; } }),
          h('span', { style: 'font-size:11px;color:#757575;line-height:1.5' }, 'POST /v1/scenarios/run. A run acts on this estate: its faults and acts are real events here, and other clients see them. Scenario files live in examples/scenarios/ in the repository.'))),
      h('div', { style: `${CARD};padding:16px;display:flex;flex-direction:column;gap:12px;min-width:0` },
        h('div', { style: 'display:flex;align-items:center;gap:12px' },
          h('div', { style: 'display:flex;flex-direction:column;gap:2px;flex:1;min-width:0' },
            h('span', { style: `font:600 14px ${MONO};color:#edeef0` }, R ? R.scenario : 'scenario'), h('span', { style: 'font-size:12px;color:#757575' }, sc.err ? sc.err : R ? `${R.reason || ''}${R.took ? ' · took ' + R.took : ''}` : 'runs on this twin; the API answers when the scenario ends')),
          h('span', { style: `font-size:12px;color:${STATUS_C[verdict] || '#757575'};border:1px solid ${STATUS_C[verdict] || '#757575'};border-radius:999px;padding:1px 10px` }, verdict),
          primary('Run scenario', () => src.runScenarioText(), sc.running)),
        h('div', { style: 'display:flex;flex-direction:column' }, (R && R.steps ? R.steps : []).map((s, i) => {
          const ok = s.status === 'passed';
          return stepRow(i, ok ? '✓' : '✕', ok ? '#5cde7b' : '#ec7575', 'none', s.kind, `${s.name || ''}${s.took ? ' · ' + s.took : ''}`, (s.detail || []).map((x) => ({ text: x, icon: ok ? '✓' : '✕', color: ok ? '#757575' : '#ec7575' })));
        })),
        reportBox(lines)));
  }

  // ------------------------------------------------------------------ design
  function design(A, M) {
    const S = A.S, src = A.src;
    const rows = V.floorRows(A, M);
    const view = S.view;
    const LCOL = { power: '#965cde', network: '#5b9dff', cooling: '#d4a853' };
    const capA = (src.feeds.A || Object.values(src.feeds)[0] || {}).capW || 900000;
    const adds = S.draft.filter((d) => d.op !== 'remove'), removes = S.draft.filter((d) => d.op === 'remove');
    const removedW = removes.reduce((a, d) => a + ((M.aggs[d.name] || {}).w || 0), 0);
    const feedPct = Math.round(((Math.max(0, M.tot - removedW) / Math.max(1, Object.keys(src.feeds).length) + adds.length * 50000) / capA) * 100);
    const formats = src.designFormats();
    // a live twin validates the draft itself: it compiles the changed estate (a dry-run apply)
    const plan = src.planFor ? src.planFor(S.draft) : null;
    let checks = [['✓', '#5cde7b', 'schema'], [adds.every((d) => formats.some(([id]) => id === d.fmt)) ? '✓' : '✕', '#5cde7b', 'packs resolve'], ['✓', '#5cde7b', S.draft.length ? `cabling: frontend ${adds.length ? '+' + adds.length * 2 : ''}${removes.length ? ' −' + removes.length * 2 : ''} leaves` : 'cabling'],
      [feedPct > 80 ? '⚠' : '✓', feedPct > 80 ? '#d1b854' : '#5cde7b', `${Object.keys(src.feeds)[0] ? 'feed ' + Object.keys(src.feeds)[0] : 'feed'} at ${feedPct} %`], ['✓', '#5cde7b', src.kind === 'live' ? `adapters: ${(src.adapters || []).join(', ') || 'none'}` : 'stack capability check (nico, dsx-exchange)']];
    if (plan && plan.loading) checks = [['…', '#757575', 'compiling the changed estate on the twin…']];
    else if (plan) {
      checks = [];
      if (plan.ok) checks.push(['✓', '#5cde7b', `compiles: ${plan.devices[0]} → ${plan.devices[1]} devices, ${plan.links[0]} → ${plan.links[1]} links`]);
      for (const p of plan.problems || []) checks.push(['✕', '#ec7575', p]);
      for (const w of plan.warnings || []) checks.push(['⚠', '#d1b854', w]);
      for (const n of plan.notes || []) checks.push(['·', '#757575', n]);
      if (plan.ok) checks.push([feedPct > 80 ? '⚠' : '✓', feedPct > 80 ? '#d1b854' : '#5cde7b', `${Object.keys(src.feeds)[0] ? 'feed ' + Object.keys(src.feeds)[0] : 'feed'} at about ${feedPct} %`]);
      if (plan.ok) checks.push(['⚠', '#d1b854', 'apply restarts the estate: active faults, power changes and the event log start fresh, and a stack sees every BMC drop and come back']);
    }
    const site = src.estate.site || 'site-1', hallN = src.estate.hallName || 'hall-a';
    const diff = plan && plan.diff ? plan.diff.trimEnd() : S.draft.length ? [...removes.map((d) => `- sites[${site}].halls[${hallN}].rows[${d.row}].racks[${d.name}]`), ...adds.map((d) => `+ sites[${site}].halls[${hallN}].rows[${d.row}].racks[${d.name}]:\n+   { position: ${d.pos}, format: ${d.fmt} }`)].join('\n') : '  (no changes — click an empty position to add a rack, or a rack to remove it)';
    const canApply = S.draft.length && src.caps.design && !S.applying && (!plan || plan.ok);
    const apply = () => {
      if (!canApply) return;
      if (src.kind === 'live' && !S.confirmApply) { A.set({ confirmApply: true }); return; }
      A.set({ applyLog: [], confirmApply: false });
      A.set({ applying: true });
      src.applyDraft(S.draft, (line) => A.set({ applyLog: [...(A.S.applyLog || []), line] }), (ok = true) => A.set(ok ? { draft: [], applying: false } : { applying: false }));
    };
    const iso = view === 'iso' ? V.iso3d(A, M, rows) : null;
    return h('div', { style: 'display:grid;grid-template-columns:minmax(0,1fr) 380px;gap:8px;padding:8px;min-height:0' },
      h('div', { style: 'display:flex;flex-direction:column;gap:8px;min-height:0;min-width:0' },
        h('div', { style: `${CARD};padding:12px;display:flex;flex-direction:column;gap:10px` },
          h('div', { style: 'display:flex;align-items:center;gap:10px;flex-wrap:wrap' },
            h('span', { style: 'font-weight:600;color:#edeef0' }, `Design · ${(src.estate.hallName || 'hall-a')}`), h('span', { style: 'font-size:12px;color:#757575' }, 'pick a rack format, then click an empty position · select a rack to remove it'),
            h('div', { style: 'flex:1' }),
            h('div', { style: 'display:flex;align-items:center;gap:4px' }, h('span', { style: 'font-size:11px;color:#4d4d4d' }, 'layers'), V.layerChips(A, LCOL, false), V.animToggle(A, '1px 7px')),
            segBox(seg([['2d', '2D'], ['iso', 'ISO']], view, (v) => A.set({ view: v }), `padding:3px 9px;font:11px ${MONO};cursor:pointer`))),
          h('div', { style: 'display:flex;gap:8px;flex-wrap:wrap' }, formats.map(([id, desc]) => h('div', { key: id, onClick: () => A.set({ designFmt: id }), style: `padding:8px 12px;border:1px solid ${S.designFmt === id ? '#965cde' : '#2a2a2a'};background:${S.designFmt === id ? '#965cde14' : '#18191b'};border-radius:6px;cursor:pointer;display:flex;flex-direction:column;gap:2px;min-width:150px` },
            h('span', { style: `font:12px ${MONO};color:#edeef0` }, id), h('span', { style: 'font-size:11px;color:#757575' }, desc))))),
        h('div', { onMouseDown: A.orbitStart, onWheel: A.onWheel, onContextMenu: A.noMenu, onDblClick: A.focus, style: `background:radial-gradient(ellipse 70% 60% at 50% 45%,#16131d 0%,#0b0b0d 60%,#080808 100%);border:1px solid #1f1f1f;border-radius:8px;flex:1;min-height:0;overflow:hidden;position:relative;cursor:${S.dragging === 'pan' ? 'move' : view === 'iso' ? (S.dragging ? 'grabbing' : 'grab') : 'move'}` },
          V.infraPopover(A, M),
          h('div', { ref: A.floorRef, style: 'position:absolute;inset:0;overflow:auto' }, view === 'iso' ? V.isoScene(A, iso, true) : V.floor2d(A, M, rows, true)))),
      h('div', { style: `${CARD};display:flex;flex-direction:column;min-height:0;overflow:hidden` },
        h('div', { style: 'display:flex;align-items:center;gap:8px;padding:10px 12px;border-bottom:1px solid #1f1f1f' },
          h('span', { style: 'font-weight:600;color:#edeef0' }, 'Draft'), h('span', { style: 'font-size:12px;color:#757575' }, `${S.draft.length} change${S.draft.length === 1 ? '' : 's'}${removes.length ? ` · ${adds.length} added, ${removes.length} removed` : ''}`),
          h('div', { style: 'flex:1' }),
          secondary(S.confirmApply ? 'Cancel' : 'Discard', () => A.set(S.confirmApply ? { confirmApply: false } : { draft: [], applyLog: null })),
          h('span', { title: src.caps.design ? (S.confirmApply ? 'the estate goes down and comes up again on the changed file' : '') : 'This twin cannot apply: it was not started from an estate file by dctwin up.' }, primary(S.applying ? 'Applying…' : S.confirmApply ? 'Confirm restart' : 'Apply', apply, !canApply))),
        h('div', { style: 'flex:1;min-height:0;overflow:auto;padding:12px;display:flex;flex-direction:column;gap:12px' },
          selectedRack(A, M),
          h('span', { style: SUB }, 'Diff · estate file'),
          h('pre', { style: `margin:0;padding:10px;background:#0a0a0a;border:1px solid #1f1f1f;border-radius:6px;font:11px/1.6 ${MONO};color:#5cde7b;white-space:pre-wrap` }, diff),
          h('span', { style: SUB }, 'Validate'),
          checks.map(([icon, color, text], i) => h('div', { key: i, style: 'display:flex;gap:8px;font-size:12px' }, h('span', { style: `color:${color};width:12px` }, icon), h('span', { style: 'color:#cfcfcf' }, text))),
          src.caps.design ? null : h('div', { style: 'font-size:11px;color:#757575;line-height:1.5;padding:8px 10px;background:#18191b;border-radius:6px' }, 'This twin cannot apply a draft: it was not started from an estate file by dctwin up. Copy the diff into the estate file and bring the estate up again.'),
          S.applyLog ? [h('span', { style: `${SUB};margin-top:6px` }, src.kind === 'live' ? 'Applying · estate restart' : 'Applying · hot change'),
            h('div', { style: `display:flex;flex-direction:column;gap:4px;font:11px ${MONO}` }, S.applyLog.map((l, i) => { const bad = /^✕/.test(l), wait = /^…/.test(l); return h('span', { key: i, style: `color:${bad ? '#ec7575' : wait ? '#757575' : '#b3b3b3'};overflow-wrap:anywhere` }, bad || wait ? l : `✓ ${l}`); }))] : null)));
  }

  // The rack selected on the Design floor, and what the draft can do with it.
  function selectedRack(A, M) {
    const S = A.S, src = A.src, R = S.designSel && src.racks[S.designSel];
    if (!R) return h('div', { style: 'font-size:11.5px;color:#757575;line-height:1.5;padding:8px 10px;background:#18191b;border-radius:6px' }, 'Click an empty position to add a rack of the selected format, or click a rack to select it and remove it.');
    const rm = S.draft.find((x) => x.op === 'remove' && x.name === R.name);
    const a = M.aggs[R.name] || { w: 0 };
    const n = R.slots.length + R.sensors.length;
    const toggle = () => A.set({ draft: rm ? S.draft.filter((x) => x !== rm) : [...S.draft, { op: 'remove', name: R.name, row: R.row, pos: R.pos, fmt: R.title }] });
    return h('div', { style: `display:flex;flex-direction:column;gap:8px;padding:10px;border:1px solid ${rm ? '#ec757566' : '#965cde66'};background:${rm ? '#861a1a1a' : '#965cde0d'};border-radius:6px` },
      h('div', { style: 'display:flex;align-items:center;gap:8px' },
        h('span', { style: SUB }, 'Selected rack'),
        h('span', { style: `font-weight:600;color:${rm ? '#ec7575' : '#edeef0'};${rm ? 'text-decoration:line-through' : ''}` }, R.name),
        h('div', { style: 'flex:1' }),
        h('button', { onClick: () => A.set({ designSel: null }), title: 'deselect', className: 'hv-text', style: 'background:none;border:0;color:#757575;cursor:pointer;font-size:14px;line-height:1;padding:0 2px' }, '×')),
      h('div', { style: `font:11px ${MONO};color:#b3b3b3;line-height:1.6` }, `${R.title}`, h('br'), `row ${R.row} · position ${R.pos} · ${n} devices · ${kw(a.w)}`),
      h('div', { style: 'display:flex;align-items:center;gap:8px' },
        h('button', { onClick: toggle, className: 'hv-bright', style: `background:${rm ? '#18191b' : '#861a1a44'};border:1px solid ${rm ? '#2a2a2a' : '#ec757566'};border-radius:6px;padding:5px 12px;font-size:12px;color:${rm ? '#cfcfcf' : '#ec7575'};cursor:pointer` }, rm ? 'Keep rack' : 'Remove rack'),
        h('span', { style: 'font-size:11px;color:#757575' }, rm ? 'marked for removal in this draft' : 'adds its removal to the draft; nothing changes until Apply')));
  }

  Object.assign(TC.views, { topology, facility, scenarios, design });
})();
