/* OpenAI Codex (ChatGPT) accounts panel.
 *
 * Self-contained on purpose: app.js keeps its helpers private. Tokens never reach this page (the
 * API does not send them) and proxies arrive masked. Every state change is a POST. */
(function () {
  'use strict';

  const section = document.getElementById('codex-section');
  if (!section) return;

  const body = document.getElementById('codex-body');
  const countBadge = document.getElementById('codex-count-badge');
  const emptyNote = document.getElementById('codex-empty');
  const addBtn = document.getElementById('codex-add');

  const dlg = {
    el: document.getElementById('codex-dialog'),
    form: document.getElementById('codex-dialog-form'),
    title: document.getElementById('codex-dialog-title'),
    account: document.getElementById('codex-dialog-account'),
    modes: document.getElementById('codex-dialog-modes'),
    modeProfile: document.getElementById('codex-mode-profile'),
    modeLink: document.getElementById('codex-mode-link'),
    modeHint: document.getElementById('codex-mode-profile-hint'),
    poolWrap: document.getElementById('codex-dialog-pool'),
    poolSelect: document.getElementById('codex-dialog-pool-select'),
    input: document.getElementById('codex-dialog-input'),
    error: document.getElementById('codex-dialog-error'),
    link: document.getElementById('codex-dialog-link'),
    urlLabel: document.getElementById('codex-dialog-url-label'),
    url: document.getElementById('codex-dialog-url'),
    copy: document.getElementById('codex-dialog-copy'),
    status: document.getElementById('codex-dialog-status'),
    paste: document.getElementById('codex-dialog-paste'),
    pasteInput: document.getElementById('codex-dialog-paste-input'),
    pasteBtn: document.getElementById('codex-dialog-paste-btn'),
    pasteError: document.getElementById('codex-dialog-paste-error'),
    start: document.getElementById('codex-dialog-start'),
    close: document.getElementById('codex-dialog-close'),
    kind: 'add', // 'add' a new account, or 'proxy' to change one's proxy
    accountID: '',
    busy: false,
    pollTimer: null,
  };

  function esc(s) {
    return String(s === null || s === undefined ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#039;');
  }

  function toast(message, type) {
    const box = document.getElementById('toast-container');
    if (!box) return;
    const t = document.createElement('div');
    t.className = 'toast toast-' + (type || 'info');
    t.setAttribute('role', 'alert');
    t.innerHTML = '<div class="toast-message">' + esc(message) + '</div><button class="toast-close" aria-label="Dismiss notification">&times;</button>';
    const dismiss = () => t.parentNode && t.parentNode.removeChild(t);
    t.querySelector('.toast-close').addEventListener('click', dismiss);
    box.appendChild(t);
    setTimeout(dismiss, 4500);
  }

  async function errorMessage(res) {
    try {
      const d = await res.json();
      return (d.error && (d.error.detail || d.error.message)) || res.statusText;
    } catch (_) {
      return res.statusText;
    }
  }

  async function post(path, payload) {
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload || {}),
    });
  }

  // ---- list ----

  let accounts = [];

  function statusBadge(a) {
    const cls = a.status === 'active' ? 'badge-success' : a.status === 'error' ? 'badge-warning' : 'badge-neutral';
    const label = a.status === 'error' ? 'needs sign-in' : a.status;
    return '<span class="badge ' + cls + '">' + esc(label) + '</span>';
  }

  function untilReset(iso) {
    const ms = new Date(iso).getTime() - Date.now();
    if (isNaN(ms)) return '';
    if (ms <= 0) return 'reset due';
    const m = Math.floor(ms / 60000);
    if (m < 60) return 'resets in ' + m + 'm';
    const h = Math.floor(m / 60);
    if (h < 48) return 'resets in ' + h + 'h' + String(m % 60).padStart(2, '0') + 'm';
    return 'resets in ' + Math.floor(h / 24) + 'd' + (h % 24) + 'h';
  }

  function ago(iso) {
    const s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
    if (isNaN(s) || s < 0) return '';
    if (s < 90) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 172800) return Math.floor(s / 3600) + ' h ago';
    return Math.floor(s / 86400) + ' d ago';
  }

  function meter(label, w) {
    if (!w) return '';
    const pct = Math.max(0, Math.min(100, Number(w.used_percent) || 0));
    const level = pct >= 90 ? ' is-danger' : pct >= 70 ? ' is-warn' : '';
    return '<div class="codex-meter' + level + '">' +
      '<span class="codex-meter-label">' + esc(label) + '</span>' +
      '<span class="codex-meter-track" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + pct + '" aria-label="' + esc(label) + ' used">' +
      '<span class="codex-meter-fill" style="width:' + pct + '%"></span></span>' +
      '<span class="codex-meter-text">' + pct + '% used' + (w.reset_at ? ' · ' + esc(untilReset(w.reset_at)) : '') + '</span></div>';
  }

  function limitsCell(a) {
    const u = a.usage;
    if (!u) return '<span class="codex-limits-note">not read yet</span>';
    const parts = [];
    if (u.limit_reached) parts.push('<span class="badge badge-warning">limit reached</span>');
    parts.push(meter('5h', u.primary) + meter('Week', u.secondary));
    let note = 'read ' + ago(u.fetched_at);
    if (u.unlimited_credits) note += ' · credits: unlimited';
    else if (u.has_credits && u.credit_balance) note += ' · credits: ' + u.credit_balance;
    parts.push('<div class="codex-limits-note">' + esc(note) + '</div>');
    return parts.join('');
  }

  function render() {
    countBadge.textContent = accounts.length + (accounts.length === 1 ? ' account' : ' accounts');
    emptyNote.hidden = accounts.length !== 0;
    body.innerHTML = accounts.map((a) => {
      const proxy = a.proxy_invalid
        ? '<span class="badge badge-warning">unusable proxy</span>'
        : a.proxy_url ? '<span class="mono">' + esc(a.proxy_url) + '</span>' : '<span class="badge badge-warning">none</span>';
      const active = a.is_active ? ' <span class="badge badge-success">in use</span>' : '';
      const handed = a.omniroute_exported_at
        ? ' <span class="badge badge-neutral" title="OmniRoute renews these tokens now. Using them here too would break its session. Sign in again to take the account back.">in OmniRoute</span>'
        : '';
      const lock = a.omniroute_exported_at ? ' disabled' : '';
      return '<tr data-id="' + esc(a.id) + '">' +
        '<td>' + esc(a.email) + active + handed + '</td>' +
        '<td>' + esc(a.plan_type || '-') + '</td>' +
        '<td>' + statusBadge(a) + '</td>' +
        '<td class="codex-limits">' + limitsCell(a) + '</td>' +
        '<td class="proxy-cell">' + proxy + '</td>' +
        '<td class="codex-actions">' +
        '<button type="button" class="btn btn-xs btn-primary" data-act="switch"' + (a.is_active || a.omniroute_exported_at ? ' disabled' : '') + '>Use</button> ' +
        '<button type="button" class="btn btn-xs btn-secondary" data-act="usage"' + lock + ' title="Read this account\'s limits through its proxy">Usage</button> ' +
        '<button type="button" class="btn btn-xs btn-secondary" data-act="refresh"' + lock + ' title="Renew this account\'s tokens">Tokens</button> ' +
        '<button type="button" class="btn btn-xs btn-secondary" data-act="proxy">Proxy</button> ' +
        '<button type="button" class="btn btn-xs btn-danger-subtle" data-act="remove">Remove</button>' +
        '</td></tr>';
    }).join('');
  }

  // Returns false when the feature is off (501) so the section stays hidden.
  async function load() {
    let res;
    try {
      res = await fetch('/api/codex/accounts');
    } catch (_) {
      return false;
    }
    if (res.status === 501) {
      section.hidden = true;
      return false;
    }
    if (!res.ok) return false;
    accounts = await res.json();
    section.hidden = false;
    render();
    return true;
  }

  async function act(btn, name, id) {
    const account = accounts.find((a) => a.id === id);
    const label = account ? account.email : 'account';
    if (name === 'proxy') {
      openDialog('proxy', account);
      return;
    }
    if (name === 'remove') {
      // Two-step confirm: removing forgets the tokens (the Codex CLI's own auth.json is untouched).
      if (btn.dataset.armed !== '1') {
        btn.dataset.armed = '1';
        btn.textContent = 'Confirm?';
        setTimeout(() => { if (btn.isConnected) { btn.dataset.armed = ''; btn.textContent = 'Remove'; } }, 4000);
        return;
      }
    }
    btn.disabled = true;
    try {
      const res = await post('/api/codex/accounts/' + name, { id });
      if (!res.ok) {
        toast(label + ': ' + (await errorMessage(res)), 'error');
      } else if (name === 'switch') {
        toast('Codex CLI now uses ' + label + '. Restart running Codex sessions.', 'success');
      } else if (name === 'refresh') {
        toast(label + ': tokens renewed through its proxy', 'success');
      } else if (name === 'usage') {
        toast(label + ': limits updated', 'success');
      } else {
        toast(label + ' removed', 'success');
      }
    } catch (err) {
      toast(label + ': ' + err.message, 'error');
    }
    await load();
  }

  // Reads every account's limits in turn, each through its own proxy; one failing does not stop
  // the others.
  const usageAllBtn = document.getElementById('codex-usage-all');
  usageAllBtn.addEventListener('click', async () => {
    usageAllBtn.disabled = true;
    const original = usageAllBtn.textContent;
    usageAllBtn.textContent = 'Reading…';
    try {
      const res = await post('/api/codex/usage/refresh', {});
      if (!res.ok) {
        toast('Usage: ' + (await errorMessage(res)), 'error');
      } else {
        const results = (await res.json()).results || [];
        const failed = results.filter((r) => !r.ok);
        toast((results.length - failed.length) + ' of ' + results.length + ' accounts updated', failed.length ? 'info' : 'success');
        failed.slice(0, 3).forEach((r) => toast(r.email + ': ' + r.error, 'error'));
      }
    } catch (err) {
      toast('Usage: ' + err.message, 'error');
    }
    usageAllBtn.textContent = original;
    usageAllBtn.disabled = false;
    await load();
  });

  body.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    const row = btn.closest('tr');
    if (row) act(btn, btn.dataset.act, row.dataset.id);
  });

  // ---- dialog ----

  function hasProxy() {
    return dlg.input.value.trim() !== '' || dlg.poolSelect.value !== '';
  }

  function updateStart() {
    dlg.start.disabled = dlg.busy || !hasProxy();
  }

  function setError(message) {
    dlg.error.textContent = message || '';
    dlg.error.hidden = !message;
  }

  function stopPolling() {
    if (dlg.pollTimer) clearInterval(dlg.pollTimer);
    dlg.pollTimer = null;
  }

  function mode() {
    return dlg.modeProfile.checked ? 'profile' : 'link';
  }

  async function fillPool() {
    dlg.poolWrap.hidden = true;
    dlg.poolSelect.innerHTML = '';
    try {
      const res = await fetch('/api/proxies');
      if (!res.ok) return;
      const free = ((await res.json()).proxies || []).filter((p) => !p.used_by && !p.invalid);
      if (free.length === 0 || !dlg.el.open) return;
      dlg.poolSelect.innerHTML = '<option value="">— none —</option>' +
        free.map((p) => '<option value="' + esc(p.id) + '">' + esc(p.proxy) + '</option>').join('');
      dlg.poolWrap.hidden = false;
    } catch (_) { /* typing a proxy URL still works */ }
  }

  async function checkProfileAPI() {
    dlg.modeHint.textContent = 'Checking for AliasMode…';
    let status = null;
    try {
      const res = await fetch('/api/onboarding/status');
      if (res.ok) status = await res.json();
    } catch (_) { /* treated as unavailable */ }
    const available = !!(status && status.available);
    dlg.modeProfile.disabled = !available;
    dlg.modeProfile.closest('.oauth-mode-option').classList.toggle('is-unavailable', !available);
    if (available) {
      dlg.modeHint.textContent = 'Found at ' + status.url + '. Creates a profile with the proxy below and opens ChatGPT in it.';
      dlg.modeProfile.checked = true;
    } else {
      dlg.modeHint.textContent = 'Not available: start AliasMode with its Local API enabled, then reopen this dialog.';
      dlg.modeLink.checked = true;
    }
    applyMode();
  }

  function applyMode() {
    if (dlg.kind === 'add') {
      dlg.start.textContent = mode() === 'profile' ? 'Open profile & sign in' : 'Start sign-in';
    }
  }

  function openDialog(kind, account) {
    stopPolling();
    dlg.kind = kind;
    dlg.accountID = account ? account.id : '';
    dlg.busy = false;
    dlg.input.value = '';
    dlg.input.disabled = false;
    dlg.poolSelect.disabled = false;
    dlg.link.hidden = true;
    dlg.url.value = '';
    dlg.paste.hidden = true;
    dlg.pasteInput.value = '';
    dlg.pasteError.hidden = true;
    setError('');
    const adding = kind === 'add';
    dlg.title.textContent = adding ? 'Add a Codex account' : 'Change proxy';
    dlg.account.hidden = adding;
    dlg.account.textContent = account ? account.email : '';
    dlg.modes.hidden = !adding;
    dlg.start.textContent = adding ? 'Start sign-in' : 'Save proxy';
    dlg.modeProfile.disabled = true;
    dlg.modeLink.checked = true;
    updateStart();
    dlg.el.showModal();
    dlg.input.focus();
    fillPool();
    if (adding) checkProfileAPI();
  }

  // After the link is shown, watch for the account to appear or be re-authenticated.
  async function watchForAccount() {
    const snapshot = async () => {
      const res = await fetch('/api/codex/accounts');
      if (!res.ok) return null;
      return new Map((await res.json()).map((a) => [a.id, a.last_refresh]));
    };
    const before = await snapshot().catch(() => null);
    if (!before) return;
    const deadline = Date.now() + 15 * 60 * 1000;
    dlg.pollTimer = setInterval(async () => {
      if (Date.now() > deadline) {
        stopPolling();
        dlg.status.textContent = 'The sign-in was not completed in time. Start again.';
        return;
      }
      const now = await snapshot().catch(() => null);
      if (!now) return;
      for (const [id, refreshed] of now) {
        if (!before.has(id) || before.get(id) !== refreshed) {
          stopPolling();
          dlg.el.close();
          toast('Codex account added through its proxy', 'success');
          load();
          return;
        }
      }
    }, 3000);
  }

  async function submit() {
    if (dlg.busy || !hasProxy()) return;
    setError('');
    dlg.busy = true;
    dlg.input.disabled = true;
    dlg.poolSelect.disabled = true;
    updateStart();

    const payload = {};
    if (dlg.poolSelect.value) payload.pool_id = dlg.poolSelect.value;
    else payload.proxy_url = dlg.input.value.trim();

    const release = () => {
      dlg.busy = false;
      dlg.input.disabled = false;
      dlg.poolSelect.disabled = false;
      updateStart();
    };

    try {
      if (dlg.kind === 'proxy') {
        payload.id = dlg.accountID;
        const res = await post('/api/codex/accounts/proxy', payload);
        if (!res.ok) { setError(await errorMessage(res)); release(); return; }
        dlg.el.close();
        toast('Proxy updated', 'success');
        load();
        return;
      }

      payload.mode = mode();
      const res = await post('/api/codex/login/start', payload);
      if (!res.ok) { setError(await errorMessage(res)); release(); return; }
      const data = await res.json();
      dlg.url.value = data.auth_url || '';
      dlg.link.hidden = false;
      dlg.start.textContent = 'Started';
      if (data.mode === 'profile') {
        dlg.urlLabel.textContent = 'If the window did not open ChatGPT, open this link inside the profile';
        dlg.status.textContent = 'Profile ' + data.profile_id + ' is open through ' + data.proxy + '. Sign in inside its window…';
      } else {
        dlg.urlLabel.textContent = 'Sign-in link: open it in a browser profile that uses this proxy';
        dlg.status.textContent = data.auth_url
          ? 'Waiting for the sign-in (proxy ' + data.proxy + ')…'
          : 'The sign-in started, but the link is not ready yet. Check the event log below.';
      }
      // The callback port could not be opened here: the user finishes by pasting the address of
      // the page that fails to load.
      dlg.paste.hidden = !data.manual;
      watchForAccount();
    } catch (err) {
      setError('Could not start: ' + err.message);
      release();
    }
  }

  dlg.input.addEventListener('input', () => {
    setError('');
    if (dlg.input.value.trim() !== '') dlg.poolSelect.value = '';
    updateStart();
  });
  dlg.poolSelect.addEventListener('change', () => {
    setError('');
    if (dlg.poolSelect.value !== '') dlg.input.value = '';
    updateStart();
  });
  dlg.form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });
  async function completePaste() {
    const pasted = dlg.pasteInput.value.trim();
    if (!pasted) return;
    dlg.pasteError.hidden = true;
    dlg.pasteBtn.disabled = true;
    try {
      const res = await post('/api/codex/login/complete', { url: pasted });
      if (!res.ok) {
        dlg.pasteError.textContent = await errorMessage(res);
        dlg.pasteError.hidden = false;
        return;
      }
      stopPolling();
      dlg.el.close();
      toast('Codex account added through its proxy', 'success');
      load();
    } catch (err) {
      dlg.pasteError.textContent = 'Could not complete: ' + err.message;
      dlg.pasteError.hidden = false;
    } finally {
      dlg.pasteBtn.disabled = false;
    }
  }
  dlg.pasteBtn.addEventListener('click', completePaste);
  dlg.pasteInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); completePaste(); } });
  dlg.modeProfile.addEventListener('change', applyMode);
  dlg.modeLink.addEventListener('change', applyMode);
  dlg.copy.addEventListener('click', () => {
    if (navigator.clipboard) navigator.clipboard.writeText(dlg.url.value).then(() => toast('Sign-in link copied', 'success'), () => {});
  });
  dlg.close.addEventListener('click', () => dlg.el.close());
  dlg.el.addEventListener('close', () => {
    stopPolling();
    dlg.input.value = ''; // never leave a typed proxy credential in the DOM
    dlg.poolSelect.innerHTML = '';
    dlg.url.value = '';
    dlg.pasteInput.value = ''; // the pasted address carries the authorization code
    setError('');
  });
  addBtn.addEventListener('click', () => openDialog('add', null));

  load();
})();
