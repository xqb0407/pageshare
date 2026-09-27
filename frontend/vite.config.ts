import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// build.outDir 指到 Go 侧的 embed 目录，vite build 一次产物直接可被内嵌
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // 管理页挂在 /admin/ 下，资产路径必须带上前缀
  base: "/admin/",
  resolve: {
    alias: { "@": path.resolve(__dirname, "src") },
  },
  server: {
    port: 5273,
    proxy: {
      "/api": "http://127.0.0.1:8300",
      "/s": "http://127.0.0.1:8300",
    },
  },
  build: {
    outDir: "../internal/server/web/dist",
    emptyOutDir: true,
  },
});
