/// <reference lib="webworker" />

// @ts-check
const CACHE = 'focusgate-shell-v15';
const ASSETS = ['/', '/style.css', '/app.js', '/manifest.webmanifest', '/icon.svg'];

const sw = /** @type {ServiceWorkerGlobalScope} */ (/** @type {unknown} */ (self));

sw.addEventListener('install', (/** @type {ExtendableEvent} */ e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(ASSETS)).then(() => sw.skipWaiting()));
});

sw.addEventListener('activate', (/** @type {ExtendableEvent} */ e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith('focusgate-shell-') && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => sw.clients.claim())
  );
});

sw.addEventListener('fetch', (/** @type {FetchEvent} */ e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith('/api/')) return;
  e.respondWith(
    fetch(e.request).catch(() => caches.match(e.request).then((res) => res || new Response('Offline', { status: 503 })))
  );
});
