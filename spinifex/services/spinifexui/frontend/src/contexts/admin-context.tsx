import { createContext, useContext, useEffect, useState } from "react"

import { getCredentials } from "@/lib/auth"
import { signedFetch } from "@/lib/signed-fetch"

const ADMIN_ACCOUNT_ID = "000000000001"

interface CallerIdentity {
  account_id: string
  user_name: string
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
        const identity = await signedFetch<CallerIdentity>({
          action: "GetCallerIdentity",
          credentials,
        })

        const isAdmin = identity.account_id === ADMIN_ACCOUNT_ID
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
          accountId: identity.account_id,
          userName: identity.user_name,
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
