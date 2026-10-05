import { defineConfig } from 'vite'
import { devtools } from '@tanstack/devtools-vite'
import { tanstackStart } from '@tanstack/react-start/plugin/vite'
import viteReact, { reactCompilerPreset } from '@vitejs/plugin-react'
import babel from '@rolldown/plugin-babel'
import tailwindcss from '@tailwindcss/vite'

// The pane is a static SPA embedded in the Go binary; in development, Vite
// forwards API calls to `embolt serve` on :9090.
export default defineConfig({
  resolve: { tsconfigPaths: true },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:9090',
      '/metrics': 'http://127.0.0.1:9090',
    },
  },
  // The SPA shell is prerendered through a preview server; an explicit IPv4
  // host avoids localhost resolving to ::1 on some platforms.
  preview: { host: '127.0.0.1' },
  plugins: [
    devtools(),
    tailwindcss(),
    tanstackStart({ spa: { enabled: true } }),
    viteReact(),
    babel({ presets: [reactCompilerPreset()] }),
  ],
})
