// scenarios.js — the scenario runner (spec 08): timed steps, waits and assertions evaluated against the
// model, the event log and the stack's declared accessors; a result always carries its fidelity report.
window.DCT = window.DCT || {};
DCT.scenarios = (() => {
  const U = DCT.util;
  const E = () => DCT.engine, S = () => DCT.stack;
  const H = 'site-1/hall-a';
  const trays = (rack, from, to) => { const out = []; for (let i = from; i <= to; i++) out.push(`${H}/${rack}/compute-tray-${String(i).padStart(2, '0')}`); return out; };
  const since = (run) => E().events.filter((e) => e.seq > run.startSeq);
  const reqOn = (run, path, re) => since(run).some((e) => e.kind === 'request' && e.path === path && re.test(e.msg) && /stack/.test(e.source));
  const stackEv = (run, re, path) => since(run).some((e) => (e.kind === 'stack' || e.kind === 'k8s') && re.test(e.msg) && (!path || e.path === path));
  const machine = (path) => S().machines[path]?.state;

  const list = [
    {
      name: 'nvl72-leak-response', milestone: 'M5', title: 'A leak in an NVL72 rack',
      description: 'A leak at leak-sensor-02 of a03 must reach the BMS topic, the stack must shut down the trays its (illustrative) gravity-aware policy names, and the trays above must be left alone.',
      uses: [`${H}/a03/**`],
      yaml: `apiVersion: dc-twin/v1alpha1\nkind: Scenario\nmetadata: { name: nvl72-leak-response }\nspec:\n  estate: ../estates/gb300-nvl72-rack.yaml\n  stack: { name: nvidia-dsx-os, components: [nico, dsx-exchange] }\n  timeout: 30m\n  steps:\n    - wait: { for: "stack.nico.machines(rack=a03).state == Ready", within: 20m }\n    - fault: { select: site-1/hall-a/a03/leak-sensor-02, inject: generic/leak/tray-level }\n    - expect:\n        within: 2m\n        all:\n          - observe: { bms: "BMS/v1/PUB/Value/Sensor/Leak/**", value: true }\n          - observe: { redfish: "a03/compute-tray-{09..12}", powerState: "Off" }\n          - observe: { event: "request power.off", from: stack, on: "a03/compute-tray-{09..12}" }\n          - not:     { event: "request power.off", on: "a03/compute-tray-{01..08}" }\n    - fault: { select: site-1/hall-a/a03/leak-sensor-02, clear: true }\n  report: fidelity`,
      steps: (run) => [
        { type: 'wait', desc: 'stack.nico.machines(rack=a03).state == Ready', within: 60, check: () => trays('a03', 1, 18).every((p) => ['Ready', 'Unhealthy'].includes(machine(p))) && !E().dev(`${H}/a03/leak-sensor-02`).leak },
        { type: 'act', desc: 'fault: inject generic/leak/tray-level on a03/leak-sensor-02', act: () => E().inject('generic/leak/tray-level', `${H}/a03/leak-sensor-02`, { source: 'scenario:nvl72-leak-response' }) },
        { type: 'expect', desc: 'within 2 m', within: 120, all: [
          { desc: 'bms BMS/v1/PUB/Value/Sensor/Leak/** == true', check: () => Object.entries(E().model.facility.bms.points).some(([k, v]) => /Leak/.test(k) && v.value === true) },
          { desc: 'redfish a03/compute-tray-{09..12} PowerState == Off', check: () => trays('a03', 9, 12).every((p) => E().dev(p).power.state === 'Off') },
          { desc: 'event: request power.off from stack on a03/compute-tray-{09..12}', check: () => trays('a03', 9, 12).every((p) => reqOn(run, p, /ComputerSystem\.Reset/)) },
          { desc: 'not: request power.off on a03/compute-tray-{01..08}', check: () => !trays('a03', 1, 8).some((p) => reqOn(run, p, /ComputerSystem\.Reset/)), negative: true },
        ] },
        { type: 'act', desc: 'fault: clear the leak', act: () => { const i = E().activeFaults().find((f) => f.target === `${H}/a03/leak-sensor-02`); if (i) E().clearFault(i.id, { source: 'scenario:nvl72-leak-response' }); } },
      ],
    },
    {
      name: 'xid-cordon', milestone: 'M3', title: 'An Xid 79 on a vm tray is cordoned, drained and quarantined',
      description: 'NVSentinel (vm host) must cordon and drain the node, request a reboot through the BMC, and quarantine the node when the fault survives the reboot. Then a technician AC-cycles the tray and the fault clears.',
      uses: [`${H}/a03/compute-tray-01`],
      yaml: `steps:\n  - fault: { select: a03/compute-tray-01/gpu-2, inject: nvidia/xid/xid-79 }\n  - expect: { within: 1m, all: [ { observe: { k8s: "cordon node *" } }, { observe: { event: "request ComputerSystem.Reset ForceRestart", from: stack } } ] }\n  - expect: { within: 2m, all: [ { observe: { stack: "machines(a03/compute-tray-01).state == Quarantined" } } ] }\n  - act:    { power: ACCycle, select: a03/compute-tray-01 }\n  - expect: { within: 1m, all: [ { observe: { faults: "a03/compute-tray-01/**", count: 0 } } ] }`,
      steps: (run) => {
        const tray = `${H}/a03/compute-tray-01`, gpu = `${tray}/gpu-2`;
        return [
          { type: 'wait', desc: 'tray a03/compute-tray-01 is On and healthy', within: 30, check: () => E().dev(tray).power.state === 'On' && E().dev(tray).health === 'OK' },
          { type: 'act', desc: 'fault: inject nvidia/xid/xid-79 on a03/compute-tray-01/gpu-2', act: () => E().inject('nvidia/xid/xid-79', gpu, { source: 'scenario:xid-cordon' }) },
          { type: 'expect', desc: 'within 1 m', within: 60, all: [
            { desc: 'k8s: cordon node', check: () => stackEv(run, /cordon node/, tray) },
            { desc: 'request ComputerSystem.Reset ForceRestart from stack', check: () => reqOn(run, tray, /ComputerSystem\.Reset/) },
          ] },
          { type: 'expect', desc: 'within 2 m', within: 120, all: [{ desc: 'stack: machine Quarantined (fault persists after reboot)', check: () => machine(tray) === 'Quarantined' }] },
          { type: 'act', desc: 'technician: AC cycle a03/compute-tray-01', act: () => E().power(tray, 'ACCycle', { source: 'scenario:xid-cordon' }) },
          { type: 'expect', desc: 'within 1 m', within: 60, all: [{ desc: 'faults on a03/compute-tray-01/** == 0', check: () => !E().activeFaults().some((f) => f.device === tray) }] },
        ];
      },
    },
    {
      name: 'bmc-unreachable', milestone: 'M1', title: 'A BMC stops answering',
      description: 'After three failed polls the stack must mark the machine Unreachable; when the BMC returns, the machine must come back.',
      uses: [`${H}/a05/server-02`],
      yaml: `steps:\n  - fault:  { select: a05/server-02, inject: generic/bmc/unreachable }\n  - expect: { within: 2m, all: [ { observe: { stack: "machines(a05/server-02).state == Unreachable" } } ] }\n  - fault:  { select: a05/server-02, clear: true }\n  - expect: { within: 1m, all: [ { observe: { stack: "machines(a05/server-02).state == Ready" } } ] }`,
      steps: (run) => { const d = `${H}/a05/server-02`; return [
        { type: 'act', desc: 'fault: inject generic/bmc/unreachable on a05/server-02', act: () => E().inject('generic/bmc/unreachable', d, { source: 'scenario:bmc-unreachable' }) },
        { type: 'expect', desc: 'within 2 m (three 30 s polls)', within: 125, all: [{ desc: 'stack: machine Unreachable', check: () => machine(d) === 'Unreachable' }] },
        { type: 'act', desc: 'fault: clear', act: () => { const i = E().activeFaults().find((f) => f.target === d && f.fault === 'generic/bmc/unreachable'); if (i) E().clearFault(i.id, { source: 'scenario:bmc-unreachable' }); } },
        { type: 'expect', desc: 'within 1 m', within: 65, all: [{ desc: 'stack: machine Ready', check: () => machine(d) === 'Ready' }] },
      ]; },
    },
    {
      name: 'firmware-drift-quarantine', milestone: 'M1', title: 'Firmware drifts from the baseline',
      description: 'The stack must quarantine the machine, stage an update through UpdateService, reset to apply it, and return the machine to Ready.',
      uses: [`${H}/a06/server-01`],
      yaml: `steps:\n  - fault:  { select: a06/server-01, inject: generic/firmware/drift }\n  - expect: { within: 1m, all: [ { observe: { stack: "machines(a06/server-01).state == Quarantined" } }, { observe: { event: "request UpdateService.SimpleUpdate", from: stack } } ] }\n  - expect: { within: 2m, all: [ { observe: { redfish: "a06/server-01", firmware: in-baseline } }, { observe: { stack: "machines(a06/server-01).state == Ready" } } ] }`,
      steps: (run) => { const d = `${H}/a06/server-01`; return [
        { type: 'act', desc: 'fault: inject generic/firmware/drift on a06/server-01', act: () => E().inject('generic/firmware/drift', d, { source: 'scenario:firmware-drift' }) },
        { type: 'expect', desc: 'within 1 m', within: 65, all: [
          { desc: 'stack: machine Quarantined', check: () => machine(d) === 'Quarantined' || stackEv(run, /Quarantined/, d) },
          { desc: 'request UpdateService.SimpleUpdate from stack', check: () => reqOn(run, d, /SimpleUpdate/) },
        ] },
        { type: 'expect', desc: 'within 2 m', within: 120, all: [
          { desc: 'redfish FirmwareInventory all in baseline', check: () => E().dev(d).firmware.every((f) => f.status === 'in-baseline') },
          { desc: 'stack: machine Ready', check: () => machine(d) === 'Ready' },
        ] },
      ]; },
    },
    {
      name: 'switch-tray-failure', milestone: 'M4', title: 'An NVLink switch tray fails',
      description: 'The domain must degrade and the stack must block allocation; replacing the tray must restore the domain.',
      uses: [`${H}/a02/**`],
      yaml: `steps:\n  - fault:  { select: a02/switch-tray-04, inject: nvidia/nvlink/switch-tray-failure }\n  - expect: { within: 30s, all: [ { observe: { domain: "a02/nvlink", state: Degraded } }, { observe: { stack: "allocation blocked for a02" } } ] }\n  - fault:  { select: a02/switch-tray-04, clear: true }\n  - expect: { within: 30s, all: [ { observe: { domain: "a02/nvlink", state: Ready } } ] }`,
      steps: (run) => { const d = `${H}/a02/switch-tray-04`; const dom = () => E().rack(`${H}/a02`).domains.find((x) => x.kind === 'nvlink'); return [
        { type: 'act', desc: 'fault: inject nvidia/nvlink/switch-tray-failure on a02/switch-tray-04', act: () => E().inject('nvidia/nvlink/switch-tray-failure', d, { source: 'scenario:switch-tray' }) },
        { type: 'expect', desc: 'within 30 s', within: 30, all: [{ desc: 'domain a02/nvlink Degraded', check: () => dom().state === 'Degraded' }, { desc: 'stack: allocation blocked for a02', check: () => stackEv(run, /allocation blocked/, `${H}/a02`) }] },
        { type: 'act', desc: 'fault: clear (tray replaced)', act: () => { const i = E().activeFaults().find((f) => f.target === d); if (i) E().clearFault(i.id, { source: 'scenario:switch-tray' }); } },
        { type: 'expect', desc: 'within 30 s', within: 30, all: [{ desc: 'domain a02/nvlink Ready', check: () => dom().state === 'Ready' }] },
      ]; },
    },
  ];

  const runs = {};
  function run(name) {
    const sc = list.find((s) => s.name === name); if (!sc) return;
    const R = { name, status: 'running', started: new Date(E().now), startSeq: E().seq, current: 0, steps: [], result: null, report: null };
    R.plan = sc.steps(R);
    R.steps = R.plan.map((s) => ({ desc: s.desc, status: 'pending', started: null, finished: null, checks: (s.all || []).map((c) => ({ desc: c.desc, ok: null })) }));
    runs[name] = R;
    E().emit({ kind: 'scenario', path: 'scenario/' + name, msg: `scenario ${name} started`, source: 'scenario runner' });
    advance(R);
    return R;
  }
  function advance(R) {
    while (R.current < R.plan.length) {
      const s = R.plan[R.current], st = R.steps[R.current];
      if (st.status === 'pending') { st.status = 'running'; st.started = new Date(E().now); }
      if (s.type === 'act') { try { s.act(); st.status = 'passed'; } catch (err) { st.status = 'failed'; st.detail = String(err); return finish(R, 'failed'); } st.finished = new Date(E().now); R.current++; continue; }
      return; // wait/expect evaluate on tick
    }
    finish(R, 'passed');
  }
  function tick() {
    for (const R of Object.values(runs)) {
      if (R.status !== 'running') continue;
      const s = R.plan[R.current], st = R.steps[R.current]; if (!s) continue;
      const elapsed = (E().now - st.started) / 1000;
      if (s.type === 'wait') { if (s.check()) { st.status = 'passed'; st.finished = new Date(E().now); R.current++; advance(R); } else if (elapsed > s.within) { st.status = 'failed'; st.detail = `not true within ${s.within} s`; finish(R, 'failed'); } }
      if (s.type === 'expect') {
        let allOk = true;
        s.all.forEach((c, i) => { const ok = !!c.check(); st.checks[i].ok = ok; if (!ok) allOk = false; });
        const negativesHold = s.all.every((c, i) => !c.negative || st.checks[i].ok);
        if (!negativesHold) { st.status = 'failed'; st.detail = 'a `not` assertion was violated'; finish(R, 'failed'); continue; }
        if (allOk && elapsed >= Math.min(5, s.within)) { st.status = 'passed'; st.finished = new Date(E().now); R.current++; advance(R); }
        else if (elapsed > s.within) { st.status = 'failed'; st.detail = `not all observed within ${s.within} s`; finish(R, 'failed'); }
      }
    }
  }
  function finish(R, status) {
    R.status = status; R.finished = new Date(E().now);
    const sc = list.find((s) => s.name === R.name);
    const used = sc.uses.flatMap((sel) => E().model.order.filter((p) => U.matchSelector(sel, p) || U.matchSelector(sel.replace(/\/\*\*$/, ''), p)));
    R.report = E().fidelityReport(used.length ? used : E().model.order.slice(0, 20));
    if (status === 'passed' && R.report.impersonated.length) { R.status = status = 'impersonated'; }
    E().emit({ kind: 'scenario', path: 'scenario/' + R.name, msg: `scenario ${R.name} ${status.toUpperCase()}${status === 'passed' ? ' (conditional on its fidelity report)' : status === 'impersonated' ? ' (ran with an impersonator: a scale run, not a conformance pass)' : ''}`, source: 'scenario runner' });
  }

  // "Save as scenario": the session's user acts, as a scenario file to add assertions to (12 — Events).
  function fromSession() {
    const E0 = E(); const t0 = E0.sessionStart;
    const lines = ['apiVersion: dc-twin/v1alpha1', 'kind: Scenario', `metadata: { name: session-${t0.toISOString().slice(0, 19).replace(/[:T]/g, '-')} }`, 'spec:', '  estate: ../estates/hall-a-demo.yaml', '  stack: { name: nvidia-dsx-os }', '  steps:'];
    let last = t0;
    for (const a of E0.session) {
      const dt = Math.round((a.t - last) / 1000); last = a.t;
      if (dt > 0) lines.push(`    - wait: { seconds: ${dt} }`);
      if (a.type === 'fault') lines.push(`    - fault: { select: ${a.target.replace('site-1/hall-a/', '')}, inject: ${a.fault} }`);
      if (a.type === 'clear') lines.push(`    - fault: { select: ${a.target.replace('site-1/hall-a/', '')}, clear: true }`);
      if (a.type === 'power') lines.push(`    - act: { power: ${a.action}, select: ${a.path.replace('site-1/hall-a/', '')} }`);
      if (a.type === 'cable') lines.push(`    - act: { cable: ${a.state}, link: "${a.link}" }`);
    }
    lines.push('    # add expect: blocks for what the stack must have done', '  report: fidelity');
    return lines.join('\n');
  }

  return { list, runs, run, tick, fromSession };
})();
