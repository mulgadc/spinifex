import { useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { useRegisterImage } from "@/mutations/ec2"

interface RegisterImageFormProps {
  snapshotId: string
  onClose: () => void
}

function buildRegisterImageCommands(
  snapshotId: string,
  name: string,
  description: string,
): CliCommand[] {
  return [
    {
      label: "Register Image",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 register-image" },
        ...commandFlag(
          "\\\n  --name",
          name ? shellSingleQuote(name) : cliPlaceholder("", "Name"),
        ),
        ...optionalFlag(
          "\\\n  --description",
          description && shellSingleQuote(description),
        ),
        ...commandFlag(
          "\\\n  --block-device-mappings",
          `'DeviceName=/dev/sda1,Ebs={SnapshotId=${snapshotId}}'`,
        ),
      ],
    },
  ]
}

// Registers an AMI from the snapshot, then selects it in the image list.
export function RegisterImageForm({
  snapshotId,
  onClose,
}: RegisterImageFormProps) {
  const register = useRegisterImage()
  const navigate = useNavigate()
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")

  async function onSubmit() {
    let result

    try {
      result = await register.mutateAsync({
        snapshotId,
        name,
        description: description || undefined,
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()

    if (result.ImageId) {
      void navigate({
        to: "/ec2/describe-images",
        search: { selected: result.ImageId },
      })
    }
  }

  return (
    <FormPanel onClose={onClose} subtitle={snapshotId} title="Register as AMI">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          void onSubmit()
        }}
      >
        <FormLock className="space-y-6" locked={register.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="registerName">Name</label>
            </FieldTitle>
            <Input
              id="registerName"
              onChange={(e) => {
                setName(e.target.value)
              }}
              placeholder="my-image"
              value={name}
            />
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor="registerDescription">
                Description (optional)
              </label>
            </FieldTitle>
            <Textarea
              id="registerDescription"
              onChange={(e) => {
                setDescription(e.target.value)
              }}
              placeholder="Optional description"
              rows={2}
              value={description}
            />
          </Field>

          <CliCommandPanel
            commands={buildRegisterImageCommands(snapshotId, name, description)}
          />

          <FormActions
            error={register.error}
            errorMsg="Failed to register image"
            isPending={register.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Registering…"
            submitDisabled={!name.trim()}
            submitLabel="Register AMI"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}
