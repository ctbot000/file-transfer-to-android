import {
  $, el, icon, badge, formatSize, plural, progressBar, setProgress, encodePath, sendFile,
} from './common.js';

// The page is served at /<token>/, and every URL it uses is relative to
// that, so the token never has to be handled explicitly.
const base = location.pathname.replace(/\/?$/, '/');

class HttpError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function getJSON(path) {
  const res = await fetch(base + path, { cache: 'no-store' });
  if (!res.ok) throw new HttpError(res.status, (await res.text()).trim());
  return res.json();
}

let index = null; // { name, receive, items }
let loads = 0; // lets a newer load discard an older one's response
let source = null;
let isExpired = false;

// ---------- Navigation ----------
// '' is the list of shared items; '#/<item id>/<path>' is a folder in one.

function route() {
  const match = /^#\/([^/]+)\/?(.*)$/.exec(location.hash);
  if (!match) return null;
  const path = match[2].split('/').filter(Boolean).map(decodeURIComponent).join('/');
  return { item: decodeURIComponent(match[1]), path };
}

const folderHash = (item, path) => `#/${encodeURIComponent(item)}/${path ? encodePath(path) : ''}`;

async function load() {
  if (isExpired) return;
  const seq = ++loads;
  const where = route();
  let fresh;
  try {
    fresh = await getJSON('api/items');
  } catch (err) {
    if (seq !== loads) return;
    if (err instanceof HttpError && err.status === 404) expire();
    else setOnline(false);
    return;
  }
  let listing = null;
  if (where) {
    try {
      listing = await getJSON(`api/items/${encodeURIComponent(where.item)}/list?path=${encodeURIComponent(where.path)}`);
    } catch (err) {
      if (seq !== loads) return;
      if (err instanceof HttpError && err.status === 404) location.hash = ''; // the folder is gone
      else setOnline(false);
      return;
    }
  }
  if (seq !== loads) return;
  index = fresh;
  setOnline(true);
  document.title = `Files from ${index.name}`;
  $('#send').hidden = !index.receive;
  if (listing) renderFolder(listing);
  else renderIndex();
}

// ---------- Rendering ----------

function row({ href, name, dir = false, meta, download = false }) {
  const link = el('a', { class: 'row', href },
    badge(name, dir),
    el('div', { class: 'main' }, el('div', { class: 'name' }, name), el('div', { class: 'meta' }, meta)),
    el('span', { class: 'trail' }, icon(dir ? 'chevron' : 'download')));
  if (download) {
    link.setAttribute('download', name);
    link.addEventListener('click', () => toast(`Downloading ${name}…`));
  }
  return link;
}

function renderIndex() {
  $('#back').hidden = true;
  $('#eyebrow').textContent = 'Files from';
  $('#title').textContent = index.name;
  const { items } = index;
  $('#list').replaceChildren(...items.map((it) => (it.dir
    ? row({ href: folderHash(it.id, ''), name: it.name, dir: true, meta: `${plural(it.files, 'file')} · ${formatSize(it.size)}` })
    : row({ href: `f/${encodeURIComponent(it.id)}/${encodeURIComponent(it.name)}`, name: it.name, meta: formatSize(it.size), download: true }))));
  showEmpty(!items.length, 'Nothing shared yet', 'Add files on the computer and they show up here.');
  if (items.length > 1) {
    const total = items.reduce((sum, it) => sum + it.size, 0);
    showZip('z/all', `Download everything · ${formatSize(total)}`, 'Shared files.zip');
  } else {
    $('#zip').hidden = true;
  }
}

function renderFolder(listing) {
  const { item, name, path, entries } = listing;
  const top = index.items.find((it) => it.id === item);
  const parent = path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '';
  const back = $('#back');
  back.hidden = false;
  back.href = path ? folderHash(item, parent) : '#';
  $('#eyebrow').textContent = path ? [top?.name, parent].filter(Boolean).join(' / ') : `Files from ${index.name}`;
  $('#title').textContent = name;

  const sorted = [...entries].sort((a, b) => (b.dir - a.dir)
    || a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' }));
  $('#list').replaceChildren(...sorted.map((entry) => {
    const p = path ? `${path}/${entry.name}` : entry.name;
    return entry.dir
      ? row({ href: folderHash(item, p), name: entry.name, dir: true, meta: 'Folder' })
      : row({ href: `f/${encodeURIComponent(item)}/${encodePath(p)}`, name: entry.name, meta: formatSize(entry.size), download: true });
  }));
  showEmpty(!sorted.length, 'This folder is empty', '');
  if (sorted.length) {
    const size = !path && top ? ` · ${formatSize(top.size)}` : '';
    showZip(`z/${encodeURIComponent(item)}/${encodePath(path)}`, `Download this folder${size}`, `${name}.zip`);
  } else {
    $('#zip').hidden = true;
  }
}

function showEmpty(empty, title, text) {
  const box = $('#empty');
  box.hidden = !empty;
  if (empty) box.replaceChildren(el('strong', {}, title), text);
}

function showZip(href, label, filename) {
  const zip = $('#zip');
  zip.hidden = false;
  zip.href = href;
  zip.setAttribute('download', filename);
  zip.replaceChildren(icon('download'), el('span', {}, label));
  zip.onclick = () => toast('Preparing a ZIP file. It downloads in the background.');
}

let toastTimer;
function toast(message) {
  const box = $('#toast');
  box.textContent = message;
  box.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { box.hidden = true; }, 3000);
}

function setOnline(online) {
  $('#offline').hidden = online;
  const dot = $('#dot');
  dot.className = `dot ${online ? 'connected' : 'offline'}`;
  dot.setAttribute('aria-label', online ? 'Connected' : 'Not connected');
}

function expire() {
  isExpired = true;
  source?.close();
  setOnline(false);
  $('#offline-title').textContent = 'This link has expired';
  $('#offline-text').textContent = 'The app on the computer was restarted. Scan its new QR code.';
  for (const id of ['#files', '#zip', '#send']) $(id).hidden = true;
}

// ---------- Live updates ----------

function listen() {
  source = new EventSource(`${base}api/events`);
  source.addEventListener('changed', load);
  source.onopen = load; // also catches up after a reconnect
  source.onerror = () => {
    setOnline(false);
    if (source.readyState !== EventSource.CLOSED) return; // the browser retries by itself
    // The stream was refused rather than dropped: the link may be stale.
    getJSON('api/items').then(listen, (err) => {
      if (err instanceof HttpError && err.status === 404) expire();
      else setTimeout(listen, 3000);
    });
  };
}

// ---------- Sending files to the computer ----------

const queue = [];
let sending = false;

function enqueue(file) {
  const task = {
    file,
    state: 'waiting',
    request: null,
    meta: el('div', { class: 'meta' }),
    bar: progressBar(0),
    action: el('button', { class: 'icon-btn', type: 'button' }),
    done: el('span', { class: 'trail' }, icon('check')),
  };
  task.row = el('div', { class: 'row' },
    badge(file.name),
    el('div', { class: 'main' }, el('div', { class: 'name' }, file.name), task.meta, task.bar),
    task.action, task.done);
  task.action.onclick = () => {
    if (task.state === 'sending') task.request.abort();
    else if (task.state === 'waiting') settle(task, 'cancelled', 'Cancelled');
    else {
      task.state = 'waiting';
      pump();
    }
    show(task);
  };
  show(task);
  $('#uploads').append(task.row);
  queue.push(task);
  pump();
}

function show(task) {
  const { state, meta, bar, action, done, file } = task;
  if (state === 'waiting') meta.textContent = `Waiting · ${formatSize(file.size)}`;
  meta.classList.toggle('error', state === 'failed');
  bar.hidden = state !== 'sending';
  done.hidden = state !== 'done';
  action.hidden = state === 'done';
  const retry = state === 'failed' || state === 'cancelled';
  action.replaceChildren(icon(retry ? 'retry' : 'close'));
  action.title = retry ? 'Try again' : 'Cancel';
  action.setAttribute('aria-label', `${action.title}: ${file.name}`);
}

function settle(task, state, text) {
  task.state = state;
  task.meta.textContent = text;
  show(task);
}

async function pump() {
  if (sending) return;
  const task = queue.find((t) => t.state === 'waiting');
  $('#keep-open').hidden = !task;
  if (!task) return;
  sending = true;
  task.state = 'sending';
  setProgress(task.bar, 0);
  show(task);
  const { file } = task;
  task.meta.textContent = `0 B of ${formatSize(file.size)}`;
  task.request = sendFile(`upload?name=${encodeURIComponent(file.name)}&modified=${file.lastModified}`, file, {
    onProgress: (loaded) => {
      setProgress(task.bar, file.size ? loaded / file.size : 1);
      task.meta.textContent = `${formatSize(loaded)} of ${formatSize(file.size)}`;
    },
  });
  try {
    const saved = JSON.parse(await task.request.done);
    const as = saved.name === file.name ? '' : ` as ${saved.name}`;
    settle(task, 'done', `Sent${as} · ${formatSize(saved.size)}`);
  } catch (err) {
    if (err.message === 'Cancelled') settle(task, 'cancelled', 'Cancelled');
    else settle(task, 'failed', err.message);
  }
  sending = false;
  pump();
}

// ---------- Start ----------

$('#back').append(icon('back'));
$('#choose').prepend(icon('upload'));
$('#choose').onclick = () => $('#file-input').click();
$('#file-input').onchange = (e) => {
  for (const file of e.target.files) enqueue(file);
  e.target.value = '';
};
window.addEventListener('hashchange', load);
listen();
load();
