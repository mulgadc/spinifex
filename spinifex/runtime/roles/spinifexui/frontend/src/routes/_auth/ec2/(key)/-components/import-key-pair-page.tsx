import { zodResolver } from "@hookform/resolvers/zod"
import {
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
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { useImportKeyPair } from "@/mutations/ec2"
import { type ImportKeyPairData, importKeyPairSchema } from "@/types/ec2"

interface ImportKeyPairPageProps {
  // Called with the imported key pair's ID, or with nothing on cancel.
  onClose: (keyPairId?: string) => void
}

export function ImportKeyPairPage({ onClose }: ImportKeyPairPageProps) {
  const importMutation = useImportKeyPair()

  const {
    handleSubmit,
    register,
    control,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(importKeyPairSchema),
  })

  const values = useWatch({ control })

  const onSubmit = async (data: ImportKeyPairData) => {
    let response

    try {
      response = await importMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(response.KeyPairId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={importMutation.isPending || isSubmitting}
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

        <CliCommandPanel commands={buildImportKeyPairCommands(values)} />

        <FormActions
          error={importMutation.error}
          errorMsg="Failed to import key pair"
          isPending={importMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Importing…"
          submitLabel="Import Key Pair"
        />
      </FormLock>
    </form>
  )
}

function buildImportKeyPairCommands(
  values: DeepPartialSkipArrayKey<ImportKeyPairData>,
): CliCommand[] {
  return [
    {
      label: "Import Key Pair",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 import-key-pair" },
        ...commandFlag(
          "\\\n  --key-name",
          cliPlaceholder(values.keyName ?? "", "KeyName"),
        ),
        ...commandFlag(
          "\\\n  --public-key-material",
          "fileb://path/to/key.pub",
        ),
      ],
    },
  ]
}
