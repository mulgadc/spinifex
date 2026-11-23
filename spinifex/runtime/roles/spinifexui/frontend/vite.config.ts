/// <reference types="vitest/config" />
import { existsSync, readFileSync } from "node:fs"
import { fileURLToPath, URL } from "node:url"

import tailwindcss from "@tailwindcss/vite"
import { tanstackRouter } from "@tanstack/router-plugin/vite"
import basicSsl from "@vitejs/plugin-basic-ssl"
import react from "@vitejs/plugin-react"
import { cn as cnTables } from "cn/vite"
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

// /api and /proxy (the AWS gateway and S3 routes) are served by the Go console,
// so dev forwards them there. Its certificate is signed by the cluster CA,
// which Node does not trust by default.
const clusterCaPath = process.env.SPINIFEX_CA ?? "/etc/spinifex/ca.pem"

const consoleProxy = {
  target: "https://localhost:3000",
  ...(existsSync(clusterCaPath) && {
    ca: readFileSync(clusterCaPath, "utf-8"),
  }),
}

export default defineConfig(({ mode }) => ({
  customLogger: logger,
  envDir: "../",
  server: {
    proxy: { "^/(?:api|proxy)/": consoleProxy },
  },
  build: {
    target: "es2023",
    chunkSizeWarningLimit: 1500,
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
    // Compiles merge tables for only the classes we use, into src/lib.
    cnTables({
      content: ["src/**/*.{ts,tsx}"],
      out: "src/lib/cn-tables.gen.ts",
    }),
  ],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("src", import.meta.url)),
      // class-variance-authority imports clsx; cn exports a drop-in clsx.
      clsx: "cn",
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
