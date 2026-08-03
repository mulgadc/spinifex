import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
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

export function CreateKeyPair() {
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
    // Matches what this page already sent before the selector existed, rather
    // than the API's ed25519 default.
    defaultValues: { keyType: "rsa" as const },
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

        {/* Key Type */}
        <Field>
          <FieldTitle>
            <label htmlFor="keyType">Key Type</label>
          </FieldTitle>
          <Controller
            control={control}
            name="keyType"
            render={({ field }) => (
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value}
              >
                <SelectTrigger
                  aria-invalid={!!errors.keyType}
                  className="w-full"
                  id="keyType"
                >
                  <SelectValue placeholder="Select a key type" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="rsa">RSA (2048-bit)</SelectItem>
                  <SelectItem value="ed25519">ED25519</SelectItem>
                </SelectContent>
              </Select>
            )}
          />
          <FieldDescription>
            RSA is required to retrieve a Windows administrator password.
            ED25519 works for SSH only.
          </FieldDescription>
          <FieldError errors={[errors.keyType]} />
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
  const rawKeyType = watch("keyType")
  const keyType = typeof rawKeyType === "string" ? rawKeyType : "rsa"

  return [
    {
      label: "Create Key Pair",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-key-pair" },
        { type: "flag", value: " --key-name" },
        { type: "value", value: ` ${keyName || "<KeyName>"}` },
        { type: "flag", value: " --key-type" },
        { type: "value", value: ` ${keyType}` },
      ],
    },
  ]
}
