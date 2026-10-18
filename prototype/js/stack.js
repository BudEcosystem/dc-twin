// stack.js — a SIMULATED software-under-test, standing in for NVIDIA DSX OS (NICo, NVSentinel,
// DSX Exchange) so the console has something to watch respond. It talks to devices only through the
// engine's request path, the way a real stack would reach a BMC, and it is labelled "simulated"
// everywhere it appears. Its policies (leak response, remediation) are illustrative, not NVIDIA's.
window.DCT = window.DCT || {};
DCT.stack = (() => {
  const U = DCT.util, P = DCT.packs;
  const S = { name: 'nvidia-dsx-os', simulated: true, from: 'stack 10.10.0.41', machines: {}, k8s: {}, timers: [], nextLease: 0x21, pollEvery: 30, lastPoll: 0, options: { dpuMode: false, scoutImpersonated: true }, leakWatch: {}, E: null };
  const MANAGED = ['compute-tray', 'server'];

  const after = (sec, fn) => S.timers.push({ at: S.E.now.getTime() + sec * 1000, fn });
  const ev = (path, msg, extra = {}) => S.E.emit({ kind: 'stack', path, msg, source: 'stack (simulated)', ...extra });
  const k8s = (path, msg, extra = {}) => S.E.emit({ kind: 'k8s', path, msg, source: 'nvsentinel (simulated)', ...extra });
  const nodeName = (d) => `node-${d.identity.serial.slice(-6).toLowerCase()}`;
  const setState = (path, state, note) => { const m = S.machines[path]; if (!m) return; const was = m.state; m.state = state; m.since = new Date(S.E.now); if (note) m.notes.unshift({ t: new Date(S.E.now), note }); if (was !== state) ev(path, `machine ${state}${note ? ': ' + note : ''}`, { data: { state } }); };
  const lease = (d) => { if (!d.bmc) return; d.bmc.lease = `10.10.${1 + Math.floor(S.nextLease / 250)}.${S.nextLease % 250 + 2}`; S.nextLease++; return d.bmc.lease; };
  const GET = (d, rpath, cause, quiet = true) => S.E.request(d.path, { method: 'GET', rpath, from: S.from, quiet, cause });
  const sysId = (d) => P.dialects[d.bmc?.dialect || 'generic/openbmc'].systemId;

  S.init = (E) => {
    S.E = E;
    for (const p of E.model.order) {
      const d = E.dev(p); if (d.bmc) lease(d);
      if (MANAGED.includes(d.class)) { S.machines[p] = { state: 'Ready', since: new Date(E.now), notes: [], node: d.fidelity === 'vm' ? nodeName(d) : null }; if (d.fidelity === 'vm') S.k8s[p] = { node: nodeName(d), cordoned: false, pods: 3 }; }
    }
    E.on(S.onEvent);
    ev(E.model.hall.path, `site controller connected: ${Object.keys(S.machines).length} machines Ready; NVSentinel on ${Object.keys(S.k8s).length} vm nodes; DSX Exchange subscribed to BMS/v1/PUB/#`);
  };

  // ------------------------------------------------------------- reactions
  S.onEvent = (e) => {
    const E = S.E;
    if (e.kind === 'state' && e.what === 'health') {
      const d = E.dev(e.path) || E.dev(U.parentPath(e.path));
      if (!d || !MANAGED.includes(d.class)) return;
      const comp = d.components.find((c) => c.path === e.path);
      if (comp && comp.kind === 'accelerator' && /→ (Critical|Warning)$/.test(e.msg)) onAcceleratorFault(d, comp, e);
    }
    if (e.kind === 'bms' && e.data && /\/Leak\//.test(e.data.topic)) onLeak(E.dev(e.path), e.data.value, e);
    if (e.kind === 'state' && e.what === 'nvlink') onNvlink(E.dev(e.path), e);
    if (e.kind === 'state' && e.what === 'link' && e.data?.state === 'down') { const d = E.dev(e.path); if (d && d.class === 'compute-tray' && /bf3/.test(e.msg)) after(2, () => ev(d.path, `DPU agent bf3-0: ${e.msg.split(' → ')[0]} no carrier; HBN BGP session to leaf dropped`, { cause: e.seq })); }
    if (e.kind === 'state' && e.what === 'power' && /→ On$/.test(e.msg)) { const d = E.dev(e.path); if (d && d.attestation === 'fail' && MANAGED.includes(d.class)) after(3, () => setState(d.path, 'Refused', 'attestation failed: PCR 7 does not match the recorded policy; host kept out of the pool')); }
    if (e.kind === 'twin' && e.what === 'racked') for (const dp of e.data.devices) { const d = E.dev(dp); if (d && d.bmc) discover(d, e); }
    if (e.kind === 'twin' && e.what === 'deracked') for (const dp of e.data.devices) { if (S.machines[dp]) { ev(dp, 'machine removed: BMC gone, lease expired'); delete S.machines[dp]; delete S.k8s[dp]; } }
    if (e.kind === 'twin' && e.what === 'reseated') { const d = E.dev(e.path); if (d && d.bmc) { if (d.fidelity === 'vm') S.k8s[d.path] = { node: nodeName(d), cordoned: false, pods: 0 }; else delete S.k8s[d.path]; discover(d, e); } }
  };

  function onAcceleratorFault(d, comp, cause) {
    const E = S.E; const m = S.machines[d.path];
    const critical = comp.health === 'Critical';
    if (d.fidelity === 'vm' && S.k8s[d.path]) {
      const n = S.k8s[d.path];
      after(2, () => k8s(d.path, `node ${n.node}: health event ${comp.name} ${comp.health} (sources: DCGM :5555, journald)`, { cause: cause.seq }));
      if (!critical) { after(5, () => k8s(d.path, `node ${n.node}: ${comp.name} Warning — scheduled GPU reset at next drain window; no cordon`)); return; }
      after(4, () => { n.cordoned = true; k8s(d.path, `cordon node ${n.node} (NoSchedule)`); setState(d.path, 'Unhealthy', `${comp.name} ${comp.health}; node cordoned`); });
      after(7, () => k8s(d.path, `drain node ${n.node}: ${n.pods} pods evicted`));
      after(9, () => { const r = E.redfishAction(d.path, 'Reset', { ResetType: 'ForceRestart' }, { from: S.from, cause: cause.seq }); k8s(d.path, `remediation: node reboot requested (Redfish ForceRestart → ${r.status})`); });
      after(9 + (E.profile(d).behaviours.onSeconds || 8) + (E.profile(d).behaviours.offSeconds || 3) + 3, () => {
        const still = comp.health !== 'OK';
        if (still) { k8s(d.path, `node ${n.node}: fault persists after reboot (${comp.name} ${comp.health}) → node quarantined`); setState(d.path, 'Quarantined', 'break-fix: labelled needs-repair (NICo)'); }
        else { n.cordoned = false; k8s(d.path, `node ${n.node}: healthy after reboot → uncordoned`); setState(d.path, 'Ready', 'remediated by reboot'); }
      });
    } else {
      after(3, () => ev(d.path, `hardware health (Redfish EventService): ${comp.name} ${comp.health}`, { cause: cause.seq }));
      if (critical) after(6, () => setState(d.path, 'Unhealthy', `${comp.name} Critical; break-fix label needs-repair. In-band remediation unavailable: no OS runs at model fidelity`));
    }
  }

  function onLeak(sensor, value, cause) {
    const E = S.E; if (!sensor) return; const rack = E.rack(sensor.rack); if (!rack) return;
    if (!value) { ev(rack.path, `DSX Exchange: leak cleared at ${sensor.name}; trays stay off until an operator restarts them`, { cause: cause.seq }); delete S.leakWatch[sensor.path]; return; }
    after(1, () => ev(rack.path, `DSX Exchange → NICo leak response: ${sensor.name} at level ${sensor.level || '?'} in ${rack.name}`, { cause: cause.seq }));
    const level = E.dev(`${rack.path}/${sensor.level}`); if (!level) return;
    const below = rack.devices.map(E.dev).filter((d) => d.class === level.class && d.slot.u !== null && d.slot.u >= level.slot.u).sort((a, b) => a.slot.u - b.slot.u).slice(0, 4);
    below.forEach((d, i) => after(3 + i, () => { const r = E.redfishAction(d.path, 'Reset', { ResetType: 'GracefulShutdown' }, { from: S.from, cause: cause.seq }); ev(d.path, `leak policy (illustrative, gravity-aware): shut down ${d.name} → ${r.status}`); setState(d.path, 'Leak shutdown', `${sensor.name}`); }));
    S.leakWatch[sensor.path] = cause.seq;
    after(90, () => { if (S.leakWatch[sensor.path] && sensor.leak) ev(rack.path, `leak persists 90 s: rack-level isolation decision for ${rack.name} (non-critical path; critical safety actions stay with the BMS)`); });
  }

  function onNvlink(tray, cause) {
    const E = S.E; const rack = E.rack(tray.rack); const dom = rack.domains.find((d) => d.kind === 'nvlink'); if (!dom) return;
    after(2, () => { if (dom.state === 'Ready') ev(rack.path, `NMX-C: NVLink domain ${rack.name} Ready again (${dom.traysOk}/${dom.traysTotal} switch trays); allocation unblocked`, { cause: cause.seq }); else ev(rack.path, `NMX-C: switch tray ${tray.name} lost; NVLink domain ${rack.name} ${dom.state} (${dom.traysOk}/${dom.traysTotal}) → allocation blocked`, { cause: cause.seq }); });
  }

  // ------------------------------------------------------------- discovery of a new BMC (05 — Boot and addressing)
  function discover(d, cause) {
    const E = S.E; const rack = E.rack(d.rack); const managed = MANAGED.includes(d.class);
    const relay = d.class === 'compute-tray' || d.class === 'switch-tray' || d.class === 'power-shelf' ? `leaf-${rack?.name}-1` : `${rack?.name}/leaf-01`;
    if (managed && !S.machines[d.path]) S.machines[d.path] = { state: 'Discovering', since: new Date(E.now), notes: [], node: d.fidelity === 'vm' ? nodeName(d) : null };
    after(2 + Math.random() * 2, () => { const ip = lease(d); E.emit({ kind: 'dhcp', path: d.path, msg: `DHCPDISCOVER from ${d.bmc.mac} via ${relay} (option 82: link-selection 10.10.0.0/22) → DHCPOFFER ${ip} from ${S.from}`, source: 'dhcp (stack)', cause: cause.seq }); });
    after(4.5, () => { const dl = P.dialects[d.bmc.dialect]; GET(d, '/redfish/v1', cause.seq, false); ev(d.path, `vendor detection: ${dl.root.Vendor} / ${dl.root.Product} → ${d.bmc.dialect} code path`); });
    after(5.5, () => { const id = sysId(d); for (const r of [`/redfish/v1/Systems/${id}`, `/redfish/v1/Chassis/${P.dialects[d.bmc.dialect].chassisId}`, `/redfish/v1/Managers/${P.dialects[d.bmc.dialect].managerId}`, '/redfish/v1/UpdateService/FirmwareInventory']) GET(d, r); ev(d.path, `inventory read: serial ${d.identity.serial}, ${d.components.filter((c) => c.kind === 'accelerator').length} accelerators, ${d.components.filter((c) => c.kind === 'nic').length} NICs, ${d.components.filter((c) => c.kind === 'dpu').length} DPUs; ${d.firmware.length} firmware components`); if (managed) setState(d.path, 'Discovered'); });
    after(7.5, () => {
      const drift = d.firmware.filter((f) => f.status === 'out-of-baseline');
      if (drift.length) { if (managed) setState(d.path, 'Quarantined', `firmware out of baseline: ${drift.map((f) => f.component).join(', ')}`); updateFirmware(d, cause); return; }
      ev(d.path, `firmware baseline: ${d.firmware.length}/${d.firmware.length} components in baseline`);
      if (!managed) { ev(d.path, `${d.class} registered as a rack component`); return; }
      setState(d.path, 'Validating');
      if (d.fidelity === 'vm') {
        const r = E.redfishAction(d.path, 'Reset', { ResetType: 'On' }, { from: S.from, cause: cause.seq }); ev(d.path, `scout: powering on to boot the discovery image (→ ${r.status})`);
        const b = E.profile(d).behaviours; after((b.onSeconds || 8) + (b.postSeconds || 6) + 2, () => {
          if (d.attestation === 'fail') { setState(d.path, 'Refused', 'attestation failed'); return; }
          ev(d.path, `scout: inventory confirmed; machine validation (DCGM diagnostic level 2) pass; attestation accepted`);
          E.redfishAction(d.path, 'Reset', { ResetType: 'GracefulShutdown' }, { from: S.from, cause: cause.seq }); setState(d.path, 'Ready', 'in pool, powered off'); if (!S.k8s[d.path]) S.k8s[d.path] = { node: nodeName(d), cordoned: false, pods: 0 };
        });
      } else {
        after(2, () => { ev(d.path, `scout IMPERSONATED (machine-a-tron): model fidelity runs no OS; validation recorded as impersonated (07)`); setState(d.path, 'Ready', 'in pool, powered off; validation impersonated'); });
      }
    });
  }

  function updateFirmware(d, cause) {
    const E = S.E;
    after(2, () => { const r = E.redfishAction(d.path, 'SimpleUpdate', {}, { from: S.from, cause: cause?.seq }); ev(d.path, `firmware update staged (SimpleUpdate, apply on reset → ${r.status} Task)`); });
    after(5, () => { const r = E.redfishAction(d.path, 'Reset', { ResetType: d.power.state === 'Off' ? 'On' : 'ForceRestart' }, { from: S.from, cause: cause?.seq }); ev(d.path, `firmware task complete; reset to apply (→ ${r.status})`); });
    const b = E.profile(d).behaviours; after(5 + (b.offSeconds || 3) + (b.onSeconds || 8) + 2, () => {
      const drift = d.firmware.filter((f) => f.status !== 'in-baseline');
      if (!drift.length) { ev(d.path, 'firmware back in baseline'); if (S.machines[d.path]) setState(d.path, 'Ready', 'firmware remediated'); }
      else ev(d.path, `firmware still out of baseline: ${drift.map((f) => f.component).join(', ')} — quarantined`);
    });
  }

  // ------------------------------------------------------------- poll
  S.tick = (E) => {
    const now = E.now.getTime();
    const due = S.timers.filter((t) => t.at <= now); S.timers = S.timers.filter((t) => t.at > now);
    for (const t of due) { try { t.fn(); } catch (err) { console.error(err); } }
    if (now - S.lastPoll < S.pollEvery * 1000) return;
    S.lastPoll = now;
    let unreachable = 0, drifted = 0;
    for (const [p, m] of Object.entries(S.machines)) {
      const d = E.dev(p); if (!d) continue;
      const r = GET(d, `/redfish/v1/Systems/${sysId(d)}`);
      if (r.status === 0) { d.bmc.failures++; if (d.bmc.failures === 3) { m.prev = m.state; setState(p, 'Unreachable', 'BMC: 3 consecutive polls timed out'); } unreachable++; continue; }
      if (d.bmc.failures) { d.bmc.failures = 0; if (m.state === 'Unreachable') setState(p, m.prev || 'Ready', 'BMC reachable again'); }
      const drift = d.firmware.filter((f) => f.status === 'out-of-baseline');
      if (drift.length && m.state !== 'Quarantined') { drifted++; setState(p, 'Quarantined', `firmware out of baseline: ${drift.map((f) => f.component + ' ' + f.version + ' ≠ ' + f.baseline).join(', ')}`); updateFirmware(d, null); }
    }
    ev(E.model.hall.path, `health poll: ${Object.keys(S.machines).length} machines; ${unreachable} unreachable; ${drifted} newly out of baseline`, { minor: true });
  };

  S.inputs = () => [['bmcNetworks', 'oob 10.10.0.0/22 (DHCP relayed by the NOS to 10.10.0.41)'], ['hostNetworks', 'provisioning 10.20.0.0/22 (PXE/HTTP boot service 10.20.0.5)'], ['ufmEndpoint', 'https://ufm-01.a10:443/ufmRestV3 (twin adapter)'], ['nmxcEndpoints', 'switch-tray-01 of every NVL rack, :9370 (twin adapter)'], ['tpmCA', 'estate EK CA (dc-twin/hall-a-demo)'], ['bmsBroker', 'mqtt://exchange.svc:1883 (dsx-exchange)'], ['attestation', 'external/stub:nvidia-nras (STUB)']];
  S.capabilities = () => [
    ['redfish/dialect:nvidia-gb', 'met', 'nvidia pack dialect, model fidelity'], ['redfish/dialect:dell-idrac', 'met', 'oem/dell pack'], ['redfish/event-service-sse', 'met', 'core Redfish engine'],
    ['dhcp/relay-option-82', 'met', 'SONiC-VS relays on oob and provisioning'], ['boot/pxe', 'reduced', 'real on 2 vm hosts; boot-walk on model hosts'], ['tpm2/measured-boot', 'reduced', 'swtpm on vm hosts only (Q5)'],
    ['switch/bgp-unnumbered', 'met', 'SONiC-VS (Q3)'], ['switch/evpn', 'met', 'SONiC-VS'], ['fabric/ufm-rest', 'met', 'twin adapter bound to the model'], ['fabric/nmx-c', 'reduced', 'telemetry subscription (Q8)'], ['fabric/rack-management', 'met', 'twin adapter'],
    ['accelerator/nvml', 'reduced', 'Mokka on 2 vm hosts'], ['accelerator/dcgm', 'reduced', 'Mokka on 2 vm hosts'], ['kubernetes/nodes:vm', 'met', '2 nodes'],
    ['facility/bms:dsx-exchange', 'met', 'BMS publisher'], ['facility/grid:dsx-flex', 'met', 'grid publisher'],
    ['dpu/bmc:bluefield', 'met', 'BlueField BMC dialect'], ['dpu/os', 'stub', 'stand-in DPU OS VM (Q1)'], ['dpu/hbn', 'missing', 'no public way to run HBN without hardware (Q1); dpuMode: false'], ['external/stub:nvidia-nras', 'stub', 'attestation stub'],
  ];

  return S;
})();
