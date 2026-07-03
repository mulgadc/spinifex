import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getCredentials } from "@/lib/auth"
import { signedFetch } from "@/lib/signed-fetch"

interface PromoteImageOutput {
  PreviousOwner: string
}

export function usePromoteImage() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (imageId: string) => {
      const credentials = getCredentials()
      if (!credentials) {
        throw new Error("Not authenticated")
      }
      return await signedFetch<PromoteImageOutput>({
        action: "PromoteImage",
        credentials,
        params: { ImageId: imageId },
      })
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "images"] })
    },
  })
}
