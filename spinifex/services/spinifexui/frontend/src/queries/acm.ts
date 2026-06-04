import { ListCertificatesCommand } from "@aws-sdk/client-acm"
import { queryOptions } from "@tanstack/react-query"

import { getAcmClient } from "@/lib/awsClient"

export const acmCertificatesQueryOptions = queryOptions({
  queryKey: ["acm", "certificates"],
  queryFn: async () => {
    const command = new ListCertificatesCommand({})
    return await getAcmClient().send(command)
  },
})
