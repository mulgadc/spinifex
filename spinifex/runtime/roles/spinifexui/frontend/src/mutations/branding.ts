import { useMutation, useQueryClient } from "@tanstack/react-query"

import { signedAdminFetch } from "@/lib/signed-fetch"
import { consoleBrandingQueryOptions } from "@/queries/branding"
import type {
  ConsoleBrandingResponse,
  PutConsoleBrandingRequest,
} from "@/types/branding"

export function usePutConsoleBranding() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async (body: PutConsoleBrandingRequest) =>
      await signedAdminFetch<
        ConsoleBrandingResponse,
        PutConsoleBrandingRequest
      >({ method: "PutConsoleBranding", body }),
    onSuccess: (saved) => {
      queryClient.setQueryData(consoleBrandingQueryOptions.queryKey, saved)
    },
  })
}
