import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { useImportKeyPair } from "@/mutations/ec2"
import { type ImportKeyPairData, importKeyPairSchema } from "@/types/ec2"

export const Route = createFileRoute("/_auth/ec2/(key)/import-key-pair")({
  head: () => ({
    meta: [
      {
        title: "Import Key Pair | EC2 | Mulga",
      },
    ],
  }),
  component: ImportKeyPair,
})

function ImportKeyPair() {
  const navigate = useNavigate()
  const importMutation = useImportKeyPair()

  const {
    handleSubmit,
    register,
    watch,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(importKeyPairSchema),
  })

  const onSubmit = async (data: ImportKeyPairData) => {
    await importMutation.mutateAsync(data)
    navigate({ to: "/ec2/describe-key-pairs" })
  }

  return (
    <>
      <BackLink to="/ec2/describe-key-pairs">Back to key pairs</BackLink>
      <PageHeading title="Import Key Pair" />

      {/* Handle error after submission */}
      {importMutation.error && (
        <ErrorBanner
          error={importMutation.error}
          msg="Failed to import key pair"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        {/* Key Name */}
        <Field>
          <FieldTitle>
            <label htmlFor="keyName">Key Pair Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.keyName}
            id="keyName"
            placeholder="my-key-pair…"
            {...register("keyName")}
          />
          <FieldError errors={[errors.keyName]} />
        </Field>

        {/* Public Key Material */}
        <Field>
          <FieldTitle>
            <label htmlFor="publicKeyMaterial">Public Key</label>
          </FieldTitle>
          <p
            className="text-xs text-muted-foreground"
            id="publicKey-description"
          >
            Paste your OpenSSH public key (e.g. the contents of id_rsa.pub)
          </p>
          <Textarea
            aria-invalid={!!errors.publicKeyMaterial}
            id="publicKeyMaterial"
            placeholder="ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC…"
            rows={8}
            {...register("publicKeyMaterial")}
          />
          <FieldError errors={[errors.publicKeyMaterial]} />
        </Field>

        <CliCommandPanel commands={buildImportKeyPairCommands(watch)} />

        {/* Actions */}
        <FormActions
          isPending={importMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-key-pairs" })
          }
          pendingLabel="Importing…"
          submitLabel="Import Key Pair"
        />
      </form>
    </>
  )
}

function buildImportKeyPairCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawKeyName = watch("keyName")
  const keyName = typeof rawKeyName === "string" ? rawKeyName : ""

  return [
    {
      label: "Import Key Pair",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 import-key-pair" },
        { type: "flag", value: " \\\n  --key-name" },
        { type: "value", value: ` ${keyName || "<KeyName>"}` },
        { type: "flag", value: " \\\n  --public-key-material" },
        { type: "value", value: " fileb://path/to/key.pub" },
      ],
    },
  ]
}
