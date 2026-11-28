/// <reference lib="webworker" />

// @ts-check
const CACHE = 'focusgate-shell-v37';
const REQUIRED_ASSETS = ['/', '/style.css?v=35', '/app.js?v=35'];
const OPTIONAL_ASSETS = [
  '/manifest.webmanifest',
  '/icon.svg',
  '/icon-192.png',
  '/icon-512.png',
];
const SHELL_ASSETS = new Set([...REQUIRED_ASSETS, ...OPTIONAL_ASSETS]);

const sw = /** @type {ServiceWorkerGlobalScope} */ (/** @type {unknown} */ (self));

sw.addEventListener('install', (/** @type {ExtendableEvent} */ e) => {
  e.waitUntil(
    caches.open(CACHE).then(async (cache) => {
      // Required core assets: failing to cache these rejects installation
      await cache.addAll(REQUIRED_ASSETS);
      // Optional assets: cache opportunistically
      await Promise.all(
        OPTIONAL_ASSETS.map((asset) =>
          fetch(asset)
            .then((resp) => {
              if (resp.ok) return cache.put(asset, resp);
            })
            .catch(() => {})
        )
      );
      return sw.skipWaiting();
    })
  );
});

sw.addEventListener('activate', (/** @type {ExtendableEvent} */ e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith('focusgate-shell-') && k !== CACHE).map((k) => caches.delete(k))))
      .then(async () => {
        const cache = await caches.open(CACHE);
        for (const request of await cache.keys()) {
          if (new URL(request.url).searchParams.has('token')) await cache.delete(request);
        }
      })
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
        if (networkRes.ok && url.origin === sw.location.origin && SHELL_ASSETS.has(url.pathname + url.search)) {
          const clone = networkRes.clone();
          caches.open(CACHE).then((cache) => cache.put(e.request, clone));
        }
        return networkRes;
      })
      .catch(() =>
        caches
          .match(e.request)
          .then((res) => res || caches.match(e.request, { ignoreSearch: true }))
          .then((res) => res || (e.request.mode === 'navigate' ? caches.match('/') : null))
          .then((res) => res || new Response('Offline', { status: 503 }))
      )
  );
});
