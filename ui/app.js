// Pocket LLM UI — zero dependencies, one origin, no CORS.
// pocketd serves this directory, forwards /v1/* to llama-server, and owns
// /api/* — so conversations are written to the drive and never to this browser.

const $ = (id) => document.getElementById(id);
const msgs = $('messages');
let history = [];
let controller = null;
let currentModel = null;      // null => let the server choose
let savedModel = null;        // preference read back from the drive, not localStorage

/* ---------- rendering ---------- */

const esc = (s) => s.replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// Qwen3 and other hybrid-reasoning models emit <think>...</think> inline.
// Render those as a collapsed block instead of leaking raw tags into the chat.
// The closing tag is absent mid-stream, so an unterminated block stays open.
function render(text) {
  const out = [];
  const re = /<think>([\s\S]*?)(<\/think>|$)/g;
  let last = 0, m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) out.push(renderBody(text.slice(last, m.index)));
    const streaming = !m[2];
    out.push(`<details class="think"${streaming ? ' open' : ''}>` +
             `<summary>${streaming ? 'thinking…' : 'reasoning'}</summary>` +
             renderBody(m[1]) + '</details>');
    last = re.lastIndex;
    if (streaming) break;
  }
  if (last < text.length) out.push(renderBody(text.slice(last)));
  return out.join('');
}

// Minimal markdown: fenced code, inline code, bold. Everything is escaped first.
function renderBody(text) {
  const parts = text.split(/```/);
  return parts.map((chunk, i) => {
    if (i % 2 === 1) {                                  // inside a fence
      const nl = chunk.indexOf('\n');
      const lang = nl > -1 ? chunk.slice(0, nl).trim() : '';
      const code = nl > -1 ? chunk.slice(nl + 1) : chunk;
      return `<pre><code data-lang="${esc(lang)}">${esc(code.replace(/\n$/, ''))}</code></pre>`;
    }
    return esc(chunk)
      .replace(/`([^`\n]+)`/g, '<code>$1</code>')
      .replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
  }).join('');
}

// llama-server splits reasoning into its own `reasoning_content` field rather
// than inlining <think> tags, so a turn has two independent parts.
function renderTurn(reasoning, content, streaming) {
  let html = '';
  if (reasoning) {
    const open = streaming && !content;      // stay open until the answer starts
    html += `<details class="think"${open ? ' open' : ''}>` +
            `<summary>${open ? 'thinking…' : 'reasoning'}</summary>` +
            renderBody(reasoning) + '</details>';
  }
  return html + render(content);
}

function addMessage(role, text) {
  msgs.querySelector('.empty')?.remove();
  const el = document.createElement('div');
  el.className = `msg ${role}`;
  el.innerHTML = `<div class="who">${role === 'user' ? 'You' : 'AI'}</div><div class="body"></div>`;
  el.querySelector('.body').innerHTML = render(text);
  msgs.appendChild(el);
  msgs.scrollTop = msgs.scrollHeight;
  return el.querySelector('.body');
}

/* ---------- connection ---------- */

async function probe() {
  try {
    const r = await fetch('/v1/models');
    if (!r.ok) throw new Error(r.status);
    const models = (await r.json()).data ?? [];
    await populateModels(models.map((m) => m.id));
    $('status').textContent = 'ready';
    $('status').className = 'on';
    return true;
  } catch {
    $('status').textContent = 'offline';
    $('status').className = 'off';
    return false;
  }
}

// The launcher writes machine.json describing what THIS machine can run.
// Router mode serves every model in models/, including ones too large for the
// host, so without this a 16GB laptop would be offered a 32B and fail on pick.
// Absent or unreadable manifest => offer everything (fail open, not closed).
let manifest = null;
async function loadManifest() {
  if (manifest !== null) return manifest;
  try {
    const r = await fetch('machine.json', { cache: 'no-store' });
    manifest = r.ok ? await r.json() : {};
  } catch { manifest = {}; }
  return manifest;
}

const prettyBytes = (b) =>
  b >= 1 << 30 ? (b / (1 << 30)).toFixed(1) + 'GB' : Math.round(b / (1 << 20)) + 'MB';

// A filename carries packaging detail — quantisation, tuning suffix, datestamp —
// that is IDENTICAL across every file on this drive and so distinguishes nothing,
// while eating most of a narrow sidebar's width. Strip it for display only;
// opt.value keeps the exact id the server expects, and opt.title keeps the
// on-disk name one hover away.
const displayName = (id) => id
  .replace(/[-_](UD[-_])?I?Q\d+(_[A-Z0-9]+)*$/i, '')   // -Q4_K_M, -UD-Q4_K_XL, -IQ4_XS
  .replace(/[-_](Instruct|it|qat|chat|hf)\b/gi, '')
  .replace(/[-_]\d{4}$/, '')                           // -2507 and friends
  .replace(/[-_]+/g, ' ')
  .replace(/^./, (c) => c.toUpperCase());

// Two facts worth surfacing because they change how a model behaves, not just
// how fast it is. "A4B" is a naming convention, not a guess: it states the
// active parameter count of a mixture-of-experts, which is why a 15.8GB file
// runs like a far smaller one. R1 emits visible chain-of-thought.
const tagFor = (id) =>
  /[-_]A\d+B\b/i.test(id)        ? 'MoE'
  : /(^|[-_])R1([-_]|$)/i.test(id) ? 'reasoning'
  : '';

// Bands are cut on size alone so a model nobody has ever described still lands
// somewhere sensible. The header carries the tradeoff, which keeps each row
// short enough to survive the sidebar.
const BANDS = [
  { label: 'Fast',         max:  8 * (1 << 30) },
  { label: 'Balanced',     max: 25 * (1 << 30) },
  { label: 'Best quality', max: Infinity },
];

async function populateModels(ids) {
  const sel = $('model-select');
  const signature = ids.join('|');
  if (sel.dataset.signature === signature) return;   // no churn while streaming

  const info = (await loadManifest()).models ?? {};

  // Router mode lists every .gguf in models/, including individual parts of a
  // multi-part model (foo-00001-of-00003). Those parts are ONE model and cannot
  // be run individually, so offering them as separate entries would hand the
  // user broken choices. The launcher applies the same filter to its size math.
  const SPLIT_PART = /-\d{5}-of-\d{5}$/;
  const saved = savedModel;

  sel.innerHTML = '';
  let firstUsable = null;

  // Called in final display order, so firstUsable lands on the first model the
  // user can actually see and pick.
  const addOption = (parent, id) => {
    const meta = info[id];
    const isPart = SPLIT_PART.test(id);
    const fits = !isPart && (!meta || meta.fits !== false);   // unknown => assume usable
    const opt = document.createElement('option');
    opt.value = id;
    opt.title = id;                                           // full on-disk name
    const tag = isPart ? '' : tagFor(id);
    opt.textContent = (isPart ? id : displayName(id))
      + (meta ? `  ·  ${prettyBytes(meta.bytes)}` : '')
      + (tag ? `  (${tag})` : '');
    if (isPart)      { opt.disabled = true; opt.textContent += '  — multi-part, unsupported'; }
    else if (!fits)  { opt.disabled = true; opt.textContent += '  — too large'; }
    else if (firstUsable === null) firstUsable = id;
    parent.appendChild(opt);
  };

  // Banding needs a size, which only the manifest has. Without it, fall back to
  // the flat list in server order rather than inventing groups (fail open, the
  // same rule `fits` follows above).
  const sized = ids.filter((id) => info[id] && !SPLIT_PART.test(id))
                   .sort((a, b) => info[a].bytes - info[b].bytes);
  const rest  = ids.filter((id) => !info[id] || SPLIT_PART.test(id));

  if (sized.length === 0) {
    for (const id of ids) addOption(sel, id);
  } else {
    const bandOf = (bytes) => BANDS.findIndex((b) => bytes < b.max);
    BANDS.forEach((band, i) => {
      const members = sized.filter((id) => bandOf(info[id].bytes) === i);
      if (members.length === 0) return;                       // no empty headers
      const group = document.createElement('optgroup');
      group.label = band.label;
      for (const id of members) addOption(group, id);
      sel.appendChild(group);
    });
    for (const id of rest) addOption(sel, id);                 // unsized/parts last
  }

  const savedUsable = saved && ids.includes(saved) &&
                      !SPLIT_PART.test(saved) && info[saved]?.fits !== false;
  currentModel = savedUsable ? saved : firstUsable;
  if (currentModel) sel.value = currentModel;

  // Claim the signature only AFTER the options exist. Setting it before the
  // await meant a failed or still-pending build would make every later probe
  // early-return, leaving the dropdown permanently empty.
  sel.dataset.signature = signature;
}

/* ---------- chat ---------- */

async function send(text) {
  addMessage('user', text);
  history.push({ role: 'user', content: text });
  const isFirstTurn = history.length === 1;
  await ensureChat();
  await record('user', text);
  if (isFirstTurn) refreshChats();      // the list is titled from this message

  const body = addMessage('assistant', '');
  body.innerHTML = '<span class="cursor"></span>';

  $('send').disabled = true;
  $('stop').hidden = false;
  controller = new AbortController();

  let acc = '';
  let reasoning = '';
  let tokens = 0;
  const t0 = performance.now();

  try {
    const res = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      signal: controller.signal,
      // No max_tokens: capping a reasoning model truncates it mid-thought and
      // yields an empty answer (see 93e2468).
      body: JSON.stringify({
        ...(currentModel ? { model: currentModel } : {}),
        messages: history, stream: true, temperature: 0.7,
      }),
    });
    if (!res.ok) throw new Error(`server returned ${res.status}`);

    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';

    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });

      const lines = buf.split('\n');
      buf = lines.pop();                                // keep partial line

      for (const line of lines) {
        if (!line.startsWith('data: ')) continue;
        const payload = line.slice(6).trim();
        if (payload === '[DONE]') continue;
        let d;
        try { d = JSON.parse(payload).choices?.[0]?.delta ?? {}; }
        catch { continue; }                             // ignore malformed keep-alives
        if (d.reasoning_content) { reasoning += d.reasoning_content; tokens++; }
        if (d.content)           { acc       += d.content;           tokens++; }
        if (!d.reasoning_content && !d.content) continue;
        body.innerHTML = renderTurn(reasoning, acc, true) + '<span class="cursor"></span>';
        msgs.scrollTop = msgs.scrollHeight;
        $('tps').textContent =
          (tokens / ((performance.now() - t0) / 1000)).toFixed(1) + ' tok/s';
      }
    }
    // Reasoning is deliberately NOT pushed to history — models are trained to
    // see only prior answers, and replaying it wastes context every turn.
    history.push({ role: 'assistant', content: acc });
    await record('assistant', acc);
    refreshChats();
  } catch (err) {
    if (err.name !== 'AbortError') {
      acc += (acc ? '\n\n' : '') + `⚠️ ${err.message}. Is the model still loading?`;
      probe();
    }
  } finally {
    body.innerHTML = renderTurn(reasoning, acc, false);
    $('send').disabled = false;
    $('stop').hidden = true;
    controller = null;
  }
}

/* ---------- conversations (kept on the drive, never in this browser) ---------- */

// pocketd is the only writer. If it did not start — a locked-down machine, a
// read-only drive — llama-server serves the UI alone and there is no /api at
// all. History then stays off and says so. It must NOT quietly fall back to
// localStorage: leaving conversations on a borrowed laptop is the one thing
// this drive is not allowed to do.
let historyOn = false;
let chatId = null;            // null => nothing written for this conversation yet

async function api(path, opts = {}) {
  const r = await fetch('/api' + path, {
    headers: { 'Content-Type': 'application/json' }, ...opts,
  });
  if (!r.ok) throw new Error(`${path} → ${r.status}`);
  return r.json();
}

async function initHistory() {
  try {
    historyOn = !!(await api('/health')).history;
  } catch { historyOn = false; }

  $('erase-all').hidden = !historyOn;
  if (!historyOn) {
    $('chats').innerHTML = '<div class="chats-off">History is off — this machine ' +
      'can\u2019t save to the drive. Conversations will be lost when you close the window.</div>';
    return;
  }
  try { savedModel = (await api('/settings')).model ?? null; } catch {}

  // Pick up where the last session stopped, the way restoring from browser
  // storage used to — only now it follows the drive, not the machine.
  const list = await refreshChats();
  if (list.length) await openChat(list[0].id);
}

async function refreshChats() {
  if (!historyOn) return [];
  let list;
  try { list = await api('/chats'); } catch { return []; }

  const box = $('chats');
  box.innerHTML = '';
  for (const c of list) {
    const row = document.createElement('div');
    row.className = 'chat-row' + (c.id === chatId ? ' active' : '');
    row.innerHTML =
      `<button class="chat-open" title="${esc(c.title)}">${esc(c.title)}</button>` +
      `<button class="chat-del" title="Delete this conversation">×</button>`;
    row.querySelector('.chat-open').onclick = () => openChat(c.id);
    row.querySelector('.chat-del').onclick = async () => {
      try { await api('/chats/' + c.id, { method: 'DELETE' }); } catch {}
      if (c.id === chatId) newChat(); else refreshChats();
    };
    box.appendChild(row);
  }
  return list;
}

async function openChat(id) {
  controller?.abort();
  let c;
  try { c = await api('/chats/' + id); } catch { return; }
  chatId = c.id;
  history = c.messages.map(({ role, content }) => ({ role, content }));
  msgs.innerHTML = '';
  if (history.length) history.forEach((m) => addMessage(m.role, m.content));
  else emptyState();
  refreshChats();
}

// The file is created by the first message, not by the New chat click —
// otherwise idle clicking litters the drive with empty transcripts.
async function ensureChat() {
  if (!historyOn || chatId) return;
  try {
    chatId = (await api('/chats', {
      method: 'POST', body: JSON.stringify({ model: currentModel }),
    })).id;
  } catch { historyOn = false; }
}

async function record(role, content) {
  if (!historyOn || !chatId) return;
  try {
    await api('/chats/' + chatId, { method: 'POST', body: JSON.stringify({ role, content }) });
  } catch {}
}

function emptyState() {
  msgs.innerHTML = '<div class="empty"><h1>Local model, ready.</h1>' +
    '<p>Everything below runs from the drive. Nothing leaves this computer.</p></div>';
}

function newChat() {
  controller?.abort();
  history = [];
  chatId = null;
  emptyState();
  $('tps').textContent = '—';
  refreshChats();
}

/* ---------- wiring ---------- */

const input = $('input');
input.addEventListener('input', () => {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 200) + 'px';
});
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); $('composer').requestSubmit(); }
});
$('composer').addEventListener('submit', (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text || $('send').disabled) return;
  input.value = '';
  input.style.height = 'auto';
  send(text);
});
$('stop').addEventListener('click', () => controller?.abort());
$('model-select').addEventListener('change', (e) => {
  currentModel = e.target.value;
  savedModel = currentModel;
  if (historyOn) {
    api('/settings', { method: 'PUT', body: JSON.stringify({ model: currentModel }) })
      .catch(() => {});
  }
  // Chat templates and tokenizers differ per model; replaying one model's
  // transcript into another produces confused output. Start clean.
  newChat();
});
$('new-chat').addEventListener('click', newChat);

// Two-step rather than a confirm() dialog: a modal blocks the page, and this
// button destroys everything on the drive. Disarms itself if left alone.
let eraseArmed = null;
$('erase-all').addEventListener('click', async () => {
  const b = $('erase-all');
  const reset = () => {
    clearTimeout(eraseArmed); eraseArmed = null;
    b.classList.remove('armed'); b.textContent = 'Erase all conversations';
  };
  if (!eraseArmed) {
    b.classList.add('armed');
    b.textContent = 'Click again to erase everything';
    eraseArmed = setTimeout(reset, 4000);
    return;
  }
  reset();
  try { await api('/chats', { method: 'DELETE' }); } catch {}
  newChat();
});

(async () => {
  await initHistory();
  await probe();
  setInterval(probe, 5000);
})();
