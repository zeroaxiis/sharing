import { defineConfig } from 'wxt';

// See https://wxt.dev/api/config.html
export default defineConfig({
  modules: ['@wxt-dev/module-react'],
  srcDir: '.',

  // Both targets ship MV3. WXT defaults Firefox to MV2, and MV2 has no
  // `host_permissions` key — it folds those origins into `permissions`, which
  // gives the two browsers different permission models for the same code.
  // Firefox MV3 landed in 109, which is already our strict_min_version.
  manifestVersion: 3,

  manifest: ({ browser }) => ({
    name: 'Nearby Share',
    description:
      'Send text, links and files between your own devices over the local network. No cloud, no account.',
    version: '0.1.0',
    permissions: ['storage'],
    // "ws://" is not a valid host_permissions scheme; the http:// entry is what
    // covers the WebSocket upgrade to ws://127.0.0.1:8765/ws.
    host_permissions: ['http://127.0.0.1:8765/*', 'http://localhost:8765/*'],

    // Gecko-only key. Chrome logs "Unrecognized manifest key" when it is
    // present, so it is emitted for the Firefox target only.
    ...(browser === 'firefox'
      ? {
          browser_specific_settings: {
            gecko: {
              id: 'nearby-share@zeroaxiis.dev',
              strict_min_version: '109.0',
              // Required by AMO for new extensions since 2025-11-03. Nothing
              // leaves the local network, so nothing is collected.
              data_collection_permissions: { required: ['none'] },
            },
          },
        }
      : {}),
  }),
});
