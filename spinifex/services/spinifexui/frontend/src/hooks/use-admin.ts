import { useQuery } from "@tanstack/react-query"

import { getCredentials } from "@/lib/auth"
import { adminVersionQueryOptions, type VersionInfo } from "@/queries/admin"
import { callerIdentityQueryOptions, isAdminAccount } from "@/queries/sts"

interface AdminState {
  isAdmin: boolean
  accountId: string | null
  userName: string | null
  version: VersionInfo | null
  license: string | null
}

// userNameFromArn pulls the user name out of an IAM user ARN
// (arn:aws:iam::<acct>:user/<name>); returns null for other ARN shapes.
function userNameFromArn(arn: string | undefined): string | null {
  const marker = ":user/"
  const index = arn?.indexOf(marker) ?? -1
  if (index === -1 || arn === undefined) {
    return null
  }
  return arn.slice(index + marker.length) || null
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
    accountId: identity?.Account ?? null,
    userName: userNameFromArn(identity?.Arn),
    version: version ?? null,
    license: version?.license ?? null,
  }
}
