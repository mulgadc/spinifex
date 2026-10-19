import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, renderHook, type RenderOptions } from "@testing-library/react"
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

function clientWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    )
  }
}

export function renderWithClient(
  ui: ReactElement,
  queryClient: QueryClient,
  options?: Omit<RenderOptions, "wrapper">,
) {
  return render(ui, { wrapper: clientWrapper(queryClient), ...options })
}

// Pass a client to seed or spy on it before the hook mounts.
export function renderMutationHook<TResult>(
  hook: () => TResult,
  queryClient: QueryClient = createTestQueryClient(),
) {
  return {
    queryClient,
    ...renderHook(hook, { wrapper: clientWrapper(queryClient) }),
  }
}
