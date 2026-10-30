import type { QueryClient } from "@tanstack/react-query"
import {
  createRootRouteWithContext,
  HeadContent,
  Outlet,
  useMatch,
} from "@tanstack/react-router"

import { Header } from "@/components/header"
import { NotFound } from "@/components/not-found"
import { ErrorBoundary } from "@/layouts/error-boundary"
import { SidebarLayout } from "@/layouts/sidebar-layout"

interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  head: () => ({
    meta: [
      {
        title: "Mulga",
      },
    ],
  }),
  component: RootLayout,
  errorComponent: ErrorBoundary,
  notFoundComponent: NotFound,
})

function RootLayout() {
  const signingIn =
    useMatch({ from: "/login", shouldThrow: false }) !== undefined

  return (
    <>
      <HeadContent />
      {/* Signed out, the shell stays in view behind the sign-in card but
          cannot be reached by pointer, keyboard or screen reader. */}
      <div className="contents" inert={signingIn}>
        <SidebarLayout />
        <main className="flex min-w-0 flex-1 flex-col">
          <Header />
          {!signingIn && (
            <div className="flex flex-1 flex-col p-4 transition-opacity duration-300 motion-reduce:transition-none md:p-6 xl:p-8 starting:opacity-0">
              <Outlet />
            </div>
          )}
        </main>
      </div>
      {signingIn && (
        <div className="fixed inset-0 z-50 flex overflow-y-auto p-4">
          <Outlet />
        </div>
      )}
    </>
  )
}
