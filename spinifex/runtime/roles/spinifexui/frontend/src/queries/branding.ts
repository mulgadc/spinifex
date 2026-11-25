import { queryOptions } from "@tanstack/react-query"

import { signedAdminFetch } from "@/lib/signed-fetch"
import type { ConsoleBrandingResponse } from "@/types/branding"

export const consoleBrandingQueryOptions = queryOptions({
  queryKey: ["admin", "consoleBranding"],
  queryFn: async () =>
    await signedAdminFetch<ConsoleBrandingResponse>({
      method: "GetConsoleBranding",
      body: {},
    }),
  retry: false,
})
