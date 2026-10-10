import { ImportCertificateCommand } from "@aws-sdk/client-acm"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getAcmClient } from "@/lib/awsClient"

const textEncoder = new TextEncoder()

export interface ImportCertificateParams {
  certificate: string
  privateKey: string
  certificateChain?: string
}

// useImportCertificate imports a PEM certificate (leaf + private key, optional
// chain) into the ACM-alike store and returns the new certificate ARN. The
// certificates query is invalidated so the listener form's selector picks it up.
export function useImportCertificate() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ImportCertificateParams) => {
      const command = new ImportCertificateCommand({
        Certificate: textEncoder.encode(params.certificate),
        PrivateKey: textEncoder.encode(params.privateKey),
        CertificateChain:
          params.certificateChain && params.certificateChain.length > 0
            ? textEncoder.encode(params.certificateChain)
            : undefined,
      })
      return await getAcmClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["acm", "certificates"] })
    },
  })
}
