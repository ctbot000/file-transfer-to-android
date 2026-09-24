import {
  $, el, icon, badge, formatSize, plural, progressBar, setProgress, encodePath, clock, sendFile,
} from './common.js';

// The admin key arrives in the URL fragment, which is never sent to the
// server with the page request itself.
const key = new URLSearchParams(location.hash.slice(1)).get('k');
const isMac = /Mac/.test(navigator.platform || navigator.userAgent);

let data = null; // the latest state pushed by the server
let linkIndex = 0; // which phone link the QR code shows
const pending = new Map(); // stage id -> files being copied in

if (key) {
  $('#app').hidden = false;
  $('#footer').hidden = false;
  $('#copy').append(icon('copy'));
  $('#copy').onclick = copyLink;
  $('#quit').onclick = quit;
  $('#addr').onchange = (e) => {
    linkIndex = Number(e.target.value);
    renderConnect();
  };
  setupAdding();
  connect();
} else {
  showNoKey(false);
}

function api(path, options = {}) {
  return fetch(path, { ...options, headers: { 'X-Admin-Key': key, ...options.headers } }).then(async (res) => {
    if (!res.ok) {
      const message = (await res.text()).trim() || res.statusText;
      throw Object.assign(new Error(message), { status: res.status });
    }
    return res;
  });
}

const postJSON = (path, body) =>
  api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

// ---------- Live state ----------

function connect() {
  const source = new EventSource(`/api/events?key=${encodeURIComponent(key)}`);
  source.addEventListener('state', (e) => {
    data = JSON.parse(e.data);
    setOffline(false);
    render();
  });
  source.addEventListener('transfers', (e) => {
    if (!data) return;
    data.transfers = JSON.parse(e.data);
    renderActivity();
  });
  source.onerror = () => {
    if (stopped) {
      source.close();
      return;
    }
    setOffline(true);
    if (source.readyState !== EventSource.CLOSED) return; // the browser retries by itself
    // The stream was refused rather than cut: find out why.
    api('/api/ping').then(connect, (err) => {
      if (err.status === 401) showNoKey(true);
      else setTimeout(connect, 2000);
    });
  };
}

function setOffline(offline) {
  if (stopped) return;
  $('#offline').hidden = !offline;
  if (offline) setStatus('offline', 'Not running');
}

let stopped = false;

async function quit() {
  if (!confirm('Stop sharing? The phone link stops working until you start the app again.')) return;
  try {
    await api('/api/quit', { method: 'POST' });
  } catch (err) {
    alert(`Could not stop the app: ${err.message}`);
    return;
  }
  stopped = true;
  for (const id of ['#app', '#footer', '#offline']) $(id).hidden = true;
  $('#stopped').hidden = false;
  setStatus('offline', 'Stopped');
}

function showNoKey(expired) {
  $('#app').hidden = true;
  $('#footer').hidden = true;
  $('#offline').hidden = true;
  $('#nokey').hidden = false;
  if (expired) {
    $('#nokey h2').textContent = 'This page is from an earlier session';
  }
  setStatus('offline', 'No session');
}

function setStatus(className, text) {
  $('#status').className = `status-pill ${className}`;
  $('#status-text').textContent = text;
}

function render() {
  const phones = data.phones.length;
  if (phones) setStatus('connected', phones === 1 ? 'Phone connected' : `${phones} phones connected`);
  else setStatus('', 'Waiting for your phone');
  $('#version').textContent = `File Transfer to Android ${data.version}`;
  renderConnect();
  renderShared();
  renderActivity();
  renderReceived();
}

// ---------- Connect card ----------

function renderConnect() {
  const { links } = data;
  const qr = $('#qr');
  if (!links.length) {
    qr.className = 'qr none';
    qr.textContent = 'No network connection. Connect this computer to Wi-Fi.';
    delete qr.dataset.url;
    $('#url').textContent = '—';
    $('#addr-wrap').hidden = true;
    return;
  }
  if (linkIndex >= links.length) linkIndex = 0;
  const link = links[linkIndex];
  if (qr.dataset.url !== link.url) {
    qr.className = 'qr';
    qr.innerHTML = link.qr; // SVG the server rendered from the URL
    qr.dataset.url = link.url;
  }
  $('#url').textContent = link.url;

  const hosts = links.map((l) => new URL(l.url).host);
  const select = $('#addr');
  if (select.dataset.hosts !== hosts.join()) {
    select.replaceChildren(...hosts.map((host, i) => el('option', { value: i }, host)));
    select.dataset.hosts = hosts.join();
  }
  select.value = String(linkIndex);
  $('#addr-wrap').hidden = links.length < 2;
}

async function copyLink() {
  const url = $('#url').textContent;
  const button = $('#copy');
  try {
    await navigator.clipboard.writeText(url);
    button.replaceChildren(icon('check'));
    setTimeout(() => button.replaceChildren(icon('copy')), 1500);
  } catch {
    getSelection().selectAllChildren($('#url'));
  }
}

// ---------- Shared items ----------

function renderShared() {
  if (!data) return;
  const { items } = data;
  $('#shared-list').replaceChildren(
    ...[...pending.values()].map((job) => job.row),
    ...items.map(itemRow),
  );
  const total = items.reduce((sum, it) => sum + it.size, 0);
  $('#shared-sub').textContent = items.length ? `${plural(items.length, 'item')} · ${formatSize(total)}` : 'Nothing yet';

  const zone = $('#dropzone');
  const empty = !items.length && !pending.size;
  zone.className = empty ? 'dropzone big' : 'dropzone';
  zone.replaceChildren(...(empty
    ? [icon('upload'), el('strong', {}, 'Drop files or folders here'),
      el('span', {}, 'They show up on your phone right away.')]
    : [el('span', {}, 'Drop more files or folders anywhere on this page.')]));
}

function itemRow(it) {
  let meta = it.dir ? `${plural(it.files, 'file')} · ${formatSize(it.size)}` : formatSize(it.size);
  if (it.missing) meta = 'Missing: moved or deleted on this computer';
  return el('div', { class: `row${it.missing ? ' missing' : ''}` },
    badge(it.name, it.dir),
    el('div', { class: 'main' },
      el('div', { class: 'name', title: it.path }, it.name),
      el('div', { class: 'meta' }, meta)),
    el('button', {
      class: 'icon-btn',
      type: 'button',
      title: 'Stop sharing',
      'aria-label': `Stop sharing ${it.name}`,
      onclick: () => api(`/api/items/${it.id}`, { method: 'DELETE' }).catch((err) => alert(err.message)),
    }, icon('close')));
}

function setupAdding() {
  $('#add-files').onclick = () => $('#pick-files').click();
  $('#add-folder').onclick = () => $('#pick-folder').click();
  $('#pick-files').onchange = (e) => {
    for (const file of e.target.files) stage({ name: file.name, dir: false, files: [{ file, path: file.name }] });
    e.target.value = '';
  };
  $('#pick-folder').onchange = (e) => {
    const jobs = jobsFromRelativePaths([...e.target.files]);
    e.target.value = '';
    jobs.forEach(stage);
  };

  const overlay = $('#drag-overlay');
  const carriesFiles = (e) => [...(e.dataTransfer?.types || [])].includes('Files');
  let depth = 0; // dragenter/dragleave fire for every child element crossed
  window.addEventListener('dragenter', (e) => {
    if (!carriesFiles(e)) return;
    e.preventDefault();
    depth++;
    overlay.hidden = false;
  });
  window.addEventListener('dragover', (e) => {
    if (!carriesFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  window.addEventListener('dragleave', (e) => {
    if (!carriesFiles(e)) return;
    depth = Math.max(0, depth - 1);
    if (!depth) overlay.hidden = true;
  });
  window.addEventListener('drop', (e) => {
    if (!carriesFiles(e)) return;
    e.preventDefault();
    depth = 0;
    overlay.hidden = true;
    // The entries must be taken now: the DataTransfer is emptied once this
    // handler returns.
    const entries = [...e.dataTransfer.items]
      .filter((item) => item.kind === 'file')
      .map((item) => item.webkitGetAsEntry?.())
      .filter(Boolean);
    if (entries.length) {
      for (const entry of entries) jobFromEntry(entry).then(stage, (err) => alert(`Could not read ${entry.name}: ${err.message}`));
    } else {
      for (const file of e.dataTransfer.files) stage({ name: file.name, dir: false, files: [{ file, path: file.name }] });
    }
  });
}

const fileOf = (entry) => new Promise((resolve, reject) => entry.file(resolve, reject));

async function jobFromEntry(entry) {
  if (entry.isFile) return { name: entry.name, dir: false, files: [{ file: await fileOf(entry), path: entry.name }] };
  const files = [];
  await collect(entry, entry.name, files);
  return { name: entry.name, dir: true, files };
}

async function collect(dirEntry, path, out) {
  const reader = dirEntry.createReader();
  for (;;) {
    // readEntries returns the folder in batches; an empty one means done.
    const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
    if (!batch.length) return;
    for (const entry of batch) {
      if (entry.name.startsWith('.')) continue; // .DS_Store and friends
      const childPath = `${path}/${entry.name}`;
      if (entry.isFile) out.push({ file: await fileOf(entry), path: childPath });
      else if (entry.isDirectory) await collect(entry, childPath, out);
    }
  }
}

// Groups the files of a folder picker by their top-level folder.
function jobsFromRelativePaths(files) {
  const jobs = new Map();
  for (const file of files) {
    const path = file.webkitRelativePath || file.name;
    const parts = path.split('/');
    if (parts.some((part) => part.startsWith('.'))) continue;
    if (!jobs.has(parts[0])) jobs.set(parts[0], { name: parts[0], dir: parts.length > 1, files: [] });
    jobs.get(parts[0]).files.push({ file, path });
  }
  return [...jobs.values()];
}

// Copies one dropped file or folder to the app, a few files at a time,
// then shares it. Browsers never reveal a dropped file's path on disk, so
// the page has to send the contents.
async function stage(job) {
  const id = crypto.randomUUID().replaceAll('-', '');
  const total = job.files.reduce((sum, f) => sum + f.file.size, 0);
  const meta = el('div', { class: 'meta' }, 'Copying…');
  const bar = progressBar(total ? 0 : null);
  const cancel = el('button', { class: 'icon-btn', type: 'button', title: 'Cancel', 'aria-label': `Cancel adding ${job.name}` }, icon('close'));
  const row = el('div', { class: 'row' },
    badge(job.name, job.dir),
    el('div', { class: 'main' }, el('div', { class: 'name' }, job.name), meta, bar),
    cancel);
  const task = { row, stopped: false, requests: new Map() };
  pending.set(id, task);
  const discard = () => api(`/api/stage/${id}`, { method: 'DELETE' }).catch(() => {});
  const stop = () => {
    task.stopped = true;
    for (const request of task.requests.keys()) request.abort();
  };
  cancel.onclick = () => {
    stop();
    pending.delete(id);
    discard();
    renderShared();
  };
  renderShared();

  let copied = 0; // bytes of finished files
  const update = () => {
    const now = copied + [...task.requests.values()].reduce((a, b) => a + b, 0);
    if (total) setProgress(bar, now / total);
    meta.textContent = `Copying… ${formatSize(now)} of ${formatSize(total)}`;
  };
  const queue = [...job.files];
  const worker = async () => {
    while (queue.length && !task.stopped) {
      const { file, path } = queue.shift();
      const request = sendFile(`/api/stage/${id}/${encodePath(path)}?modified=${file.lastModified}`, file, {
        method: 'PUT',
        headers: { 'X-Admin-Key': key },
        onProgress: (loaded) => {
          task.requests.set(request, loaded);
          update();
        },
      });
      task.requests.set(request, 0);
      try {
        await request.done;
      } finally {
        task.requests.delete(request);
      }
      copied += file.size;
      update();
    }
  };

  try {
    await Promise.all(Array.from({ length: Math.min(3, Math.max(1, job.files.length)) }, worker));
    if (task.stopped) return;
    await postJSON(`/api/stage/${id}/share`, { name: job.name, dir: job.dir });
    pending.delete(id);
    renderShared();
  } catch (err) {
    if (task.stopped) return;
    stop();
    discard();
    meta.textContent = `Could not add: ${err.message}`;
    meta.classList.add('error');
    bar.hidden = true;
    cancel.title = 'Dismiss';
    cancel.onclick = () => {
      pending.delete(id);
      renderShared();
    };
  }
}

// ---------- Activity ----------

const activityRows = new Map(); // transfer id -> { row, meta, bar, done }

function renderActivity() {
  const shown = data.transfers.slice(0, 12);
  $('#activity').hidden = !shown.length;
  const list = $('#activity-list');
  const ids = shown.map((t) => t.id).join();
  if (list.dataset.ids !== ids) {
    const keep = new Set(shown.map((t) => t.id));
    for (const id of activityRows.keys()) if (!keep.has(id)) activityRows.delete(id);
    list.replaceChildren(...shown.map((t) => (activityRows.get(t.id) || createTransferRow(t)).row));
    list.dataset.ids = ids;
  }
  for (const t of shown) updateTransferRow(activityRows.get(t.id), t);
}

function createTransferRow(t) {
  const sending = t.direction === 'send';
  const parts = {
    meta: el('div', { class: 'meta' }),
    bar: progressBar(0),
    done: el('span', { class: 'trail' }, icon('check')),
  };
  parts.row = el('div', { class: 'row' },
    el('span', { class: `badge ${sending ? 'dir-send' : 'dir-receive'}`, title: sending ? 'To the phone' : 'From the phone' },
      icon(sending ? 'download' : 'upload')),
    el('div', { class: 'main' }, el('div', { class: 'name' }, t.name), parts.meta, parts.bar),
    parts.done);
  activityRows.set(t.id, parts);
  return parts;
}

function updateTransferRow(parts, t) {
  const sending = t.direction === 'send';
  const known = t.total > 0;
  let text;
  if (t.state === 'active') {
    const seconds = Math.max(0.5, (Date.now() - Date.parse(t.started)) / 1000);
    const rate = `${formatSize(Math.round(t.bytes / seconds))}/s`;
    text = known ? `${formatSize(t.bytes)} of ${formatSize(t.total)} · ${rate}` : `${formatSize(t.bytes)} · ${rate}`;
    setProgress(parts.bar, known ? t.bytes / t.total : null);
  } else if (t.state === 'done') {
    text = `${sending ? 'Sent to' : 'Received from'} ${t.peer} · ${formatSize(known ? t.total : t.bytes)} · ${clock(t.ended)}`;
  } else {
    const where = known ? `${Math.floor((t.bytes * 100) / t.total)}%` : formatSize(t.bytes);
    text = `${sending ? 'Sending' : 'Receiving'} stopped at ${where} · ${clock(t.ended)}`;
  }
  parts.meta.textContent = text;
  parts.meta.classList.toggle('error', t.state === 'failed');
  parts.bar.hidden = t.state !== 'active';
  parts.done.hidden = t.state !== 'done';
}

// ---------- Received files ----------

function renderReceived() {
  const { receive, received } = data;
  $('#received').hidden = !receive.enabled;
  if (!receive.enabled) return;
  $('#received-sub').textContent = `Saved to ${receive.dir}`;
  $('#open-folder').onclick = () => reveal(receive.path);
  $('#received-empty').hidden = received.length > 0;
  $('#received-list').replaceChildren(...received.slice(0, 50).map((f) => el('div', { class: 'row' },
    badge(f.name),
    el('div', { class: 'main' },
      el('div', { class: 'name', title: f.path }, f.name),
      el('div', { class: 'meta' }, `${formatSize(f.size)} · ${clock(f.at)}`)),
    el('button', { class: 'btn', type: 'button', onclick: () => reveal(f.path) }, isMac ? 'Show in Finder' : 'Show'))));
}

function reveal(path) {
  postJSON('/api/reveal', { path }).catch((err) => alert(`Could not open it: ${err.message}`));
}
