import { useState } from "react"

import { ErrorBanner } from "@/components/error-banner"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Field, FieldTitle } from "@/components/ui/field"
import { Textarea } from "@/components/ui/textarea"
import { useImportCertificate } from "@/mutations/acm"

interface CertificateImportDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  // Called with the new certificate ARN once the import succeeds.
  onImported: (certificateArn: string) => void
}

export function CertificateImportDialog({
  open,
  onOpenChange,
  onImported,
}: CertificateImportDialogProps) {
  const [certificate, setCertificate] = useState("")
  const [privateKey, setPrivateKey] = useState("")
  const [certificateChain, setCertificateChain] = useState("")
  const importMutation = useImportCertificate()

  const reset = () => {
    setCertificate("")
    setPrivateKey("")
    setCertificateChain("")
    importMutation.reset()
  }

  const handleOpenChange = (next: boolean) => {
    if (!next) {
      reset()
    }
    onOpenChange(next)
  }

  const canSubmit =
    certificate.trim().length > 0 && privateKey.trim().length > 0

  const handleImport = async () => {
    if (!canSubmit) {
      return
    }
    try {
      const result = await importMutation.mutateAsync({
        certificate,
        privateKey,
        certificateChain,
      })
      if (result.CertificateArn) {
        onImported(result.CertificateArn)
      }
      reset()
      onOpenChange(false)
    } catch {
      // surfaced via importMutation.error
    }
  }

  return (
    <AlertDialog onOpenChange={handleOpenChange} open={open}>
      <AlertDialogContent
        className="grid-cols-[minmax(0,1fr)]"
        style={{ maxWidth: "36rem", width: "calc(100vw - 2rem)" }}
      >
        <AlertDialogHeader>
          <AlertDialogTitle>Import certificate</AlertDialogTitle>
          <AlertDialogDescription>
            Paste a PEM-encoded certificate and its private key. The optional
            chain is appended for intermediate CAs.
          </AlertDialogDescription>
        </AlertDialogHeader>

        {importMutation.error && (
          <ErrorBanner
            error={importMutation.error}
            msg="Failed to import certificate"
          />
        )}

        <div className="min-w-0 space-y-4">
          <Field>
            <FieldTitle>
              <label htmlFor="cert-body">Certificate body</label>
            </FieldTitle>
            <Textarea
              className="font-mono text-xs"
              id="cert-body"
              onChange={(e) => setCertificate(e.target.value)}
              placeholder="-----BEGIN CERTIFICATE-----"
              rows={5}
              value={certificate}
            />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="cert-key">Private key</label>
            </FieldTitle>
            <Textarea
              className="font-mono text-xs"
              id="cert-key"
              onChange={(e) => setPrivateKey(e.target.value)}
              placeholder="-----BEGIN PRIVATE KEY-----"
              rows={5}
              value={privateKey}
            />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="cert-chain">Certificate chain (optional)</label>
            </FieldTitle>
            <Textarea
              className="font-mono text-xs"
              id="cert-chain"
              onChange={(e) => setCertificateChain(e.target.value)}
              placeholder="-----BEGIN CERTIFICATE-----"
              rows={4}
              value={certificateChain}
            />
          </Field>
        </div>

        <AlertDialogFooter>
          <AlertDialogCancel disabled={importMutation.isPending}>
            Cancel
          </AlertDialogCancel>
          <AlertDialogAction
            disabled={!canSubmit || importMutation.isPending}
            onClick={handleImport}
          >
            {importMutation.isPending ? "Importing…" : "Import"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
