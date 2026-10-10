import type { QueryClient } from "@tanstack/react-query"
import {
  createRootRouteWithContext,
  HeadContent,
  Outlet,
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
  component: () => (
    <>
      <HeadContent />
      <SidebarLayout />
      <main className="flex flex-1 flex-col">
        <Header />
        <div className="flex flex-1 flex-col p-8">
          <Outlet />
        </div>
      </main>
    </>
  ),
  errorComponent: ErrorBoundary,
  notFoundComponent: NotFound,
})
