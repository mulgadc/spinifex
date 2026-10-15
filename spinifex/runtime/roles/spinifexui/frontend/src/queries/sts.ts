import { GetCallerIdentityCommand } from "@aws-sdk/client-sts"
import { queryOptions } from "@tanstack/react-query"

import { getStsClient } from "@/lib/sts"

const ADMIN_ACCOUNT_ID = "000000000001"

export function isAdminAccount(accountId: string | undefined): boolean {
  return accountId === ADMIN_ACCOUNT_ID
}

// The caller's identity cannot change within a session, and sign-out clears
// the query cache, so it is fetched once per session.
export const callerIdentityQueryOptions = queryOptions({
  queryKey: ["sts", "callerIdentity"],
  queryFn: async () =>
    await getStsClient().send(new GetCallerIdentityCommand({})),
  staleTime: "static",
})
