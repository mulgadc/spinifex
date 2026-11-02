/// <reference types="vitest/config" />
import { request } from "node:https"
import { fileURLToPath, URL } from "node:url"

import tailwindcss from "@tailwindcss/vite"
import { tanstackRouter } from "@tanstack/router-plugin/vite"
import basicSsl from "@vitejs/plugin-basic-ssl"
import react from "@vitejs/plugin-react"
import { cn as cnTables } from "cn/vite"
import { createLogger, defineConfig, type Plugin } from "vite"

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

const hopByHopHeaders = new Set([
  "connection",
  "keep-alive",
  "proxy-connection",
  "transfer-encoding",
  "upgrade",
])

// /api and /proxy (the same-origin AWS gateway and S3 routes) are served by the
// Go console, not Vite. Forward them to the local console in dev; a middleware,
// as server.proxy would drop Vite to HTTP/1.1 and serialise module loads.
const consoleApi = (target = "https://localhost:3000"): Plugin => ({
  name: "spinifex-console-api",
  apply: "serve",
  configureServer(server) {
    server.middlewares.use((req, res, next) => {
      if (!/^\/(?:api|proxy)\//.test(req.url ?? "")) {
        next()

        return
      }

      const upstream = request(
        new URL(req.url ?? "/", target),
        {
          method: req.method,
          // HTTP/2 pseudo-headers (:method, :path) are not valid HTTP/1.1.
          headers: Object.fromEntries(
            Object.entries(req.headers).filter(
              ([name]) => !name.startsWith(":"),
            ),
          ),
          rejectUnauthorized: false,
        },
        (response) => {
          // Connection-specific headers are forbidden on HTTP/2 responses.
          const headers = Object.fromEntries(
            Object.entries(response.headers).filter(
              ([name]) => !hopByHopHeaders.has(name),
            ),
          )

          res.writeHead(response.statusCode ?? 502, headers)
          response.pipe(res)
        },
      )

      upstream.on("error", (error) => {
        res.statusCode = 502
        res.end(`console API unreachable at ${target}: ${error.message}`)
      })
      req.pipe(upstream)
    })
  },
})

export default defineConfig(({ mode }) => ({
  customLogger: logger,
  envDir: "../",
  build: {
    target: "es2023",
    chunkSizeWarningLimit: 1500,
  },
  plugins: [
    basicSsl(),
    consoleApi(),
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
