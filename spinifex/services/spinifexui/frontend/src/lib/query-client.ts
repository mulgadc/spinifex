import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query"

import { clearCredentials } from "./auth"
import { isStaleCredentialsError } from "./auth-error"
import { clearClients } from "./awsClient"

// Guard so the flood of concurrent failures from stale creds triggers a single
// recovery rather than racing redirects.
let recovering = false

// On the first request that fails because the stored credentials no longer
// resolve, wipe them and bounce to the login page with an explanatory banner.
// Authorization denials (AccessDenied / UnauthorizedOperation) and other errors
// fall through to the normal error UI.
function recoverFromStaleCredentials(
  error: unknown,
  queryClient: QueryClient,
): void {
  if (recovering || !isStaleCredentialsError(error)) {
    return
  }
  recovering = true
  clearCredentials()
  clearClients()
  queryClient.clear()
  if (window.location.pathname !== "/login") {
    window.location.href = "/login?reason=expired"
  }
}

export function createQueryClient(): QueryClient {
  const queryClient = new QueryClient({
    queryCache: new QueryCache({
      onError: (error) => recoverFromStaleCredentials(error, queryClient),
    }),
    mutationCache: new MutationCache({
      onError: (error) => recoverFromStaleCredentials(error, queryClient),
    }),
    defaultOptions: {
      queries: {
        staleTime: 5000,
        refetchIntervalInBackground: false,
      },
    },
  })
  return queryClient
}
