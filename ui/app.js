// Pocket LLM UI — zero dependencies, one origin, no CORS.
// pocketd serves this directory, forwards /v1/* to llama-server, and owns
// /api/* — so conversations are written to the drive and never to this browser.

const $ = (id) => document.getElementById(id);
const msgs = $('messages');

let history = [];             // what gets replayed to the model: role + content only
let controller = null;
let currentModel = null;      // null => let the server choose
let savedModel = null;        // preference read back from the drive, not localStorage
let searchOn = false;         // pocketd can store and index documents
let useDocs = false;          // ...and the user wants answers grounded in them

const esc = (s) => String(s).replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const icon = (name) => `<svg class="ic"><use href="#i-${name}"/></svg>`;

/* ---------------------------------------------------------- markdown ---
   A local model answers in markdown whether or not the UI understands it, so
   "minimal markdown" is not a smaller feature — it is tables rendered as rows
   of pipes and headings rendered as hashes. This is a small block parser:
   headings, lists (nested), tables, quotes, rules, fenced and inline code,
   links, emphasis. Everything is escaped before any tag is introduced. */

const FENCE_MARK = /^\u0000F(\d+)\u0000$/;

// A few token classes are enough to give code shape. This is deliberately not a
// real grammar for any language — it is one pass that colours comments, strings,
// numbers and common keywords across the languages a local model actually emits.
const HL = new RegExp([
  '(\\/\\/[^\\n]*|#[^\\n]*|--[^\\n]*|\\/\\*[\\s\\S]*?\\*\\/)',      // comments
  '("(?:[^"\\\\\\n]|\\\\.)*"|\'(?:[^\'\\\\\\n]|\\\\.)*\'|`(?:[^`\\\\]|\\\\.)*`)', // strings
  '\\b(\\d+(?:\\.\\d+)?)\\b',                                        // numbers
  '\\b(const|let|var|function|return|if|elif|else|for|while|do|done|then|fi|case|esac|' +
  'class|struct|interface|enum|type|new|delete|import|from|export|package|use|pub|mod|' +
  'async|await|yield|try|catch|except|finally|throw|raise|def|lambda|fn|func|go|defer|' +
  'select|switch|break|continue|in|is|not|and|or|None|True|False|null|nil|undefined|' +
  'true|false|self|this|super|static|public|private|void|int|string|bool|float|echo|' +
  'local|set|unset|source|with|as|pass|end|require|module)\\b',       // keywords
].join('|'), 'g');

function highlight(code) {
  let out = '', last = 0, m;
  HL.lastIndex = 0;
  while ((m = HL.exec(code)) !== null) {
    out += esc(code.slice(last, m.index));
    const cls = m[1] ? 'hl-c' : m[2] ? 'hl-s' : m[3] ? 'hl-n' : 'hl-k';
    out += `<span class="${cls}">${esc(m[0])}</span>`;
    last = HL.lastIndex;
  }
  return out + esc(code.slice(last));
}

// The code text rides in a data attribute rather than a JS side-table: the
// message is re-rendered on every streamed token, and a table keyed by a
// generated id would grow by one entry per token and never be collected.
function codeBlock(lang, code) {
  return `<pre data-src="${esc(code)}">` +
    `<div class="code-head"><span>${esc(lang || 'code')}</span>` +
    `<button class="copy-code" type="button">${icon('copy')}Copy</button></div>` +
    `<code>${highlight(code)}</code></pre>`;
}

function inline(s) {
  // Inline code is lifted out first so emphasis cannot fire inside it —
  // `a * b` must not become `a <em> b`.
  const codes = [];
  s = s.replace(/`([^`]+)`/g, (_, c) => `\u0001${codes.push(c) - 1}\u0001`);
  s = esc(s);
  s = s.replace(/\[([^\]\n]+)\]\(([^)\s]+)\)/g, (m, t, u) =>
    /^(https?:|mailto:)/i.test(u) ? `<a href="${u}" target="_blank" rel="noreferrer noopener">${t}</a>` : m);
  s = s.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>')
       .replace(/__([^_\n]+)__/g, '<strong>$1</strong>')
       .replace(/(^|[\s(])\*([^*\n]+)\*/g, '$1<em>$2</em>')
       .replace(/~~([^~\n]+)~~/g, '<del>$1</del>');
  return s.replace(/\u0001(\d+)\u0001/g, (_, i) => `<code>${esc(codes[i])}</code>`);
}

const indentOf = (s) => s.match(/^\s*/)[0].replace(/\t/g, '    ').length;
const isBullet = (s) => /^\s*([-*+]|\d+[.)])\s+/.test(s);

function isTableAt(lines, i) {
  return lines[i].includes('|') && i + 1 < lines.length &&
         /^\s*\|?[\s:|-]*-[\s:|-]*$/.test(lines[i + 1]) && lines[i + 1].includes('-');
}

function blocks(text, fences) {
  const lines = text.split('\n');
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }

    let m = line.trim().match(FENCE_MARK);
    if (m) { out.push(codeBlock(...fences[+m[1]])); i++; continue; }

    if ((m = line.match(/^ {0,3}(#{1,6})\s+(.*)$/))) {
      const level = Math.min(m[1].length, 4);
      out.push(`<h${level}>${inline(m[2].trim())}</h${level}>`); i++; continue;
    }
    if (/^ {0,3}([-*_])\s*(\1\s*){2,}$/.test(line)) { out.push('<hr>'); i++; continue; }

    if (/^ {0,3}>/.test(line)) {
      const buf = [];
      while (i < lines.length && /^ {0,3}>/.test(lines[i])) {
        buf.push(lines[i].replace(/^ {0,3}>\s?/, '')); i++;
      }
      out.push(`<blockquote>${blocks(buf.join('\n'), fences)}</blockquote>`);
      continue;
    }
    if (isTableAt(lines, i)) { const [html, next] = table(lines, i); out.push(html); i = next; continue; }
    if (isBullet(line))      { const [html, next] = list(lines, i, fences); out.push(html); i = next; continue; }

    const buf = [];
    while (i < lines.length && lines[i].trim() && !isBullet(lines[i]) &&
           !FENCE_MARK.test(lines[i].trim()) && !/^ {0,3}(#{1,6}\s|>)/.test(lines[i]) &&
           !isTableAt(lines, i)) {
      buf.push(lines[i]); i++;
    }
    out.push(`<p>${inline(buf.join('\n')).replace(/\n/g, '<br>')}</p>`);
  }
  return out.join('');
}

// A single-line item comes back from blocks() wrapped in <p>, which would put
// paragraph margins inside every bullet. Unwrap when it is the only block.
function unwrapP(html) {
  const m = html.match(/^<p>([\s\S]*)<\/p>$/);
  return m && !m[1].includes('<p>') ? m[1] : html;
}

function list(lines, start, fences) {
  const base = indentOf(lines[start]);
  const ordered = /^\s*\d+[.)]\s/.test(lines[start]);
  const items = [];
  let i = start;

  while (i < lines.length) {
    const l = lines[i];
    if (!l.trim()) {
      const next = lines[i + 1];
      if (next && next.trim() && indentOf(next) >= base && isBullet(next)) { i++; continue; }
      break;
    }
    const ind = indentOf(l);
    if (ind >= base + 2 && items.length) { items[items.length - 1].push(l.slice(base + 2)); i++; continue; }
    if (ind < base || !isBullet(l)) {
      if (items.length && ind >= base) { items[items.length - 1].push(l.trim()); i++; continue; }
      break;
    }
    items.push([l.replace(/^\s*([-*+]|\d+[.)])\s+/, '')]);
    i++;
  }
  const body = items.map((chunk) => `<li>${unwrapP(blocks(chunk.join('\n'), fences))}</li>`).join('');
  return [`<${ordered ? 'ol' : 'ul'}>${body}</${ordered ? 'ol' : 'ul'}>`, i];
}

function table(lines, i) {
  const cells = (l) => l.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
  const head = cells(lines[i]);
  const rows = [];
  let j = i + 2;
  while (j < lines.length && lines[j].trim() && lines[j].includes('|')) { rows.push(cells(lines[j])); j++; }
  const th = head.map((c) => `<th>${inline(c)}</th>`).join('');
  const tb = rows.map((r) => `<tr>${r.map((c) => `<td>${inline(c)}</td>`).join('')}</tr>`).join('');
  return [`<table><thead><tr>${th}</tr></thead><tbody>${tb}</tbody></table>`, j];
}

function renderBody(text) {
  const fences = [];
  // An unterminated fence is normal mid-stream: treat the rest as code so the
  // block does not flicker between prose and code on every token.
  const src = text.replace(/```([^\n`]*)\n?([\s\S]*?)(?:```|$)/g, (_, lang, code) => {
    fences.push([lang.trim(), code.replace(/\n$/, '')]);
    return `\n\u0000F${fences.length - 1}\u0000\n`;
  });
  return blocks(src, fences);
}

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

// Citations are rendered from the message itself, so they survive a reload
// rather than existing only in the live view.
function sourcesHTML(sources) {
  if (!sources?.length) return '';
  return `<div class="sources">Sources: ${sources.map(esc).join(' · ')}</div>`;
}

/* ----------------------------------------------------------- messages */

function addMessage(role, text, sources) {
  const el = document.createElement('div');
  el.className = `turn ${role}`;
  el.innerHTML = '<div class="body"></div><div class="actions"></div>';
  el.querySelector('.body').innerHTML = render(text) + sourcesHTML(sources);
  el.dataset.text = text;
  msgs.appendChild(el);
  setEmpty(false);
  refreshActions();
  scrollToEnd();
  return el.querySelector('.body');
}

// Regenerate belongs on the last answer only — offering it halfway up a thread
// implies it would rewrite from there, which is not what it does.
function refreshActions() {
  const turns = [...msgs.querySelectorAll('.turn')];
  turns.forEach((t, idx) => {
    const box = t.querySelector('.actions');
    if (!box) return;
    const last = idx === turns.length - 1 && t.classList.contains('assistant');
    box.innerHTML =
      `<button class="act" data-act="copy" title="Copy">${icon('copy')}</button>` +
      (last ? `<button class="act" data-act="again" title="Regenerate">${icon('refresh')}</button>` : '');
  });
}

function scrollToEnd() { msgs.scrollTop = msgs.scrollHeight; }

async function copyText(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    // Clipboard API needs a secure context; 127.0.0.1 qualifies, but a stray
    // permissions policy should not lose the copy.
    const ta = document.createElement('textarea');
    ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } catch {}
    ta.remove();
  }
  if (!btn) return;
  const original = btn.innerHTML;
  btn.innerHTML = btn.classList.contains('copy-code') ? `${icon('check')}Copied` : icon('check');
  btn.classList.add('done');
  setTimeout(() => { btn.innerHTML = original; btn.classList.remove('done'); }, 1400);
}

/* --------------------------------------------------------- empty state */

const SUGGESTIONS = [
  ['Explain this code', 'paste a snippet and ask what it does'],
  ['Draft a reply', 'a short, polite decline to a meeting invite'],
  ['Plan something', 'break a task into steps I can actually start'],
];

function setEmpty(on) {
  $('main').classList.toggle('is-empty', on);
  $('suggestions').hidden = !on;
  if (!on) { msgs.querySelector('.hero')?.remove(); return; }

  msgs.innerHTML = `<div class="hero">${icon('mark').replace('class="ic"', 'class="ic mark"')}` +
    `<h1>${esc(currentModel ? displayName(currentModel) : 'Pocket LLM')}</h1></div>`;

  const items = SUGGESTIONS.slice();
  if (searchOn && docCount > 0) {
    items[0] = ['Ask about your documents', `search the ${docCount} file(s) on this drive`];
  }
  $('suggestions').innerHTML =
    `<div class="sugg-head">${icon('spark')}Suggested</div>` +
    items.map(([t, s]) =>
      `<button class="sugg" type="button" data-fill="${esc(t)}"><b>${esc(t)}</b><span>${esc(s)}</span></button>`
    ).join('');
}

/* ---------------------------------------------------------- connection */

async function probe() {
  try {
    const r = await fetch('/v1/models');
    if (!r.ok) throw new Error(r.status);
    const models = (await r.json()).data ?? [];
    await populateModels(models.map((m) => m.id));
    $('status').textContent = 'ready';
    $('status-dot').className = 'dot on';
    return true;
  } catch {
    $('status').textContent = 'offline';
    $('status-dot').className = 'dot';
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
// that is IDENTICAL across every file on this drive and so distinguishes nothing.
// Strip it for display only; the value passed to the server stays the exact id.
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
  /[-_]A\d+B\b/i.test(id)          ? 'MoE'
  : /(^|[-_])R1([-_]|$)/i.test(id) ? 'reasoning'
  : '';

// Bands are cut on size alone so a model nobody has ever described still lands
// somewhere sensible. The header carries the tradeoff, which keeps each row
// short enough to read at a glance.
const BANDS = [
  { label: 'Fast',         max:  8 * (1 << 30) },
  { label: 'Balanced',     max: 25 * (1 << 30) },
  { label: 'Best quality', max: Infinity },
];

const SPLIT_PART = /-\d{5}-of-\d{5}$/;

async function populateModels(ids) {
  const menu = $('model-menu');
  const signature = ids.join('|');
  if (menu.dataset.signature === signature) return;   // no churn while streaming

  const info = (await loadManifest()).models ?? {};
  menu.innerHTML = '';
  let firstUsable = null;

  // Called in final display order, so firstUsable lands on the first model the
  // user can actually see and pick.
  const addItem = (id) => {
    const meta = info[id];
    const isPart = SPLIT_PART.test(id);
    const fits = !isPart && (!meta || meta.fits !== false);   // unknown => assume usable
    const tag = isPart ? '' : tagFor(id);
    const note = isPart ? 'multi-part, unsupported' : fits ? '' : 'too large for this machine';
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'menu-item';
    b.dataset.model = id;
    b.title = id;                                            // full on-disk name
    b.disabled = !fits;
    b.innerHTML =
      `<span>${esc(isPart ? id : displayName(id))}</span>` +
      (tag ? `<span class="tag">${tag}</span>` : '') +
      `<span class="size">${meta ? prettyBytes(meta.bytes) : ''}${note ? ' · ' + note : ''}</span>`;
    if (fits && firstUsable === null) firstUsable = id;
    menu.appendChild(b);
  };

  // Banding needs a size, which only the manifest has. Without it, fall back to
  // a flat list in server order rather than inventing groups (fail open, the
  // same rule `fits` follows above).
  const sized = ids.filter((id) => info[id] && !SPLIT_PART.test(id))
                   .sort((a, b) => info[a].bytes - info[b].bytes);
  const rest  = ids.filter((id) => !info[id] || SPLIT_PART.test(id));

  if (sized.length === 0) {
    ids.forEach(addItem);
  } else {
    const bandOf = (bytes) => BANDS.findIndex((b) => bytes < b.max);
    BANDS.forEach((band, i) => {
      const members = sized.filter((id) => bandOf(info[id].bytes) === i);
      if (!members.length) return;                            // no empty headers
      const label = document.createElement('div');
      label.className = 'menu-label';
      label.textContent = band.label;
      menu.appendChild(label);
      members.forEach(addItem);
    });
    rest.forEach(addItem);                                    // unsized/parts last
  }

  const savedUsable = savedModel && ids.includes(savedModel) &&
                      !SPLIT_PART.test(savedModel) && info[savedModel]?.fits !== false;
  setModel(savedUsable ? savedModel : firstUsable, false);

  // Claim the signature only AFTER the items exist. Setting it before the await
  // meant a failed or still-pending build would make every later probe
  // early-return, leaving the picker permanently empty.
  menu.dataset.signature = signature;
}

function setModel(id, persist = true) {
  currentModel = id;
  $('model-name').textContent = id ? displayName(id) : 'no model';
  $('model-btn').title = id || '';
  for (const b of $('model-menu').querySelectorAll('.menu-item')) {
    b.classList.toggle('on', b.dataset.model === id);
  }
  if (msgs.querySelector('.hero h1')) msgs.querySelector('.hero h1').textContent =
    id ? displayName(id) : 'Pocket LLM';
  if (persist && id) { savedModel = id; settings.model = id; saveSettings(); }
}

/* --------------------------------------------------------------- chat */

let busy = false;

function setBusy(on) {
  busy = on;
  $('send').hidden = on;
  $('stop').hidden = !on;
}

async function submit(text) {
  addMessage('user', text);
  history.push({ role: 'user', content: text });
  const isFirstTurn = history.length === 1;
  await ensureChat();
  await record('user', text);
  if (isFirstTurn) refreshChats();      // the list is titled from this message
  await stream();
}

// Split from submit() so Regenerate can re-run the answer without re-recording
// the question that produced it.
async function stream() {
  const prompt = [...history].reverse().find((m) => m.role === 'user')?.content ?? '';
  const el = addMessage('assistant', '');
  const body = el;
  body.innerHTML = '<span class="cursor"></span>';
  setBusy(true);
  controller = new AbortController();

  let acc = '', reasoning = '', tokens = 0;
  const t0 = performance.now();

  // Re-rendering full markdown on every token is wasted work — the screen
  // repaints far less often than tokens arrive. Coalesce to one frame.
  let queued = false;
  const paint = () => {
    queued = false;
    const stick = msgs.scrollHeight - msgs.scrollTop - msgs.clientHeight < 80;
    body.innerHTML = renderTurn(reasoning, acc, true) + '<span class="cursor"></span>';
    if (stick) scrollToEnd();
    $('tps').textContent = (tokens / ((performance.now() - t0) / 1000)).toFixed(1) + ' tok/s';
  };
  const schedule = () => { if (!queued) { queued = true; requestAnimationFrame(paint); } };

  // Retrieval runs against the question actually asked, not the whole thread:
  // dragging the entire history into the query buries the current topic under
  // everything discussed before it.
  let grounding = null;
  let sources = [];
  if (useDocs && searchOn && prompt) {
    try {
      const hits = await api('/search?k=4&q=' + encodeURIComponent(prompt));
      if (hits.length) {
        sources = [...new Set(hits.map((h) => h.docName))];
        grounding = {
          role: 'system',
          content:
            'Use the excerpts below when they answer the question. If they do not, ' +
            'say so plainly and answer from your own knowledge — never invent a citation ' +
            'or attribute something to a document that does not say it.\n\n' +
            hits.map((h, i) => `[${i + 1}] ${h.docName}\n${h.text}`).join('\n\n'),
        };
      }
    } catch { /* retrieval is an enhancement; a failure must not block the answer */ }
  }

  try {
    const res = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      signal: controller.signal,
      // No max_tokens: capping a reasoning model truncates it mid-thought and
      // yields an empty answer (see 93e2468).
      // The grounding block is prepended for this one request only. It is never
      // pushed into `history`, so it is not stored, not replayed on the next
      // turn, and not re-sent once it has served its purpose.
      body: JSON.stringify({
        ...(currentModel ? { model: currentModel } : {}),
        messages: grounding ? [grounding, ...history] : history,
        stream: true, temperature: 0.7,
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
        if (d.reasoning_content || d.content) schedule();
      }
    }
    // Reasoning is deliberately NOT pushed to history — models are trained to
    // see only prior answers, and replaying it wastes context every turn.
    history.push({ role: 'assistant', content: acc });
    await record('assistant', acc, sources);
    refreshChats();
  } catch (err) {
    if (err.name !== 'AbortError') {
      acc += (acc ? '\n\n' : '') + `⚠️ ${err.message}. Is the model still loading?`;
      probe();
    }
  } finally {
    body.innerHTML = renderTurn(reasoning, acc, false) + sourcesHTML(sources);
    body.closest('.turn').dataset.text = acc;
    setBusy(false);
    controller = null;
    refreshActions();
  }
}

async function regenerate() {
  if (busy) return;
  if (history[history.length - 1]?.role !== 'assistant') return;
  history.pop();
  // The transcript is append-only, so dropping the answer from the drive means
  // truncating its last line — otherwise a reload would show both attempts.
  if (historyOn && chatId) {
    try { await api('/chats/' + chatId + '/last', { method: 'DELETE' }); } catch {}
  }
  msgs.querySelector('.turn:last-child')?.remove();
  await stream();
}

/* ---------- conversations (kept on the drive, never in this browser) ---------- */

// pocketd is the only writer. If it did not start — a locked-down machine, a
// read-only drive — llama-server serves the UI alone and there is no /api at
// all. History then stays off and says so. It must NOT quietly fall back to
// localStorage: leaving conversations on a borrowed laptop is the one thing
// this drive is not allowed to do.
let historyOn = false;
let chatId = null;            // null => nothing written for this conversation yet
let chatList = [];            // last fetched list, kept so search can filter it
let docCount = 0;

async function api(path, opts = {}) {
  const r = await fetch('/api' + path, {
    headers: { 'Content-Type': 'application/json' }, ...opts,
  });
  if (!r.ok) {
    // pocketd explains refusals in the body ("only text files can be indexed").
    // Losing that and reporting a bare 400 would make every rejection look the same.
    let detail = '';
    try { detail = (await r.json()).error ?? ''; } catch {}
    const e = new Error(detail || `${path} → ${r.status}`);
    e.status = r.status;
    throw e;
  }
  return r.json();
}

let settings = {};
function saveSettings() {
  if (!historyOn) return;
  api('/settings', { method: 'PUT', body: JSON.stringify(settings) }).catch(() => {});
}

async function initHistory(health) {
  historyOn = !!health?.history;
  $('erase-all').hidden = !historyOn;
  $('history-badge').hidden = historyOn;

  if (!historyOn) {
    $('chats').innerHTML = '<div class="side-empty">History is off — this machine ' +
      'can’t save to the drive. Conversations will be lost when you close the window.</div>';
    return;
  }
  try { settings = await api('/settings'); } catch { settings = {}; }
  savedModel = settings.model ?? null;
  useDocs = !!settings.useDocs;

  // Pick up where the last session stopped, the way restoring from browser
  // storage used to — only now it follows the drive, not the machine.
  const list = await refreshChats();
  if (list.length) await openChat(list[0].id);
}

// Grouped by age, the way every chat app does it, because "3 days ago" is how
// people remember a conversation — not by its position in a flat list.
function bucketOf(iso) {
  const t = new Date(iso).getTime();
  if (!t) return 'Earlier';
  const now = new Date();
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86400000;
  if (t >= startOfToday) return 'Today';
  if (t >= startOfToday - day) return 'Yesterday';
  if (t >= startOfToday - 7 * day) return 'Previous 7 days';
  if (t >= startOfToday - 30 * day) return 'Previous 30 days';
  return 'Earlier';
}

function drawChats() {
  const box = $('chats');
  const q = $('chat-search').value.trim().toLowerCase();
  const list = q ? chatList.filter((c) => c.title.toLowerCase().includes(q)) : chatList;

  box.innerHTML = '';
  if (!list.length) {
    box.innerHTML = `<div class="side-empty">${q ? 'No conversation matches that.'
      : 'No conversations yet.'}</div>`;
    return;
  }
  let bucket = null;
  for (const c of list) {
    const b = bucketOf(c.updated);
    if (b !== bucket && !q) {          // headings would fragment a search result
      bucket = b;
      const h = document.createElement('div');
      h.className = 'date-label';
      h.textContent = b;
      box.appendChild(h);
    }
    const row = document.createElement('div');
    row.className = 'row' + (c.id === chatId ? ' active' : '');
    row.innerHTML =
      `<button class="row-open" type="button" title="${esc(c.title)}">` +
        `${icon('msg')}<span>${esc(c.title)}</span></button>` +
      `<button class="row-del" type="button" title="Delete this conversation">${icon('x')}</button>`;
    row.querySelector('.row-open').onclick = () => openChat(c.id);
    row.querySelector('.row-del').onclick = async () => {
      try { await api('/chats/' + c.id, { method: 'DELETE' }); } catch {}
      if (c.id === chatId) newChat(); else refreshChats();
    };
    box.appendChild(row);
  }
}

async function refreshChats() {
  if (!historyOn) return [];
  try { chatList = await api('/chats'); } catch { return []; }
  drawChats();
  return chatList;
}

async function openChat(id) {
  controller?.abort();
  let c;
  try { c = await api('/chats/' + id); } catch { return; }
  chatId = c.id;
  // `history` is what gets replayed to the model, so it carries role and
  // content only — sources are for the reader, not for the prompt.
  history = c.messages.map(({ role, content }) => ({ role, content }));
  msgs.innerHTML = '';
  if (c.messages.length) {
    setEmpty(false);
    c.messages.forEach((m) => addMessage(m.role, m.content, m.sources));
  } else {
    setEmpty(true);
  }
  drawChats();
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

async function record(role, content, sources) {
  if (!historyOn || !chatId) return;
  try {
    await api('/chats/' + chatId, {
      method: 'POST',
      body: JSON.stringify({ role, content, ...(sources?.length ? { sources } : {}) }),
    });
  } catch {}
}

function newChat() {
  controller?.abort();
  history = [];
  chatId = null;
  msgs.innerHTML = '';
  setEmpty(true);
  $('tps').textContent = '—';
  drawChats();
  $('input').focus();
}

/* ---------- documents (retrieval) ---------- */

// Retrieval is lexical (BM25) inside pocketd. No embedding model on the drive,
// no second llama-server, nothing added to drive.lock — and for searching your
// own notes the words you ask with are usually the words you wrote. It will not
// match "car" against a document that only says "automobile"; that is the
// upgrade path, not a bug.
async function initDocs(health) {
  searchOn = !!health?.search;
  if (!searchOn) {
    $('add-docs').hidden = true;
    $('attach').hidden = true;
    $('doc-list').innerHTML =
      '<div class="side-empty">Unavailable — the drive can’t be written to here.</div>';
    return;
  }
  await refreshDocs();
}

async function refreshDocs() {
  if (!searchOn) return [];
  let list;
  try { list = await api('/docs'); } catch { return []; }
  docCount = list.length;

  const box = $('doc-list');
  box.innerHTML = '';
  for (const d of list) {
    const row = document.createElement('div');
    row.className = 'row';
    row.innerHTML =
      `<button class="row-open" type="button" title="${esc(d.name)} — ${d.chunks} passage(s) indexed">` +
        `${icon('file')}<span>${esc(d.name)}</span></button>` +
      `<button class="row-del" type="button" title="Remove this document from the drive">${icon('x')}</button>`;
    row.querySelector('.row-del').onclick = async () => {
      try { await api('/docs/' + d.id, { method: 'DELETE' }); } catch {}
      refreshDocs();
    };
    box.appendChild(row);
  }
  if (!list.length) {
    box.innerHTML = '<div class="side-empty">None yet — add text, PDF, Office ' +
      'or archive files to search them.</div>';
  }

  $('docs-label').textContent = list.length ? `Documents (${list.length})` : 'Documents';
  // Nothing to ground an answer in means the switch would be a lie.
  if (!list.length) useDocs = false;
  syncDocsToggle();
  if ($('main').classList.contains('is-empty')) setEmpty(true);   // refresh suggestions
  return list;
}

function syncDocsToggle() {
  const off = !searchOn || docCount === 0;
  $('use-docs').disabled = off;
  $('use-docs').checked = useDocs;
  $('docs-chip').disabled = off;
  $('docs-chip').classList.toggle('on', useDocs && !off);
}

function setUseDocs(on) {
  useDocs = on;
  settings.useDocs = on;
  saveSettings();
  syncDocsToggle();
}

// Queued rather than shown immediately: these are raised mid-upload, before the
// refreshDocs() that would erase them.
const pendingNotes = [];
const docsError = (m) => pendingNotes.push([m, 'side-error']);
const docsNote  = (m) => pendingNotes.push([m, 'side-note']);

function flushDocsMessages() {
  for (const [msg, cls] of pendingNotes.splice(0)) {
    const el = document.createElement('div');
    el.className = cls;
    el.textContent = msg;
    $('doc-list').prepend(el);
    setTimeout(() => el.remove(), 7000);
  }
}

async function addDocs(files) {
  if (!searchOn) return;
  for (const f of files) {
    try {
      // The File goes into the body as-is. Reading it to a string first would
      // corrupt every binary format, and base64 would inflate each upload by a
      // third for nothing. pocketd works out the format from ?name=.
      const added = await api('/docs?name=' + encodeURIComponent(f.name), {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: f,
      });
      // An archive comes back as one document per member; say so, because the
      // sidebar suddenly growing by nine entries otherwise looks like a bug.
      if (added.length > 1) docsNote(`${f.name}: added ${added.length} files from the archive.`);
    } catch (err) {
      docsError(`${f.name}: ${err.message}`);
    }
  }
  await refreshDocs();
  flushDocsMessages();
}

/* ---------------------------------------------------------- wiring */

const input = $('input');
const grow = () => {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 216) + 'px';
};
input.addEventListener('input', grow);
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); $('composer').requestSubmit(); }
});
$('composer').addEventListener('submit', (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text || busy) return;
  input.value = '';
  grow();
  submit(text);
});
$('stop').addEventListener('click', () => controller?.abort());
$('new-chat').addEventListener('click', newChat);
$('new-chat-top').addEventListener('click', newChat);

// Model picker
$('model-btn').addEventListener('click', (e) => {
  e.stopPropagation();
  const menu = $('model-menu');
  menu.hidden = !menu.hidden;
  $('model-btn').setAttribute('aria-expanded', String(!menu.hidden));
});
$('model-menu').addEventListener('click', (e) => {
  const item = e.target.closest('.menu-item');
  if (!item || item.disabled) return;
  $('model-menu').hidden = true;
  if (item.dataset.model === currentModel) return;
  setModel(item.dataset.model);
  // Chat templates and tokenizers differ per model; replaying one model's
  // transcript into another produces confused output. Start clean.
  newChat();
});
document.addEventListener('click', () => { $('model-menu').hidden = true; });
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { $('model-menu').hidden = true; $('chat-search').blur(); }
});

// Sidebar
const setSidebar = (open) => {
  document.body.classList.toggle('side-hidden', !open);
  document.body.classList.toggle('side-open', open);
  $('show-side').hidden = open;
};
$('toggle-side').addEventListener('click', () => setSidebar(false));
$('show-side').addEventListener('click', () => setSidebar(true));

$('toggle-search').addEventListener('click', () => {
  const w = $('search-wrap');
  w.hidden = !w.hidden;
  if (!w.hidden) $('chat-search').focus();
  else { $('chat-search').value = ''; drawChats(); }
});
$('chat-search').addEventListener('input', drawChats);

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
  await refreshChats();
  newChat();
});

// Documents
$('add-docs').addEventListener('click', () => $('doc-input').click());
$('attach').addEventListener('click', () => $('doc-input').click());
$('doc-input').addEventListener('change', async (e) => {
  const files = [...e.target.files];
  e.target.value = '';        // so the same file can be added again after a delete
  await addDocs(files);
});
$('use-docs').addEventListener('change', (e) => setUseDocs(e.target.checked));
$('docs-chip').addEventListener('click', () => setUseDocs(!useDocs));

// Message and code actions are delegated: a streaming message replaces its own
// innerHTML many times, so anything bound directly would be discarded.
document.addEventListener('click', (e) => {
  const copyCode = e.target.closest('.copy-code');
  if (copyCode) { copyText(copyCode.closest('pre').dataset.src, copyCode); return; }

  const act = e.target.closest('.act');
  if (act) {
    const turn = act.closest('.turn');
    if (act.dataset.act === 'copy') copyText(turn.dataset.text ?? '', act);
    if (act.dataset.act === 'again') regenerate();
    return;
  }
  const sugg = e.target.closest('.sugg');
  if (sugg) { input.value = sugg.dataset.fill + ' '; grow(); input.focus(); }
});

// Jump-to-latest, shown only when the view has actually drifted from the end.
msgs.addEventListener('scroll', () => {
  const away = msgs.scrollHeight - msgs.scrollTop - msgs.clientHeight;
  $('to-bottom').hidden = away < 160;
});
$('to-bottom').addEventListener('click', scrollToEnd);

// Dropping files anywhere on the window is the gesture people reach for first.
// preventDefault on both events matters: without it the browser navigates away
// to the dropped file and the session is gone.
document.addEventListener('dragover', (e) => e.preventDefault());
document.addEventListener('drop', (e) => {
  e.preventDefault();
  const files = [...(e.dataTransfer?.files ?? [])];
  if (files.length) addDocs(files);
});

(async () => {
  setEmpty(true);
  let health = {};
  try { health = await api('/health'); } catch {}
  await initHistory(health);
  await initDocs(health);
  await probe();
  setInterval(probe, 5000);
})();
