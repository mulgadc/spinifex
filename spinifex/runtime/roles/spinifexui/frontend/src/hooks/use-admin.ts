import { useQuery } from "@tanstack/react-query"

import { getCredentials } from "@/lib/auth"
import { adminVersionQueryOptions, type VersionInfo } from "@/queries/admin"
import { callerIdentityQueryOptions, isAdminAccount } from "@/queries/sts"

interface AdminState {
  isAdmin: boolean
  version: VersionInfo | null
  license: string | null
}

// isAdmin is false until the identity resolves, so callers that only hide
// admin extras need no loading state. Admin routes guard with requireAdmin.
export function useAdmin(): AdminState {
  const { data: identity } = useQuery({
    ...callerIdentityQueryOptions,
    enabled: getCredentials() !== null,
  })

  const isAdmin = isAdminAccount(identity?.Account)

  const { data: version } = useQuery({
    ...adminVersionQueryOptions,
    enabled: isAdmin,
  })

  return {
    isAdmin,
    version: version ?? null,
    license: version?.license ?? null,
  }
}
