import type { QueryClient } from "@tanstack/react-query"
import { redirect } from "@tanstack/react-router"

import { callerIdentityQueryOptions, isAdminAccount } from "@/queries/sts"

// beforeLoad guard for admin-only routes. A failed identity lookup propagates
// to the error component rather than being reported as "not an admin".
export async function requireAdmin({
  context,
}: {
  context: { queryClient: QueryClient }
}): Promise<void> {
  const identity = await context.queryClient.query(callerIdentityQueryOptions)
  if (!isAdminAccount(identity.Account)) {
    throw redirect({ to: "/" })
  }
}
