// QFN-PGX console — push-first: two SSE streams (/api/events for state,
// /api/engine/logs for the container), one /api/state fetch at boot. Charts
// are vendored uPlot (no build step). Help text lives in HELP below and is
// wired to every [data-help] element — hover '?' , click for the full tour.
'use strict';
const $ = (id) => document.getElementById(id);
const state = { hist: { t: [], gen: [], prompt: [], pct: [] }, rows: {}, meta: {}, status: null };

const fmtGib = (kib) => (kib ? kib / 1048576 : 0).toFixed(1);
const fmtNum = (v, d = 1) => (v ?? null) === null ? '—' : (+v).toFixed(d);
const fmtBytes = (b) => b > 1e9 ? (b / 1e9).toFixed(2) + ' GB/s' : b > 1e6 ? (b / 1e6).toFixed(1) + ' MB/s'
  : b > 1e3 ? (b / 1e3).toFixed(0) + ' kB/s' : Math.round(b) + ' B/s';
const or = (v) => v ?? '—';

// ---------------- boot ----------------
async function boot() {
  let r;
  try { r = await fetch('/api/state', { headers: { accept: 'application/json' } }); }
  catch { $('login-err').textContent = 'console unreachable'; $('login').classList.remove('hidden'); return; }
  if (r.status === 401) { $('login').classList.remove('hidden'); return; }
  if (!r.ok) return setTimeout(boot, 1500);
  $('login').classList.add('hidden'); $('app').classList.remove('hidden');
  paintState(await r.json());
  connectEvents(); connectLogs(); wireUI();
  setInterval(() => { $('clock').textContent = new Date().toLocaleTimeString(); }, 1000);
}

function connectEvents() {
  const es = new EventSource('/api/events');
  es.addEventListener('snapshot', (e) => paintSnapshot(JSON.parse(e.data)));
  es.addEventListener('requests', (e) => paintRequests(JSON.parse(e.data)));
  es.addEventListener('ops', (e) => paintOps(JSON.parse(e.data)));
  es.addEventListener('status', (e) => paintStatus(JSON.parse(e.data)));
  es.onerror = () => { $('op-status').textContent = 'events reconnecting…'; };
}

// ---------------- log pane (no jumping) ----------------
const logsEl = $('logs');
let logs = [], following = true, logSource = null;

function connectLogs() {
  if (logSource) logSource.close();
  logSource = new EventSource('/api/engine/logs');
  logSource.addEventListener('replay', (e) => { logs = JSON.parse(e.data).slice(-3000); renderLogs(true); });
  logSource.onmessage = (e) => { logs.push(e.data); if (logs.length > 3000) logs.splice(0, 500); renderLogs(); };
  logSource.onerror = () => { $('op-status').textContent = 'log stream reconnecting…'; };
}

function logColor(l) {
  const t = esc(l);
  if (/ERROR|Traceback|out of memory|CUDA error|failed/i.test(l)) return `<span class="text-bad">${t}</span>`;
  if (/\bWARN(ING)?\b/i.test(l)) return `<span class="text-warn">${t}</span>`;
  if (/\bDEBUG\b/.test(l)) return `<span class="text-slate-600">${t}</span>`;
  // INFO-shape: dim the timestamp and the level word, keep the payload readable
  return t.replace(/^(.{10,40}?\d{2}:\d{2}:\d{2}[^\s]*\s)/, '<span class="text-slate-600">$1</span>')
          .replace(/\bINFO\b/, '<span class="text-slate-500">INFO</span>');
}
function renderLogs(force) {
  logsEl.innerHTML = logs.map((l) =>
    /^\s*$/.test(l) ? '' : `${logColor(l)}\n`).join('');
  if (following || force) jump();
}
function jump() { logsEl.scrollTop = logsEl.scrollHeight; }
logsEl.addEventListener('scroll', () => {
  const atBottom = logsEl.scrollHeight - logsEl.scrollTop - logsEl.clientHeight < 40;
  if (!atBottom && following) setFollow(false);
});
function setFollow(v) {
  following = v;
  $('btn-follow').textContent = v ? 'following' : 'paused';
  $('btn-follow').classList.toggle('border-warn', !v);
  $('btn-jump').classList.toggle('hidden', v);
}
$('btn-follow').onclick = () => { setFollow(true); jump(); };
$('btn-jump').onclick = () => { setFollow(true); jump(); };
const esc = (s) => s.replace(/[&<>]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c]));

// ---------------- painting ----------------
function paintState(st) {
  state.meta = st.meta || state.meta;
  if (st.snapshot) paintSnapshot(st.snapshot);
  if (st.requests) paintRequests(st.requests);
  if (st.status) paintStatus(st.status);
  if (st.op) paintOps({ op: st.op });
  const sel = $('profile');
  if (st.profiles && sel.options.length === 0) {
    for (const n of st.profiles) sel.add(new Option(n, n));
    if (st.default_profile) sel.value = st.default_profile;
  }
  if (st.keys) $('keys-line').textContent = `machine keys: ${st.keys.map((k) => k.name).join(', ')} · add with \`qfn keys add <name>\``;
}

function paintSnapshot(snap) {
  state.snapshot = snap;
  const h = snap.host || {}, e = snap.engine || {}, g = snap.gpu || {}, c = snap.container;

  // pressure banner — NAME the dominant factor; the word "backpressure" is
  // reserved for when requests are actually being delayed by it.
  const p = $('pill-pressure');
  const swapPct = h.swap_total_kib ? (h.swap_used_kib / h.swap_total_kib) * 100 : 0;
  const psiIo = h.psi?.io?.full_avg10 ?? 0, psiMem = h.psi?.memory?.some_avg10 ?? 0;
  const factors = [];
  if (psiIo > 5) factors.push([psiIo, `io ${fmtNum(psiIo, 0)}`]);
  if (e.kv_usage_pct > 85) factors.push([e.kv_usage_pct / 10, `kv ${fmtNum(e.kv_usage_pct, 0)}%`]);
  if (swapPct > 20) factors.push([swapPct / 10, `swap ${fmtNum(swapPct, 0)}%`]);
  if (psiMem > 4) factors.push([psiMem, `mem ${fmtNum(psiMem, 0)}`]);
  factors.sort((a, b) => b[0] - a[0]);
  const delayed = (e.waiting ?? 0) > 0 || (e.swapped ?? 0) > 0;
  if (factors.length && delayed) {
    p.className = 'pill pill-bad'; p.textContent = `backpressure · ${factors[0][1]}`;
  } else if (factors.length) {
    p.className = 'pill pill-boot'; p.textContent = `pressure · ${factors[0][1]}`;
  } else { p.className = 'pill pill-up'; p.textContent = 'nominal'; }

  // engine pill (boot detail lives in the progress bar, not the pill)
  const up = !!c && e.reachable;
  $('pill-engine').className = 'pill ' + (up ? 'pill-up' : c ? 'pill-boot' : 'pill-down');
  $('engine-state').textContent = up ? 'engine: up' : c ? 'engine: booting' : 'engine: down';
  $('btn-up').disabled = !!c; $('btn-down').disabled = !c;

  // model panel — compact, only things you'd act on
  const m = state.meta;
  $('model-body').innerHTML = up ? kvRows([
    ['model', m.model_name || 'qwen3.8-flash-next'],
    ['mode', m.mode || '—'], ['context', m.ctx || '—'], ['MTP', m.mtp ?? '—'],
    ['running / waiting', (e.running ?? 0) + ' / ' + (e.waiting ?? 0)],
    ['uptime', h.uptime_s ? upDur(h.uptime_s) : '—'],
    ['console', m.serve_port ? ':' + m.serve_port : '—'],
  ]) : `<div class="text-slate-500 text-xs py-1">${c ? 'engine is ' + (state.status?.phase || 'booting') + '…' : 'engine down — <span class=\"text-slate-400\">▶ start</span> when you want it back'}</div>`;

  // perf numbers + chart feed
  $('kv-gen').textContent = fmtNum(e.gen_tok_per_s, 1);
  const genSamples = state.hist.gen.filter((v) => v > 0);
  $('kv-gen-avg').textContent = genSamples.length
    ? fmtNum(genSamples.reduce((a, b) => a + b, 0) / genSamples.length, 1) : '—';
  $('kv-prefill').textContent = fmtNum(e.prompt_tok_per_s, 0);
  $('kv-usage').textContent = fmtNum(e.kv_usage_pct, 1) + '%';
  $('kv-prefix').textContent = e.prefix_hit_ratio != null ? fmtNum(e.prefix_hit_ratio * 100, 0) + '%' : '—';
  const timing = kvRows([
    ['TTFT p50/p90', (e.ttft_p50 || e.ttft_p90) ? `${fmtNum(e.ttft_p50 * 1000, 0)} / ${fmtNum(e.ttft_p90 * 1000, 0)} ms` : '—'],
    ['ITL p50', e.itl_p50 ? fmtNum(e.itl_p50 * 1000, 1) + ' ms' : '—'],
    ['gpu util · power', g.available ? `${fmtNum(g.util_pct, 0)}% · ${fmtNum(g.power_w, 0)} W` : 'n/a'],
    ['queue', (e.waiting ?? 0) > 0 ? `waiting ${e.waiting}` : 'idle'],
  ].map(([k, v]) => [`<span class="text-slate-500">${k}</span>`, v]));
  const sp = e.spec;
  const specBlock = sp?.active ? kvRows([
    ['acceptance', fmtNum(sp.acceptance_pct, 1) + '%'],
    ['accepted', fmtNum(sp.accepted_per_s, 1) + ' tok/s'],
    ['drafted', fmtNum(sp.drafted_per_s, 1) + ' tok/s'],
    ['mean / draft', fmtNum(sp.mean_accepted, 2) + ' tok'],
  ].map(([k, v]) => [`<span class="text-slate-500">${k}</span>`, v]))
    : '<div class="text-[11px] text-slate-600">MTP counters idle — drafts show here once requests flow</div>';
  $('perf-extra').innerHTML =
    `<div><div class="text-[11px] uppercase tracking-widest text-slate-600 mb-1">latency</div>${timing}</div>` +
    `<div><div class="text-[11px] uppercase tracking-widest text-slate-600 mb-1">speculative (MTP)</div>${specBlock}</div>`

  state.hist.t.push(Date.now() / 1000);
  state.hist.gen.push(e.gen_tok_per_s ?? 0);
  state.hist.prompt.push(e.prompt_tok_per_s ?? 0);
  state.hist.pct.push(Math.max(e.kv_usage_pct ?? 0, g.util_pct ?? 0));
  for (const k in state.hist) { if (state.hist[k].length > 150) state.hist[k].shift(); }
  drawChart();

  // system — numbers next to every bar, no bar-only guessing
  const memTot = h.mem_total_kib, memUsed = h.mem_used_kib;
  setBar('bar-mem', memUsed, memTot);
  $('mem-text').textContent = memTot ? `${fmtGib(memUsed)} of ${fmtGib(memTot)} GiB` : '—';
  setBar('bar-swap', h.swap_used_kib, h.swap_total_kib);
  $('swap-text').textContent = h.swap_total_kib ? `${fmtGib(h.swap_used_kib)} of ${fmtGib(h.swap_total_kib)} GiB` : 'none';

  const cores = h.cpu?.per_core || [];
  $('cores').innerHTML = cores.map((v) =>
    `<i class="w-[3px] rounded-sm ${v > 0.85 ? 'bg-bad' : v > 0.5 ? 'bg-warn' : 'bg-good/70'}" style="height:${Math.max(8, v * 100).toFixed(0)}%"></i>`).join('');
  $('load-text').textContent = `load ${fmtNum(h.load1, 2)}`;

  $('psi').innerHTML = ['cpu', 'memory', 'io'].map((k) => {
    const v = h.psi?.[k]?.some_avg10 ?? 0, cls = v > 20 ? 'text-bad' : v > 5 ? 'text-warn' : 'text-good';
    return `<span class="pill pill-down !border-edge"><span class="text-slate-500">${k} stall</span> <b class="${cls} font-mono">${fmtNum(v, 1)}</b></span>`;
  }).join('');

  $('sys-kv').innerHTML = kvRows([
    ['engine memory', c && c.mem_bytes ? fmtGib(c.mem_bytes / 1024) + ' GiB' : 'n/r'],
    ['container cpu', c ? fmtNum(c.cpu_cores, 1) + ' cores' : '—'],
    ['NVMe reads (PLE)', c ? fmtBytes(c.read_bytes_per_s || 0) : '—'],
    ['major faults', fmtNum(h.maj_fault_per_s, 1) + '/s'],
    ['swap in / out', `${fmtNum(h.swap_in_per_s, 0)} / ${fmtNum(h.swap_out_per_s, 0)} p/s`],
    ['HF disk free', h.hf_free_kib ? fmtGib(h.hf_free_kib) + ' GiB' : '—'],
  ]);
}

function setBar(id, used, total) {
  const el = $(id), pct = total ? Math.min(100, (used / total) * 100) : 0;
  el.style.width = pct.toFixed(1) + '%';
  el.style.background = pct > 92 ? '#f16a6a' : pct > 75 ? '#f0b429' : id === 'bar-swap' ? '#f0b429' : '#4cc2ff';
}

function paintStatus(st) {
  state.status = st;
  const wrap = $('boot-wrap');
  const booting = st.running && st.pct < 100 && st.phase !== 'down';
  wrap.classList.toggle('hidden', !booting && st.phase !== 'failed');
  if (st.phase === 'failed') {
    wrap.classList.remove('hidden');
    $('boot-phase').textContent = 'boot failed'; $('boot-phase').className = 'text-bad';
    $('boot-detail').textContent = st.fail_hint || 'check logs below';
    $('boot-bar').style.width = (st.pct || 30) + '%'; $('boot-bar').style.background = '#f16a6a';
    return;
  }
  if (booting) {
    $('boot-phase').textContent = st.phase + '…';
    $('boot-bar').style.width = Math.max(2, st.pct).toFixed(0) + '%';
    $('boot-detail').textContent = st.detail || '';
    $('boot-eta').textContent = st.eta_s > 10 ? `~${Math.round(st.eta_s / 60) + 1} min left` : '';
  }
}

function paintRequests(rows) {
  const live = rows.filter((r) => !r.done_at), done = rows.filter((r) => r.done_at).slice(0, 10);
  rows.forEach((r) => state.rows[r.id] = r);
  $('req-counts').textContent = `${live.length} live · ${done.length} recent`;
  const ph = (r) => ({ queued: 'text-slate-400', prefill: 'text-accent', decoding: 'text-good', done: 'text-slate-500', error: 'text-bad', aborted: 'text-warn' }[r.phase] || 'text-slate-300');
  $('req-table').innerHTML = [...live, ...done].map((r) => `
    <tr class="req border-b border-edge/40" data-id="${r.id}">
      <td class="py-1 font-mono text-slate-500">#${String(r.id).replace('r', '')}</td>
      <td class="${ph(r)}">${r.phase}</td>
      <td class="text-slate-400">${r.stream ? 'sse' : 'json'}${r.endpoint === 'messages' ? '·claude' : ''}</td>
      <td class="text-right font-mono text-slate-500">${r.prompt_tokens || ''}</td>
      <td class="text-right font-mono">${r.tokens || ''}</td>
      <td class="text-right font-mono ${r.tps > 0 ? 'text-good' : ''}">${r.tps ? fmtNum(r.tps, 1) : ''}</td>
      <td class="truncate text-slate-500">${or(r.model)} · ${or(r.client)}</td>
    </tr>`).join('');
}

$('req-table').addEventListener('click', (ev) => {
  const id = ev.target.closest('tr')?.dataset.id; if (!id) return;
  const r = state.rows[id]; if (!r) return;
  $('req-modal-id').textContent = '#' + String(id).replace('r', '');
  $('req-modal-body').innerHTML = kvRows([
    ['endpoint', r.endpoint], ['mode', r.stream ? 'streaming (sse)' : 'single response'],
    ['phase', r.phase], ['client', r.client], ['model', r.model],
    ['prompt tokens', r.prompt_tokens ?? '—'], ['generated', (r.tokens ?? 0) + ' tok'],
    ['speed', r.tps ? fmtNum(r.tps, 1) + ' tok/s' : '—'],
    ['time to first token', r.first_token_at ? ((new Date(r.first_token_at) - new Date(r.started_at)) / 1000).toFixed(2) + ' s' : '—'],
    ['total time', r.done_at ? ((new Date(r.done_at) - new Date(r.started_at)) / 1000).toFixed(2) + ' s' : 'running…'],
    ['status', r.status ? 'upstream HTTP ' + r.status : (r.aborted ? 'client hung up' : 'ok')],
    ['started', new Date(r.started_at).toLocaleTimeString()],
  ]);
  $('req-modal').classList.remove('hidden');
});
$('req-modal-close').onclick = () => $('req-modal').classList.add('hidden');

function paintOps(ev) {
  const op = ev.op; if (!op || !op.kind) return;
  $('op-status').textContent = op.err ? `${op.kind} failed: ${op.err}`
    : op.done ? `${op.kind} by ${op.actor} ✓ ${durBetween(op.started, op.done_at)}`
    : `${op.kind} by ${op.actor}… ${durBetween(op.started, new Date().toISOString())}`;
}

// ---------------- chart (vendored TanStack Charts, vanilla host) ----------------
let chart = null;
function drawChart() {
  const el = $('chart-perf');
  if (!el.clientWidth || !window.QfnChartMod) return;
  const rows = state.hist.t.map((t, i) => ({ t: Math.round(t), gen: state.hist.gen[i], prompt: state.hist.prompt[i], pct: state.hist.pct[i] }));
  try {
    chart ||= QfnChartMod.create(el);
    chart.update(rows);
  } catch { /* chart is a luxury; the numbers above it are not */ }
}
window.addEventListener('resize', () => { if (chart) { const el = $('chart-perf'); el.innerHTML = ''; chart = null; drawChart(); } });

// ---------------- controls ----------------
function wireUI() {
  $('btn-up').onclick = () => engineOp('up');
  $('btn-restart').onclick = () => confirm('Restart the engine? In-flight requests die and boot takes ~10 min.') && engineOp('restart');
  $('btn-down').onclick = () => confirm('Stop the engine? Nothing will be serving until you start it again.') && engineOp('down');
  $('logout').onclick = async () => { await fetch('/api/logout', { method: 'POST' }); location.reload(); };
  $('btn-restart-console').onclick = restartConsole;
  $('btn-help').onclick = () => openHelp(null);
  const drawer = $('pg-drawer');
  const setDrawer = (v) => { drawer.classList.toggle('hidden', !v); if (v) $('pg-msg').focus(); };
  const wire = (id, fn) => { const el = $(id); if (el) el.onclick = fn; };
  wire('btn-play', () => setDrawer(drawer.classList.contains('hidden')));
  wire('btn-pg-close', () => setDrawer(false));
  $('help-close').onclick = () => $('help-modal').classList.add('hidden');
  document.querySelectorAll('[data-help]').forEach((el) =>
    el.addEventListener('click', (e) => { e.stopPropagation(); openHelp(el.dataset.help); }));
  wirePlayground();
}

async function engineOp(kind) {
  const btns = [$('btn-up'), $('btn-restart'), $('btn-down')];
  btns.forEach((b) => (b.disabled = true));
  try {
    const r = await fetch('/api/engine/' + kind, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ profile: $('profile').value || null }) });
    if (r.status === 409) $('op-status').textContent = 'busy: ' + (await r.json()).error;
    else if (!r.ok) $('op-status').textContent = (await r.json()).error || kind + ' refused';
    else $('op-status').textContent = kind + ' accepted…';
  } finally { setTimeout(() => btns.forEach((b) => (b.disabled = false)), 400); }
}

async function restartConsole() {
  if (!confirm('Restart the CONSOLE (not the engine)? This web server restarts itself — a few seconds of downtime. Use it after installing a new qfn version.')) return;
  const r = await fetch('/api/console/restart', { method: 'POST' });
  const { mode, restart_in_ms } = await r.json();
  $('op-status').textContent = `console restarting (${mode})…`;
  setTimeout(() => {
    const tryReload = async () => {
      try { const h = await fetch('/api/health'); if (h.ok) return location.reload(); } catch {}
      setTimeout(tryReload, 1000);
    };
    tryReload();
  }, restart_in_ms || 400);
}

// ---------------- playground ----------------
let pgAbort = null;
function wirePlayground() {
  $('pg-preset').onchange = () => {
    const p = $('pg-preset').value, set = (id, v) => $(id).value = v;
    set('pg-temp', ''); set('pg-topp', ''); set('pg-maxtok', ''); set('pg-think', '');
    if (p === 'deterministic') set('pg-temp', '0');
    if (p === 'creative') { set('pg-temp', '1'); set('pg-topp', '0.95'); }
    if (p === 'fast') { set('pg-maxtok', '64'); set('pg-think', 'off'); }
  };
  $('btn-send').onclick = pgSend;
  $('pg-msg').addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) pgSend(); });
  $('btn-stop').onclick = () => pgAbort?.abort();
  $('pg-clear').onclick = () => { $('pg-out').textContent = ''; $('pg-stats').textContent = ''; };
}

async function pgSend() {
  const text = $('pg-msg').value.trim(); if (!text || pgAbort) return;
  const body = { model: 'qwen3.8-flash-next', stream: true, messages: [] };
  const sys = $('pg-system').value.trim(); if (sys) body.messages.push({ role: 'system', content: sys });
  body.messages.push({ role: 'user', content: text });
  if ($('pg-temp').value !== '') body.temperature = +$('pg-temp').value;
  if ($('pg-topp').value !== '') body.top_p = +$('pg-topp').value;
  if ($('pg-maxtok').value !== '') body.max_tokens = +$('pg-maxtok').value;
  const think = $('pg-think').value; // '', off, low, med, high
  if (think === 'off') body.chat_template_kwargs = { enable_thinking: false };
  else if (think) body.chat_template_kwargs = { enable_thinking: true, thinking_budget: { low: 1024, med: 4096, high: 16384 }[think] };
  body.stream_options = { include_usage: true };

  const out = $('pg-out');
  out.textContent += `\n\n▸ ${text}\n\n`;
  $('pg-msg').value = '';
  $('btn-send').classList.add('hidden'); $('btn-stop').classList.remove('hidden');
  pgAbort = new AbortController();
  const t0 = performance.now(); let ttft = 0, tok = 0, first = false, thinking = true;
  try {
    const r = await fetch('/v1/chat/completions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal: pgAbort.signal });
    if (!r.ok) { out.textContent += `✗ HTTP ${r.status}: ${(await r.json()).error?.message || ''}\n`; return; }
    const rd = r.body.getReader(), td = new TextDecoder(); let buf = '';
    while (true) {
      const { value, done } = await rd.read(); if (done) break;
      buf += td.decode(value, { stream: true });
      const lines = buf.split('\n'); buf = lines.pop();
      for (const ln of lines) {
        if (!ln.startsWith('data:')) continue;
        const d = ln.slice(5).trim(); if (d === '[DONE]') continue;
        let ev; try { ev = JSON.parse(d); } catch { continue; }
        const del = ev.choices?.[0]?.delta || {};
        if (!first && (del.content || del.reasoning_content)) { first = true; ttft = (performance.now() - t0) / 1000; }
        if (del.reasoning_content) { if (thinking) { out.textContent += '◌ '; thinking = false; } out.textContent += del.reasoning_content; }
        if (del.content) { if (thinking) { thinking = false; } out.textContent += del.content; tok++; }
      }
      out.scrollTop = out.scrollHeight;
    }
    const secs = (performance.now() - t0) / 1000;
    $('pg-stats').textContent = `ttft ${ttft.toFixed(2)}s · ${tok} tok · ${(tok / secs).toFixed(1)} tok/s · total ${secs.toFixed(1)}s`;
  } catch (err) {
    if (err.name !== 'AbortError') out.textContent += `\n✗ ${err.message}`;
  } finally {
    pgAbort = null; $('btn-send').classList.remove('hidden'); $('btn-stop').classList.add('hidden');
  }
}

// ---------------- help (beginner-friendly, everywhere) ----------------
const HELP = {
  engine: `<h3 class="text-slate-200">Engine panel</h3>
    <p>The engine is the vLLM server inside a Docker container — the thing that actually runs the model. It takes <b>~10 minutes</b> to boot (weights load from NVMe into the unified memory pool, then CUDA graphs are captured), so the progress bar exists to make that wait legible.</p>
    <p><b>mode nvfp4</b> = 135 GB checkpoint fits memory by streaming expert weights from disk (PLE). <b>hybrid</b> converts the hot half of experts to fp8 first (<code>qfn prepare-hybrid</code>) so less rides over that bus — usually much faster decode.</p>`,
  'engine-buttons': `<h3 class="text-slate-200">start / restart / stop</h3>
    <p><b>start</b> launches with the selected profile. <b>restart</b> = down + up (kills in-flight requests; ~10 min before it serves again). <b>stop</b> frees ALL the memory — the machine feels brand new. Nothing here touches your data; the model just isn't serving while stopped.</p>`,
  profiles: `<h3 class="text-slate-200">Profiles</h3>
    <p>A profile is a saved set of engine choices (context size, MTP speculation, prefix cache, mode) living in <code>~/.config/qfn/profiles/*.toml</code>. Use them to A/B configurations without editing config each time: <code>qfn profile new burst</code>, edit, launch with it selected.</p>`,
  backpressure: `<h3 class="text-slate-200">The status pill</h3>
    <p>The one "should I worry?" answer, naming its dominant cause: <b>nominal</b> → fine. <b>pressure · io 91</b> → a resource is strained (here: disk stalls) but nothing is delayed yet. <b>backpressure</b> → the strain is now <i>delaying requests</i> (something is queued or swapped). The detail lives in the System panel's stall readouts — the pill is just the summary.</p>`,
  perf: `<h3 class="text-slate-200">Performance</h3>
    <p><b>generation</b> = output tokens/s (the typing-speed you feel). <b>prefill</b> = prompt tokens/s read. <b>KV cache</b> = working memory per active conversation; near 100% = queueing. <b>prefix cache hit</b> = share of prompt tokens reused (why turn two is instant).</p>
    <p><b>TTFT</b> = time to first token; <b>ITL</b> = gap between tokens during generation. The <b>speculative</b> block is MTP in numbers: drafts the model guessed ahead and % that stuck — acceptance near 0 with MTP on means drafts are wasted work.</p>`,
  chart: `<p>One line each for decode (green, left axis), prefill (blue, left), KV/GPU% (amber, right, 0-100). Drag to zoom, hover for values. Flat green line at 0 = nothing is generating, not broken.</p>`,
  system: `<h3 class="text-slate-200">System</h3>
    <p>The Spark's 128 GB is <b>unified memory</b> — one pool shared by CPU and GPU alike. Bars always carry used-of-total numbers; a bar without a max is a lie. <b>swap</b> is disk pretending to be RAM — sustained nonzero swap = everything slows; <code>qfn doctor</code> names the culprit. <b>PSI</b> = the kernel admitting tasks are stalled on CPU / memory / IO.</p>`,
  requests: `<h3 class="text-slate-200">Requests</h3>
    <p>Live traffic through the front door: everything — the playground, <code>qfn chat</code>, your agents, Claude-Code-format calls (marked ·claude). Phases: queued → prefill → decoding → done. Click a row for timing details (time-to-first-token is the one to watch for responsiveness).</p>`,
  logs: `<h3 class="text-slate-200">Engine logs</h3>
    <p>Real-time container output — the same lines <code>qfn logs</code> shows, streamed not polled. Scrolling up auto-pauses the follow (nothing yanks the view); press <b>following</b> to re-arm. Errors render red. Boot progress comes from these same lines.</p>`,
  keys: `<h3 class="text-slate-200">Machine keys</h3>
    <p>Named credentials for tools outside this box (opencode, harnesses, scripts): <code>qfn keys add opencode</code> prints a token once; use it as <code>Authorization: Bearer …</code> against <code>http://this-box:8799/v1</code>. Revoking one key (<code>qfn keys rm</code>) doesn't disturb the others — unlike the shared front key.</p>`,
  'restart-console': `<h3 class="text-slate-200">Restart console</h3>
    <p>Restarts just the web server + proxy (NOT the model engine — generations keep running). The process exits and systemd relaunches it, so if you ran <code>sudo make install</code> with a newer build, this picks it up. CLI equivalent: <code>qfn serve restart</code>.</p>`,
  playground: `<h3 class="text-slate-200">Playground</h3>
    <p>Chat while trying settings live: <b>temperature</b> (0 = repeatable, 1 = varied), <b>top_p</b> (sampling cut), <b>max tokens</b> (length cap), <b>thinking budget</b> (how much reasoning the model may spend before answering: off/low/medium/high — more budget, more latency). Presets combine them; <b>engine defaults</b> sends none of the fields so the checkpoint's own defaults decide. Stats show what each combo cost: TTFT + tok/s.</p>
    <p>Images: the engine lane must be launched with images allowed (engine.images in config.toml); the API speaks standard OpenAI/Anthropic multimodal shapes — test from the CLI with <code>qfn chat --image photo.jpg "what is this?"</code>.</p>`,
};
function openHelp(anchor) {
  $('help-body').innerHTML = anchor && HELP[anchor] ? HELP[anchor] : Object.values(HELP).join('<hr class="border-edge">');
  $('help-modal').classList.remove('hidden');
}

// ---------------- helpers ----------------
function kvRows(pairs) {
  return pairs.map(([k, v]) => `<div class="kv"><b>${k}</b><span>${v ?? '—'}</span></div>`).join('');
}
function upDur(s) { const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600); return (d ? d + 'd ' : '') + h + 'h'; }
function durBetween(a, b) { return Math.max(0, Math.round((new Date(b) - new Date(a)) / 1000)) + 's'; }

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { document.querySelectorAll('.fixed.z-50, #pg-drawer').forEach((m) => m.classList.add('hidden')); return; }
  // 'p' toggles the playground — but never while typing
  if (e.key === 'p' && !e.ctrlKey && !e.metaKey && !e.altKey && !/INPUT|TEXTAREA|SELECT/.test(document.activeElement?.tagName || '')) {
    e.preventDefault();
    $('pg-drawer').classList.toggle('hidden');
  }
});
boot();
