// Login card only — probes /api/health; if authed via cookie, app.js takes over.
(() => {
  const form = document.getElementById('login-form');
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const pw = document.getElementById('pw').value;
    const err = document.getElementById('login-err');
    err.textContent = '';
    const r = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: pw }),
    });
    if (r.ok) { location.reload(); return; }
    try {
      const j = await r.json();
      err.textContent = j.retry_in ? `locked — retry in ${j.retry_in}` : (j.error || 'wrong password');
    } catch { err.textContent = 'login failed'; }
    document.getElementById('pw').select();
  });
})();
