/// <reference types="vitest/config" />
import { fileURLToPath, URL } from "node:url"

import tailwindcss from "@tailwindcss/vite"
import { tanstackRouter } from "@tanstack/router-plugin/vite"
import basicSsl from "@vitejs/plugin-basic-ssl"
import react from "@vitejs/plugin-react"
import { createLogger, defineConfig } from "vite"

// React Compiler emits a Todo diagnostic per function it cannot compile. Those
// are its own unimplemented syntax, not defects here, and they bury the build
// output. Other compiler diagnostics still surface.
const logger = createLogger()
const warn = logger.warn.bind(logger)
logger.warn = (msg, options) => {
  if (msg.includes("react-compiler(Todo)")) {
    return
  }
  warn(msg, options)
}

export default defineConfig(({ mode }) => ({
  customLogger: logger,
  envDir: "../",
  build: {
    target: "es2023",
    chunkSizeWarningLimit: 1500,
    rolldownOptions: {
      output: {
        // Minified export letters are positional, so a chunk only ever agrees
        // with the build it was emitted with. The hash keeps a cached chunk
        // from a previous deploy from resolving against a newer entry.
        entryFileNames: "assets/[name].[hash].js",
        chunkFileNames: "assets/[name].[hash].js",
        assetFileNames: "assets/[name].[hash].[ext]",
      },
    },
  },
  plugins: [
    basicSsl(),
    tanstackRouter({
      target: "react",
      autoCodeSplitting: true,
      routeFileIgnorePattern: "\\.test\\.(ts|tsx)$",
    }),
    react({ compiler: mode !== "test" }),
    tailwindcss(),
  ],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("src", import.meta.url)),
    },
  },
  test: {
    globals: true,
    environment: "happy-dom",
    setupFiles: "./src/test/setup.ts",
    clearMocks: true,
    pool: "threads",
    coverage: {
      include: ["src/**/*.{ts,tsx}"],
      exclude: [
        "src/components/ui/**",
        "src/layouts/**",
        "src/routes/*.{ts,tsx}",
        "src/routes/**/!(-components)/*.{ts,tsx}",
        "src/test/**",
        "src/**/*.test.*",
        "src/main.tsx",
        "src/routeTree.gen.ts",
      ],
      thresholds: {
        lines: 70,
      },
    },
  },
}))
