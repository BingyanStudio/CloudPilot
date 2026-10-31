import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: Object.fromEntries(
      ['/api', '/hooks', '/health'].map((path) => [
        path,
        {
          target: 'http://127.0.0.1:8080',
          changeOrigin: false,
        },
      ]),
    ),
  },
  css: { preprocessorOptions: { scss: { quietDeps: true } } },
  build: { chunkSizeWarningLimit: 1000 },
});
