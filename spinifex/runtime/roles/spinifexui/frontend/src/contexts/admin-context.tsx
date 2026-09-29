import { GetCallerIdentityCommand } from "@aws-sdk/client-sts"
import { createContext, useContext, useEffect, useState } from "react"

import { getCredentials } from "@/lib/auth"
import { signedFetch } from "@/lib/signed-fetch"
import { getStsClient } from "@/lib/sts"

const ADMIN_ACCOUNT_ID = "000000000001"

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

interface VersionInfo {
  version: string
  commit: string
  os: string
  arch: string
  license: string
}

interface AdminState {
  isAdmin: boolean
  accountId: string | null
  userName: string | null
  version: VersionInfo | null
  license: string | null
  loading: boolean
}

const AdminContext = createContext<AdminState | undefined>(undefined)

export function AdminProvider({ children }: { children: React.ReactNode }) {
  const [state, setState] = useState<AdminState>({
    isAdmin: false,
    accountId: null,
    userName: null,
    version: null,
    license: null,
    loading: true,
  })

  useEffect(() => {
    async function detect() {
      const credentials = getCredentials()
      if (!credentials) {
        setState((prev) => ({ ...prev, loading: false }))
        return
      }

      try {
        const identity = await getStsClient().send(
          new GetCallerIdentityCommand({}),
        )

        const accountId = identity.Account ?? null
        const isAdmin = accountId === ADMIN_ACCOUNT_ID
        let version: VersionInfo | null = null

        if (isAdmin) {
          try {
            version = await signedFetch<VersionInfo>({
              action: "GetVersion",
              credentials,
            })
          } catch {
            // Version fetch is best-effort
          }
        }

        setState({
          isAdmin,
          accountId,
          userName: userNameFromArn(identity.Arn),
          version,
          license: version?.license ?? null,
          loading: false,
        })
      } catch {
        setState((prev) => ({ ...prev, loading: false }))
      }
    }

    void detect()
  }, [])

  return <AdminContext value={state}>{children}</AdminContext>
}

export function useAdmin(): AdminState {
  const context = useContext(AdminContext)
  if (context === undefined) {
    throw new Error("useAdmin must be used within an AdminProvider")
  }
  return context
}
