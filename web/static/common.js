// Helpers shared by the desktop and phone pages. File names come from
// other people's devices, so everything here builds DOM nodes with
// textContent; nothing user-provided is ever parsed as HTML.

export const $ = (selector) => document.querySelector(selector);

// el('div', { class: 'row', onclick: fn }, child, 'text', ...)
export function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value == null || value === false) continue;
    if (key === 'class') node.className = value;
    else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    node.append(child instanceof Node ? child : String(child));
  }
  return node;
}

const ICONS = {
  folder: '<svg viewBox="0 0 24 24"><path fill="#e8a000" d="M2.5 6.5A2.5 2.5 0 0 1 5 4h4.2c.7 0 1.3.3 1.8.8L12.6 6.5H19a2.5 2.5 0 0 1 2.5 2.5v8.5A2.5 2.5 0 0 1 19 20H5a2.5 2.5 0 0 1-2.5-2.5z"/><path fill="#ffc53d" d="M2.5 9.5h19v8A2.5 2.5 0 0 1 19 20H5a2.5 2.5 0 0 1-2.5-2.5z"/></svg>',
  download: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 4v11M7.5 10.5 12 15l4.5-4.5M5 19.5h14"/></svg>',
  upload: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 15V4M7.5 8.5 12 4l4.5 4.5M5 19.5h14"/></svg>',
  chevron: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 5 7 7-7 7"/></svg>',
  back: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 5l-7 7 7 7"/></svg>',
  close: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6.5 6.5l11 11M17.5 6.5l-11 11"/></svg>',
  copy: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2.5"/><path d="M5 15V6.5A2.5 2.5 0 0 1 7.5 4H15"/></svg>',
  check: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="m5 12.5 4.5 4.5L19 7.5"/></svg>',
  retry: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7"/></svg>',
  reveal: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="6.5"/><path d="m16 16 4 4"/></svg>',
  plus: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>',
};

export function icon(name) {
  const template = document.createElement('template');
  template.innerHTML = ICONS[name];
  const svg = template.content.firstElementChild;
  svg.setAttribute('aria-hidden', 'true');
  svg.classList.add('icon');
  return svg;
}

// SI units, like Android and macOS show.
export function formatSize(bytes) {
  if (bytes < 1000) return `${bytes} B`;
  let value = bytes;
  for (const unit of ['kB', 'MB', 'GB', 'TB']) {
    value /= 1000;
    if (value < 999.95 || unit === 'TB') {
      const digits = value < 9.995 ? 2 : value < 99.95 ? 1 : 0;
      return `${value.toFixed(digits)} ${unit}`;
    }
  }
  return `${bytes} B`;
}

export const plural = (n, word) => `${n.toLocaleString()} ${word}${n === 1 ? '' : 's'}`;

const KINDS = [
  ['image', /^(jpe?g|png|gif|webp|heic|heif|avif|bmp|tiff?|svg|dng|raw|cr2|cr3|nef|arw)$/],
  ['video', /^(mp4|m4v|mov|mkv|webm|avi|3gp|wmv|flv|mts|m2ts)$/],
  ['audio', /^(mp3|m4a|aac|flac|wav|ogg|opus|wma|aiff?|mid|midi|amr)$/],
  ['app', /^(apk|apks|xapk|apkm|aab)$/],
  ['archive', /^(zip|rar|7z|tar|gz|tgz|bz2|xz|zst)$/],
  ['doc', /^(pdf|docx?|xlsx?|pptx?|odt|ods|odp|rtf|txt|md|csv|json|epub|pages|numbers|key|srt)$/],
];

export function badge(name, dir = false) {
  if (dir) return el('span', { class: 'badge folder', 'aria-hidden': 'true' }, icon('folder'));
  const dot = name.lastIndexOf('.');
  const ext = dot > 0 ? name.slice(dot + 1).toLowerCase() : '';
  const kind = (KINDS.find(([, pattern]) => pattern.test(ext)) || ['other'])[0];
  const label = ext && ext.length <= 4 ? ext.toUpperCase() : 'FILE';
  return el('span', { class: `badge ${kind}`, 'aria-hidden': 'true' }, label);
}

export function progressBar(fraction) {
  const bar = el('span');
  const track = el('div', { class: 'progress' }, bar);
  setProgress(track, fraction);
  return track;
}

// fraction in [0, 1], or null for "unknown".
export function setProgress(track, fraction) {
  track.classList.toggle('indeterminate', fraction == null);
  track.firstElementChild.style.width = fraction == null ? '' : `${Math.round(Math.min(1, fraction) * 1000) / 10}%`;
}

// Encodes each element of a slash-separated path for use in a URL.
export const encodePath = (path) => path.split('/').map(encodeURIComponent).join('/');

export const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

// Sends a file as the raw request body, reporting upload progress, which
// fetch() cannot do. Returns { done: Promise, abort() }.
export function sendFile(url, file, { method = 'POST', headers = {}, onProgress } = {}) {
  const xhr = new XMLHttpRequest();
  const done = new Promise((resolve, reject) => {
    xhr.open(method, url);
    for (const [name, value] of Object.entries(headers)) xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = (e) => onProgress?.(e.loaded);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve(xhr.responseText);
      else reject(new Error(xhr.responseText.trim() || `Failed (HTTP ${xhr.status})`));
    };
    xhr.onerror = () => reject(new Error('Connection lost'));
    xhr.onabort = () => reject(new Error('Cancelled'));
    xhr.send(file);
  });
  return { done, abort: () => xhr.abort() };
}
