import { fileURLToPath } from 'node:url'
import license from 'rollup-plugin-license'
import { generateMaterialIcons } from './scripts/material-icons.mjs'

export default defineNuxtConfig({
  modules: ['@nuxt/eslint', '@nuxt/ui', '@nuxtjs/i18n', '@vueuse/nuxt', '@pinia/nuxt'],
  ssr: false,
  devtools: { enabled: false },
  app: {
    head: {
      title: 'PKUDisk',
      link: [
        { rel: 'icon', type: 'image/png', href: '/icon.png' },
        { rel: 'apple-touch-icon', href: '/apple-touch-icon.png' }
      ]
    }
  },
  css: ['~/assets/css/main.css'],
  colorMode: {
    storage: 'cookie',
    storageKey: 'pkudisk-color-mode',
    // The transition helper inserts an unnonced style. Keep normal transitions
    // so changing themes also works under the desktop server's strict CSP.
    disableTransition: false
  },
  ui: { fonts: false },
  experimental: { appManifest: false },
  compatibilityDate: '2026-10-13',
  nitro: { prerender: { crawlLinks: false, routes: ['/'] } },
  telemetry: false,
  vite: {
    optimizeDeps: {
      // These client-only modules are discovered after the SPA boots. Listing
      // them here prevents Vite from reloading a newly authenticated page.
      include: [
        '@uppy/core',
        '@uppy/xhr-upload',
        'browser-fs-access',
        'pretty-bytes',
        'lru-cache',
        'p-limit',
        'zod',
        'zod/v4/locales/en.js',
        'zod/v4/locales/zh-CN.js'
      ]
    }
  },
  hooks: {
    ready: generateMaterialIcons,
    'vite:extendConfig': (config, { isClient }) => {
      if (isClient) {
        config.plugins?.push(
          license({
            thirdParty: {
              multipleVersions: true,
              output: {
                file: fileURLToPath(new URL('./.nuxt/THIRD_PARTY_LICENSES.txt', import.meta.url))
              }
            }
          })
        )
      }
    }
  },
  i18n: {
    strategy: 'no_prefix',
    defaultLocale: 'zh-CN',
    locales: [
      { code: 'zh-CN', language: 'zh-CN', name: '简体中文', file: 'zh-CN.json' },
      { code: 'en', language: 'en', name: 'English', file: 'en.json' }
    ],
    detectBrowserLanguage: { useCookie: true, cookieKey: 'pkudisk-locale', redirectOn: 'root' }
  },
  icon: {
    mode: 'svg',
    provider: 'none',
    fallbackToApi: false,
    clientBundle: {
      scan: { globInclude: ['app/**/*.{vue,ts}'] }
    }
  }
})
