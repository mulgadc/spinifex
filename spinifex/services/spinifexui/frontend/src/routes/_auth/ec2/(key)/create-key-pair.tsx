import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { useForm, useWatch } from "react-hook-form"

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
import { useCreateKeyPair } from "@/mutations/ec2"
import { type CreateKeyPairData, createKeyPairSchema } from "@/types/ec2"

import { PrivateKeyModal } from "../-components/private-key-modal"

export const Route = createFileRoute("/_auth/ec2/(key)/create-key-pair")({
  head: () => ({
    meta: [
      {
        title: "Create Key Pair | EC2 | Mulga",
      },
    ],
  }),
  component: CreateKeyPair,
})

function CreateKeyPair() {
  const navigate = useNavigate()
  const createMutation = useCreateKeyPair()
  const [keyMaterial, setKeyMaterial] = useState<{
    keyName: string
    material: string
  } | null>(null)

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createKeyPairSchema),
  })

  const values = useWatch({ control })
  const cliWatch = (name?: string): unknown =>
    name ? (values as Record<string, unknown>)[name] : undefined

  const onSubmit = async (data: CreateKeyPairData) => {
    const response = await createMutation.mutateAsync(data)

    if (response.KeyMaterial) {
      setKeyMaterial({
        keyName: data.keyName,
        material: response.KeyMaterial,
      })
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-key-pairs">Back to key pairs</BackLink>
      <PageHeading title="Create Key Pair" />

      {/* Handle error after submission */}
      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create key pair"
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

        <CliCommandPanel commands={buildCreateKeyPairCommands(cliWatch)} />

        {/* Actions */}
        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-key-pairs" })
          }
          pendingLabel="Creating…"
          submitLabel="Create Key Pair"
        />
      </form>

      {/* Private Key Modal */}
      {keyMaterial && (
        <PrivateKeyModal
          keyMaterial={keyMaterial.material}
          keyName={keyMaterial.keyName}
          open={!!keyMaterial}
        />
      )}
    </>
  )
}

function buildCreateKeyPairCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawKeyName = watch("keyName")
  const keyName = typeof rawKeyName === "string" ? rawKeyName : ""

  return [
    {
      label: "Create Key Pair",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-key-pair" },
        { type: "flag", value: " --key-name" },
        { type: "value", value: ` ${keyName || "<KeyName>"}` },
      ],
    },
  ]
}
