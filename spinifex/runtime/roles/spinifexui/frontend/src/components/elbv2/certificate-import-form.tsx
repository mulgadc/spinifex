import { type SubmitEvent, useState } from "react"

import {
  type CliCommand,
  CliCommandPanel,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { Textarea } from "@/components/ui/textarea"
import { useImportCertificate } from "@/mutations/acm"

interface CertificateImportFormProps {
  onClose: () => void
  // Called with the new certificate ARN once the import succeeds.
  onImported: (certificateArn: string) => void
}

export function CertificateImportForm({
  onClose,
  onImported,
}: CertificateImportFormProps) {
  const [certificate, setCertificate] = useState("")
  const [privateKey, setPrivateKey] = useState("")
  const [certificateChain, setCertificateChain] = useState("")
  const importMutation = useImportCertificate()

  const canSubmit =
    certificate.trim().length > 0 && privateKey.trim().length > 0

  // The form lies over another form, so its submit must not reach that one.
  async function handleSubmit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault()
    event.stopPropagation()

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

      onClose()
    } catch {
      // surfaced via importMutation.error
    }
  }

  return (
    <FormPanel onClose={onClose} title="Import certificate">
      <form onSubmit={handleSubmit}>
        <FormLock className="space-y-4" locked={importMutation.isPending}>
          <FieldDescription>
            Paste a PEM-encoded certificate and its private key. The optional
            chain is appended for intermediate CAs.
          </FieldDescription>

          <Field>
            <FieldTitle>
              <label htmlFor="cert-body">Certificate body</label>
            </FieldTitle>
            <Textarea
              size="sm"
              variant="code"
              id="cert-body"
              onChange={(e) => {
                setCertificate(e.target.value)
              }}
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
              size="sm"
              variant="code"
              id="cert-key"
              onChange={(e) => {
                setPrivateKey(e.target.value)
              }}
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
              size="sm"
              variant="code"
              id="cert-chain"
              onChange={(e) => {
                setCertificateChain(e.target.value)
              }}
              placeholder="-----BEGIN CERTIFICATE-----"
              rows={4}
              value={certificateChain}
            />
          </Field>

          <CliCommandPanel
            commands={buildImportCertificateCommands(
              certificateChain.trim().length > 0,
            )}
          />

          <FormActions
            error={importMutation.error}
            errorMsg="Failed to import certificate"
            isPending={importMutation.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Importing…"
            submitDisabled={!canSubmit}
            submitLabel="Import"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

// The PEM blocks are pasted into the form; the CLI reads the same from files.
export function buildImportCertificateCommands(
  withChain: boolean,
): CliCommand[] {
  return [
    {
      label: "Import certificate",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws acm import-certificate",
        },
        ...commandFlag("--certificate", "fileb://certificate.pem"),
        ...commandFlag("--private-key", "fileb://private-key.pem"),
        ...optionalFlag(
          "--certificate-chain",
          withChain ? "fileb://certificate-chain.pem" : undefined,
        ),
      ],
    },
  ]
}
