// Portable LLM UI — zero dependencies, same-origin against llama-server.
// llama-server serves this directory via --path, so /v1/* is same-origin: no CORS.

const $ = (id) => document.getElementById(id);
const msgs = $('messages');
let history = [];
let controller = null;
let currentModel = null;      // null => let the server choose

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
  const saved = (() => { try { return localStorage.getItem('portable-llm-model'); } catch { return null; } })();

  sel.innerHTML = '';
  let firstUsable = null;
  for (const id of ids) {
    const meta = info[id];
    const isPart = SPLIT_PART.test(id);
    const fits = !isPart && (!meta || meta.fits !== false);   // unknown => assume usable
    const opt = document.createElement('option');
    opt.value = id;
    opt.textContent = meta ? `${id}  (${prettyBytes(meta.bytes)})` : id;
    if (isPart)      { opt.disabled = true; opt.textContent += '  — multi-part, unsupported'; }
    else if (!fits)  { opt.disabled = true; opt.textContent += '  — too large'; }
    else if (firstUsable === null) firstUsable = id;
    sel.appendChild(opt);
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
  save();

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
    save();
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

/* ---------- persistence (per-viewer convenience only) ---------- */

function save() {
  try { localStorage.setItem('portable-llm-chat', JSON.stringify(history)); } catch {}
}
function restore() {
  try {
    const saved = JSON.parse(localStorage.getItem('portable-llm-chat') || '[]');
    if (!Array.isArray(saved) || !saved.length) return;
    history = saved;
    saved.forEach((m) => addMessage(m.role, m.content));
  } catch {}
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
  try { localStorage.setItem('portable-llm-model', currentModel); } catch {}
  // Chat templates and tokenizers differ per model; replaying one model's
  // transcript into another produces confused output. Start clean.
  $('new-chat').click();
});
$('new-chat').addEventListener('click', () => {
  controller?.abort();
  history = [];
  save();
  msgs.innerHTML = '<div class="empty"><h1>Local model, ready.</h1>' +
    '<p>Everything below runs from the drive. Nothing leaves this computer.</p></div>';
  $('tps').textContent = '—';
});

restore();
probe();
setInterval(probe, 5000);
