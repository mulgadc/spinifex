import { KeyAlgorithm, ListCertificatesCommand } from "@aws-sdk/client-acm"
import { queryOptions } from "@tanstack/react-query"

import { getAcmClient } from "@/lib/aws-client"

export const acmCertificatesQueryOptions = queryOptions({
  queryKey: ["acm", "certificates"],
  queryFn: async () => {
    // ListCertificates lists only RSA_1024 and RSA_2048 unless asked for more.
    const command = new ListCertificatesCommand({
      Includes: { keyTypes: Object.values(KeyAlgorithm) },
    })

    return await getAcmClient().send(command)
  },
})
