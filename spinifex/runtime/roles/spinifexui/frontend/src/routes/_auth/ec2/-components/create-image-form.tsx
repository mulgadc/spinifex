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
import { useCreateImage } from "@/mutations/ec2"

interface CreateImageFormProps {
  instanceId: string
  onClose: () => void
}

function buildCreateImageCommands(
  instanceId: string,
  name: string,
  description: string,
): CliCommand[] {
  return [
    {
      label: "Create Image",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 create-image" },
        ...commandFlag("\\\n  --instance-id", instanceId),
        ...commandFlag(
          "\\\n  --name",
          name ? shellSingleQuote(name) : cliPlaceholder("", "Name"),
        ),
        ...optionalFlag(
          "\\\n  --description",
          description && shellSingleQuote(description),
        ),
      ],
    },
  ]
}

// Creates an AMI from the instance, then selects it in the image list.
export function CreateImageForm({ instanceId, onClose }: CreateImageFormProps) {
  const createImage = useCreateImage()
  const navigate = useNavigate()
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")

  async function onSubmit() {
    let result

    try {
      result = await createImage.mutateAsync({
        instanceId,
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
    <FormPanel
      onClose={onClose}
      subtitle={instanceId}
      title="Create image (AMI)"
    >
      <form
        onSubmit={(event) => {
          event.preventDefault()
          void onSubmit()
        }}
      >
        <FormLock className="space-y-6" locked={createImage.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="imageName">Name</label>
            </FieldTitle>
            <Input
              id="imageName"
              onChange={(e) => {
                setName(e.target.value)
              }}
              placeholder="my-image"
              value={name}
            />
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor="imageDescription">Description</label>
            </FieldTitle>
            <Textarea
              id="imageDescription"
              onChange={(e) => {
                setDescription(e.target.value)
              }}
              placeholder="Optional description"
              rows={2}
              value={description}
            />
          </Field>

          <CliCommandPanel
            commands={buildCreateImageCommands(instanceId, name, description)}
          />

          <FormActions
            error={createImage.error}
            errorMsg="Failed to create image"
            isPending={createImage.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Creating…"
            submitDisabled={!name.trim()}
            submitLabel="Create image"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}
