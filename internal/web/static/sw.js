/// <reference lib="webworker" />

// @ts-check
const CACHE = 'focusgate-shell-v19';
const ASSETS = ['/', '/style.css?v=19', '/app.js?v=19', '/manifest.webmanifest', '/icon.svg'];

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
  if (e.request.method !== 'GET') return;

  e.respondWith(
    fetch(e.request)
      .then((networkRes) => {
        if (networkRes.ok && url.origin === sw.location.origin) {
          const clone = networkRes.clone();
          caches.open(CACHE).then((cache) => cache.put(e.request, clone));
        }
        return networkRes;
      })
      .catch(() => caches.match(e.request).then((res) => res || new Response('Offline', { status: 503 })))
  );
});
