// QFN-PGX console — vanilla ES, no build. SSE multiplex on /api/events;
// initial paint from /api/state.
const $ = (id) => document.getElementById(id);

let state = { hist: { gen: [], prompt: [], gpu: [], kv: [] } };

function fmtGib(kib) { return (kib / 1048576).toFixed(1) + ' GiB'; }
function fmtNum(v, d = 1) { return (v ?? 0).toFixed(d); }
function fmtBytes(b) {
  if (b > 1e9) return (b / 1e9).toFixed(2) + ' GB/s';
  if (b > 1e6) return (b / 1e6).toFixed(1) + ' MB/s';
  if (b > 1e3) return (b / 1e3).toFixed(0) + ' kB/s';
  return Math.round(b) + ' B/s';
}

// ---- boot ----
async function boot() {
  const r = await fetch('/api/state', { headers: { accept: 'application/json' } });
  if (r.status === 401) { $('login').classList.remove('hidden'); return; }
  if (!r.ok) { $('login-err').textContent = 'console unreachable'; $('login').classList.remove('hidden'); return; }
  $('login').classList.add('hidden');
  $('app').classList.remove('hidden');
  const st = await r.json();
  paintState(st);
  connectEvents();
  connectLogs();
  setInterval(() => { $('clock').textContent = new Date().toLocaleTimeString(); }, 1000);
}

function connectEvents() {
  const es = new EventSource('/api/events');
  es.addEventListener('snapshot', (e) => paintSnapshot(JSON.parse(e.data)));
  es.addEventListener('requests', (e) => paintRequests(JSON.parse(e.data)));
  es.addEventListener('ops', (e) => paintOps(JSON.parse(e.data)));
  es.onerror = () => { $('op-status').textContent = 'events lost — reconnecting…'; };
}

function connectLogs() {
  const es = new EventSource('/api/engine/logs');
  const pre = $('logs');
  es.onmessage = (e) => {
    pre.textContent += e.data + '\n';
    if (pre.textContent.length > 120000) pre.textContent = pre.textContent.slice(-90000);
    pre.scrollTop = pre.scrollHeight;
  };
  es.onerror = () => { pre.textContent = ''; };
}

// ---- controls ----
async function engineOp(kind) {
  const btns = [$('btn-up'), $('btn-restart'), $('btn-down')];
  btns.forEach((b) => (b.disabled = true));
  try {
    const r = await fetch('/api/engine/' + kind, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ profile: $('profile').value || null }),
    });
    if (r.status === 409) { $('op-status').textContent = 'busy: ' + (await r.json()).error; }
    else if (!r.ok) { $('op-status').textContent = (await r.json()).error; }
    else { $('op-status').textContent = kind + ' accepted…'; }
  } finally { setTimeout(() => btns.forEach((b) => (b.disabled = false)), 400); }
}
$('btn-up').onclick = () => engineOp('up');
$('btn-restart').onclick = () => confirm('Restart the engine? In-flight requests die and boot takes 8–13 min.') && engineOp('restart');
$('btn-down').onclick = () => confirm('Stop the engine? Nothing will be serving until you start it again.') && engineOp('down');
$('logout').onclick = async () => { await fetch('/api/logout', { method: 'POST' }); location.reload(); };

// ---- painting ----
function paintState(st) {
  if (st.snapshot) paintSnapshot(st.snapshot);
  if (st.requests) paintRequests(st.requests);
  if (st.op) paintOps({ op: st.op, kind: st.op.done ? 'finish' : 'start' });
  const sel = $('profile');
  if (st.profiles && sel.options.length === 0) {
    for (const name of st.profiles) sel.add(new Option(name, name));
    if (st.default_profile) sel.value = st.default_profile;
  }
}

function paintSnapshot(snap) {
  state.snapshot = snap;
  const h = snap.host || {};
  const e = snap.engine || {};
  const g = snap.gpu || {};
  const c = snap.container;

  // banner (backpressure rules from the plan)
  const b = $('banner');
  b.className = 'banner';
  const swapPct = h.swap_total_kib ? (h.swap_used_kib / h.swap_total_kib) * 100 : 0;
  const psiMem = h.psi?.memory?.some_avg10 ?? 0;
  const psiIoFull = h.psi?.io?.full_avg10 ?? 0;
  if (e.reachable && e.kv_usage_pct > 90 || psiIoFull > 10) {
    b.classList.add('bad'); b.textContent = 'BACKPRESSURE — kv ' + fmtNum(e.kv_usage_pct, 0) + '% · io ' + fmtNum(psiIoFull, 0);
  } else if (swapPct > 25 || psiMem > 5) {
    b.classList.add('warn'); b.textContent = 'pressure — swap ' + fmtNum(swapPct, 0) + '% · psi ' + fmtNum(psiMem, 1);
  } else {
    b.classList.add('ok'); b.textContent = 'nominal';
  }

  // engine pill
  const pill = $('engine-state');
  if (!c) { pill.className = 'pill down'; pill.textContent = 'engine: down'; }
  else if (!e.reachable) { pill.className = 'pill booting'; pill.textContent = 'engine: booting…'; }
  else { pill.className = 'pill up'; pill.textContent = 'engine: up'; }

  // model panel
  const m = state.meta || {};
  $('model-body').innerHTML = kvRows([
    ['served', e.reachable ? (m.model_name || 'qwen3.8-flash-next') : '—'],
    ['mode', m.mode || '—'],
    ['ctx', m.ctx || '—'],
    ['mtp', m.mtp ?? '—'],
    ['prefix cache', m.prefix_cache == null ? '—' : (m.prefix_cache ? 'on' : 'off')],
    ['exact top-k', m.exact_topk == null ? '—' : (m.exact_topk ? 'on (deterministic)' : 'off')],
    ['uptime', h.uptime_s ? upDur(h.uptime_s) : '—'],
    ['console', m.serve_port ? ':' + m.serve_port : '—'],
  ]);

  // performance panel
  $('perf-body').innerHTML = kvRows([
    ['decode', fmtNum(e.gen_tok_per_s, 1) + ' tok/s'],
    ['prefill', fmtNum(e.prompt_tok_per_s, 0) + ' tok/s'],
    ['running / waiting', (e.running ?? 0) + ' / ' + (e.waiting ?? 0) + (e.swapped ? (' · swapped ' + e.swapped) : '')],
    ['kv usage', fmtNum(e.kv_usage_pct, 1) + '%'],
    ['prefix hit', fmtNum((e.prefix_hit_ratio || 0) * 100, 0) + '% (window)'],
    ['ttft p50/p90', fmtNum(e.ttft_p50, 2) + ' / ' + fmtNum(e.ttft_p90, 2) + ' s'],
    ['itl p50', (e.itl_p50 ? fmtNum(1 / e.itl_p50, 1) + ' tok/s' : '—')],
    ['gpu util', g.available ? fmtNum(g.util_pct, 0) + '% · ' + fmtNum(g.power_w, 0) + ' W' : 'n/a'],
  ]);

  push(state.hist.gen, e.gen_tok_per_s); push(state.hist.prompt, e.prompt_tok_per_s);
  push(state.hist.gpu, g.util_pct || 0); push(state.hist.kv, e.kv_usage_pct || 0);
  drawSpark();

  // system panel
  const memPct = h.mem_total_kib ? (h.mem_used_kib / h.mem_total_kib) * 100 : 0;
  $('bar-mem').parentElement.classList.toggle('crit', memPct > 92);
  $('bar-mem').style.width = memPct.toFixed(1) + '%';
  $('bar-swap').parentElement.classList.toggle('crit', swapPct > 50);
  $('bar-swap').style.width = swapPct.toFixed(1) + '%';

  $('psi').innerHTML = ['cpu', 'memory', 'io'].map((k) => {
    const v = h.psi?.[k]?.some_avg10 ?? 0;
    const cls = v > 20 ? 'bad' : v > 5 ? 'warn' : 'ok';
    return `<div class="t ${cls}"><div>${k}</div><div class="v">${fmtNum(v, 1)}</div></div>`;
  }).join('');

  const cores = h.cpu?.per_core || [];
  $('cores').innerHTML = cores.map((v) =>
    `<i class="${v > 0.85 ? 'hot' : ''}" style="height:${Math.max(8, v * 100).toFixed(0)}%"></i>`).join('');

  const cv = c ? [
    ['container', fmtGib((c.mem_bytes || 0) / 1024) + ' charged'],
    ['cpu', fmtNum(c.cpu_cores, 1) + ' cores'],
    ['nvme reads', fmtBytes(c.read_bytes_per_s || 0) + ' <span class=dim>(PLE)</span>'],
    ['maj faults', fmtNum(h.maj_fault_per_s, 1) + '/s'],
    ['swap io in/out', fmtNum(h.swap_in_per_s, 0) + ' / ' + fmtNum(h.swap_out_per_s, 0) + ' /s'],
    ['hf disk free', h.hf_free_kib ? fmtGib(h.hf_free_kib) : '—'],
    ['load 1m', fmtNum(h.load1, 2)],
    ['pool', fmtGib((h.mem_available_kib || 0) / 1024) + ' free of ' + fmtGib((h.mem_total_kib || 0) / 1024)],
  ] : [['unified pool', fmtGib((h.mem_available_kib || 0) / 1024) + ' free of ' + fmtGib((h.mem_total_kib || 0) / 1024)],
      ['load 1m', fmtNum(h.load1, 2)]];
  $('sys-kv').innerHTML = kvRows(cv);
}

function paintRequests(rows) {
  const live = rows.filter((r) => !r.done_at);
  const done = rows.filter((r) => r.done_at).slice(0, 8);
  const fmtRow = (r) => `
    <div class="row"><span class="${'st-' + r.phase}">${r.id.replace('r', '#')}</span>
    <span class="st-${r.phase}">${r.phase}</span>
    <span>${r.stream ? 'sse' : 'json'}</span>
    <span>${r.tokens} tok</span>
    <span>${r.prompt_tokens ? r.prompt_tokens + ' p-tok' : ''}</span>
    <span>${r.tps ? fmtNum(r.tps, 1) + ' t/s' : ''}</span>
    <span class="dim">${(r.model || '') + ' ' + (r.client || '')}</span></div>`;
  $('req-counts').textContent = `${live.length} live · ${done.length} recent`;
  $('req-table').innerHTML =
    `<div class="row hd"><span>id</span><span>phase</span><span>type</span><span>gen</span><span>prompt</span><span>speed</span><span>model · client</span></div>` +
    [...live, ...done].map(fmtRow).join('');
}

function paintOps(ev) {
  const op = ev.op || ev;
  if (!op || !op.kind) return;
  const t = (op.done_at ? new Date(op.done_at) : null);
  $('op-status').textContent = op.err ? op.kind + ' failed: ' + op.err
    : op.done ? `${op.kind} by ${op.actor} ✓ ${durBetween(op.started, op.done_at)}`
    : `${op.kind} by ${op.actor}… ${durBetween(op.started, new Date().toISOString())}`;
}

// ---- helpers ----
function kvRows(pairs) {
  return pairs.map(([k, v]) => `<b>${k}</b><span class="num">${v ?? '—'}</span>`).join('');
}
function push(a, v) { a.push(v || 0); if (a.length > 120) a.shift(); }
function upDur(s) { const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600); return (d ? d + 'd ' : '') + h + 'h'; }
function durBetween(a, b) { return Math.max(0, Math.round((new Date(b) - new Date(a)) / 1000)) + 's'; }

const cv = $('perf-canvas');
function drawSpark() {
  const ctx = cv.getContext('2d');
  const W = cv.width, H = cv.height;
  ctx.clearRect(0, 0, W, H);
  const series = [
    [state.hist.gen, '#4cc2ff', 'gen tok/s'],
    [state.hist.gpu, '#3fd08a', 'gpu %'],
    [state.hist.kv, '#f0b429', 'kv %'],
  ];
  for (const [arr, color] of series) {
    const max = Math.max(1, ...arr);
    ctx.strokeStyle = color; ctx.lineWidth = 1.5; ctx.beginPath();
    arr.forEach((v, i) => {
      const x = (i / Math.max(1, 119)) * W;
      const y = H - (v / max) * (H - 6) - 3;
      i ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
    });
    ctx.stroke();
  }
}

boot();
