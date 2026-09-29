// Shared helpers for ELBv2 route-level integration tests.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, type RenderOptions } from "@testing-library/react"
import type { ReactElement, ReactNode } from "react"

export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: Number.POSITIVE_INFINITY,
        refetchOnMount: false,
      },
      mutations: { retry: false },
    },
  })
}

export function renderWithClient(
  ui: ReactElement,
  queryClient: QueryClient,
  options?: Omit<RenderOptions, "wrapper">,
) {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    )
  }
  return render(ui, { wrapper: Wrapper, ...options })
}
