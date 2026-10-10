import { QueryClientProvider } from "@tanstack/react-query"
import { createRouter, RouterProvider } from "@tanstack/react-router"
import { StrictMode } from "react"
import ReactDOM from "react-dom/client"

import { ThemeProvider } from "@/components/theme-provider"
import { SidebarProvider } from "@/components/ui/sidebar"
import { AdminProvider } from "@/contexts/admin-context"
import { loadClusterConfig } from "@/lib/cluster-config"
import { createQueryClient } from "@/lib/query-client"

import { routeTree } from "./routeTree.gen"

import "./styles.css"

const queryClient = createQueryClient()

const router = createRouter({
  routeTree,
  context: {
    queryClient,
  },
  defaultPreload: "intent",
  scrollRestoration: true,
  defaultStructuralSharing: true,
  defaultPreloadStaleTime: 0,
})

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router
  }
}

// The region has to be known before anything signs a request, so the config is
// loaded first and a failure is shown instead of the app.
async function bootstrap(root: ReactDOM.Root) {
  try {
    await loadClusterConfig()
  } catch (error: unknown) {
    root.render(
      <div
        style={{
          fontFamily: "sans-serif",
          margin: "4rem auto",
          maxWidth: "40rem",
        }}
      >
        <h1>Console unavailable</h1>
        <p>
          This cluster did not report its region, so the console cannot sign
          requests. Check that the serving node has a region set in its
          configuration.
        </p>
        <pre>{error instanceof Error ? error.message : String(error)}</pre>
      </div>,
    )
    return
  }

  root.render(
    <StrictMode>
      <ThemeProvider defaultTheme="dark" storageKey="spinifex-ui-theme">
        <QueryClientProvider client={queryClient}>
          <AdminProvider>
            <SidebarProvider>
              <RouterProvider router={router} />
            </SidebarProvider>
          </AdminProvider>
        </QueryClientProvider>
      </ThemeProvider>
    </StrictMode>,
  )
}

const rootElement = document.querySelector("#app")
if (rootElement && !rootElement.innerHTML) {
  await bootstrap(ReactDOM.createRoot(rootElement))
}
