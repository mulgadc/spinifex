import { zodResolver } from "@hookform/resolvers/zod"
import { useState } from "react"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
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

import { PrivateKeyModal } from "../../-components/private-key-modal"

interface CreateKeyPairPageProps {
  // Called with the new key pair's ID once its private key is saved, or with
  // nothing on cancel.
  onClose: (keyPairId?: string) => void
}

export function CreateKeyPairPage({ onClose }: CreateKeyPairPageProps) {
  const createMutation = useCreateKeyPair()

  const [keyMaterial, setKeyMaterial] = useState<{
    keyName: string
    keyPairId?: string
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

  const onSubmit = async (data: CreateKeyPairData) => {
    let response

    try {
      response = await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    if (response.KeyMaterial) {
      setKeyMaterial({
        keyName: data.keyName,
        keyPairId: response.KeyPairId,
        material: response.KeyMaterial,
      })
    }
  }

  return (
    <>
      <form onSubmit={handleSubmit(onSubmit)}>
        <FormLock
          className="space-y-6"
          locked={createMutation.isPending || isSubmitting}
        >
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

          <Field>
            <FieldTitle>
              <label htmlFor="keyType">Key Type</label>
            </FieldTitle>
            <Controller
              control={control}
              name="keyType"
              render={({ field }) => (
                <Select onValueChange={field.onChange} value={field.value}>
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

          <CliCommandPanel commands={buildCreateKeyPairCommands(values)} />

          <FormActions
            error={createMutation.error}
            errorMsg="Failed to create key pair"
            isPending={createMutation.isPending}
            isSubmitting={isSubmitting}
            onCancel={() => {
              onClose()
            }}
            pendingLabel="Creating…"
            submitLabel="Create Key Pair"
          />
        </FormLock>
      </form>

      {keyMaterial && (
        <PrivateKeyModal
          keyMaterial={keyMaterial.material}
          keyName={keyMaterial.keyName}
          onClose={() => {
            onClose(keyMaterial.keyPairId)
          }}
          open={!!keyMaterial}
        />
      )}
    </>
  )
}

function buildCreateKeyPairCommands(
  values: DeepPartialSkipArrayKey<CreateKeyPairData>,
): CliCommand[] {
  return [
    {
      label: "Create Key Pair",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-key-pair" },
        ...commandFlag(
          "--key-name",
          cliPlaceholder(values.keyName ?? "", "KeyName"),
        ),
        ...commandFlag("--key-type", values.keyType ?? "rsa"),
      ],
    },
  ]
}
