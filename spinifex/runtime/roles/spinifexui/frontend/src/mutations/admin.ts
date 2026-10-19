import { useMutation, useQueryClient } from "@tanstack/react-query"

import { signedFetch } from "@/lib/signed-fetch"

interface PromoteImageOutput {
  PreviousOwner: string
}

export function usePromoteImage() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async (imageId: string) =>
      await signedFetch<PromoteImageOutput>({
        action: "PromoteImage",
        params: { ImageId: imageId },
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "images"] })
    },
  })
}
