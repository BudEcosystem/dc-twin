// forms.js — the designer's fields. Text and number fields keep what is typed in an edit buffer
// (A.S.dz.edit) and commit on Enter or leaving the field, so typing is never fought by the
// once-a-second render. Parameter fields are built from a format's or template's declarations:
// an int with a range becomes a stepper, `allowed` a select, a profile a picker filtered by class,
// a rack a rack picker.
window.TC = window.TC || {};
(() => {
  const { h } = TC.h;
  const MONO = 'ui-monospace,Menlo,monospace';
  const LBL = 'font-size:11px;color:#757575;min-width:96px;flex:none';
  const INPUT = `background:#0a0a0a;border:1px solid #2a2a2a;border-radius:5px;color:#e4e4e4;font:11.5px ${MONO};padding:4px 7px;outline:none;min-width:0;flex:1`;

  function row(label, field, hint) {
    return h('div', { style: 'display:flex;align-items:center;gap:8px;min-height:26px' },
      h('span', { style: LBL, title: hint || '' }, label), field);
  }

  // text: value shown from the tree unless being edited; commit(newValue) on Enter / blur
  function text(A, key, value, commit, opts = {}) {
    const ed = A.S.dz.edit;
    const shown = key in ed ? ed[key] : value ?? '';
    const done = (el) => { const v = el.value; delete ed[key]; if (String(v) !== String(value ?? '')) commit(opts.number ? Number(v) : v); A.render(); };
    return h('input', { key, className: 'dz-in', style: INPUT + (opts.width ? `;flex:none;width:${opts.width}px` : ''), type: opts.number ? 'number' : 'text', value: shown, placeholder: opts.placeholder || '',
      min: opts.min, max: opts.max, step: opts.step,
      onInput: (e) => { ed[key] = e.target.value; },
      onKeyDown: (e) => { if (e.key === 'Enter') { e.preventDefault(); e.target.blur(); } if (e.key === 'Escape') { delete ed[key]; e.target.value = value ?? ''; e.target.blur(); } },
      onBlur: (e) => { if (key in ed) done(e.target); } });
  }
  const number = (A, key, value, commit, opts = {}) => text(A, key, value, commit, { ...opts, number: true, width: opts.width || 110 });

  function select(A, key, value, options, commit, opts = {}) {
    const opt = options.map((o) => (Array.isArray(o) ? o : [o, o]));
    if (value !== undefined && value !== '' && !opt.some(([v]) => String(v) === String(value))) opt.unshift([value, `${value} (not in the catalogue)`]);
    return h('select', { key, className: 'dz-in', style: INPUT + ';cursor:pointer', value: value ?? '', onChange: (e) => commit(e.target.value) },
      opts.none ? h('option', { value: '' }, opts.none) : null,
      opt.map(([v, l]) => h('option', { key: String(v), value: v }, l)));
  }

  function toggle(on, onClick, label) {
    return h('button', { onClick, style: `display:flex;align-items:center;gap:6px;background:none;border:0;color:#cfcfcf;font-size:11.5px;cursor:pointer;padding:0` },
      h('span', { style: `width:26px;height:14px;border-radius:999px;background:${on ? '#965cde' : '#2a2a2a'};position:relative;flex:none` }, h('span', { style: `position:absolute;top:2px;left:${on ? 14 : 2}px;width:10px;height:10px;border-radius:50%;background:#fff` })), label);
  }

  function chips(items, onToggle) {
    return h('div', { style: 'display:flex;flex-wrap:wrap;gap:4px' }, items.map(([k, on, label, color]) => h('button', { key: k, onClick: () => onToggle(k, !on),
      style: `background:${on ? '#965cde26' : 'transparent'};color:${on ? '#edeef0' : '#757575'};border:1px solid ${on ? '#965cde' : '#2a2a2a'};border-radius:4px;padding:1px 7px;font:11px ${MONO};cursor:pointer` },
      color ? h('span', { style: `display:inline-block;width:7px;height:7px;border-radius:2px;background:${color};margin-right:5px` }) : null, label || k)));
  }

  // A field for one declared parameter. decl: {type, default, range, allowed, class, label}
  function param(A, key, name, decl, value, commit, ctx) {
    const cat = ctx.catalogue, label = decl.label || name;
    const hint = [decl.type, decl.range ? `range ${decl.range[0]}–${decl.range[1]}` : '', decl.class ? `class ${decl.class}` : '', decl.default !== undefined && decl.default !== '' ? `default ${decl.default}` : ''].filter(Boolean).join(' · ');
    const isDefault = value === undefined;
    const v = isDefault ? decl.default : value;
    let field;
    if (decl.type === 'int' && decl.allowed && decl.allowed.length) field = select(A, key, v, decl.allowed.map((x) => [x, String(x)]), (x) => commit(Number(x)));
    else if (decl.type === 'int') field = number(A, key, v, (x) => commit(Math.round(x)), { min: decl.range && decl.range[0], max: decl.range && decl.range[1] });
    else if (decl.type === 'profile') {
      const ps = (cat.profiles || []).filter((p) => !decl.class || p.class === decl.class).map((p) => [p.name, `${p.name}${p.pinned ? '' : ' (pack not pinned)'}`]);
      field = select(A, key, v || '', ps, (x) => commit(x), { none: '— none —' });
    } else if (decl.type === 'rack') field = select(A, key, v || '', (ctx.racks || []).map((r) => [r, r]), (x) => commit(x), { none: '— none —' });
    else field = text(A, key, v, commit);
    return h('div', { key: 'p' + key, style: 'display:flex;align-items:center;gap:8px;min-height:26px' },
      h('span', { style: LBL, title: hint }, label, isDefault ? h('span', { style: 'color:#4d4d4d' }, ' · default') : null), field,
      !isDefault && decl.default !== undefined ? h('button', { title: `reset to the default (${decl.default})`, onClick: () => commit(undefined), className: 'hv-text', style: 'background:none;border:0;color:#757575;cursor:pointer;font-size:12px;padding:0 2px' }, '↺') : null);
  }

  function section(title, kids, right) {
    return h('div', { style: 'display:flex;flex-direction:column;gap:6px;padding:10px 12px;border-bottom:1px solid #1a1a1a' },
      h('div', { style: 'display:flex;align-items:center;gap:8px' }, h('span', { style: 'font-size:10.5px;color:#757575;text-transform:uppercase;letter-spacing:.06em' }, title), h('div', { style: 'flex:1' }), right || null),
      kids);
  }
  const btn = (label, onClick, kind, title) => h('button', { onClick, title: title || '', className: kind === 'danger' ? 'hv-act-danger' : 'hv-act',
    style: `background:${kind === 'primary' ? '#1e0c34' : kind === 'danger' ? '#861a1a33' : '#18191b'};border:1px solid ${kind === 'primary' ? '#965cde' : kind === 'danger' ? '#ec757566' : '#2a2a2a'};border-radius:6px;padding:3px 9px;font-size:11.5px;color:${kind === 'danger' ? '#ec7575' : '#cfcfcf'};cursor:pointer;white-space:nowrap` }, label);
  const kv = (k, v, color) => h('div', { style: 'display:flex;gap:8px;font-size:11.5px;min-height:20px;align-items:baseline' }, h('span', { style: LBL }, k), h('span', { style: `font:11.5px ${MONO};color:${color || '#cfcfcf'};overflow-wrap:anywhere` }, v));
  const note = (t, color) => h('div', { style: `font-size:11px;color:${color || '#757575'};line-height:1.5` }, t);

  TC.forms = { row, text, number, select, toggle, chips, param, section, btn, kv, note, INPUT, LBL, MONO };
})();
