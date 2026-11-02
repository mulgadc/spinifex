import { useQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { getRegion } from "@/lib/cluster-config"
import { useCopySnapshot } from "@/mutations/ec2"
import { ec2RegionsQueryOptions } from "@/queries/ec2"

interface CopySnapshotFormProps {
  snapshotId: string
  onClose: () => void
}

function buildCopySnapshotCommands(
  snapshotId: string,
  region: string,
  description: string,
): CliCommand[] {
  return [
    {
      label: "Copy Snapshot",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ec2 copy-snapshot" },
        ...commandFlag("\\\n  --source-snapshot-id", snapshotId),
        ...commandFlag("\\\n  --source-region", region),
        ...optionalFlag(
          "\\\n  --description",
          description && shellSingleQuote(description),
        ),
      ],
    },
  ]
}

// Copies within the current region, the only one a cluster serves, then
// selects the copy in the snapshot list.
export function CopySnapshotForm({
  snapshotId,
  onClose,
}: CopySnapshotFormProps) {
  const { data } = useQuery(ec2RegionsQueryOptions)
  const copy = useCopySnapshot()
  const navigate = useNavigate()
  const [description, setDescription] = useState("")
  const region = data?.Regions?.[0]?.RegionName ?? getRegion()

  async function onSubmit() {
    let result

    try {
      result = await copy.mutateAsync({
        sourceSnapshotId: snapshotId,
        sourceRegion: region,
        description: description || undefined,
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()

    if (result.SnapshotId) {
      void navigate({
        to: "/ec2/describe-snapshots",
        search: { selected: result.SnapshotId },
      })
    }
  }

  return (
    <FormPanel onClose={onClose} subtitle={snapshotId} title="Copy snapshot">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          void onSubmit()
        }}
      >
        <FormLock className="space-y-6" locked={copy.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="copyDescription">Description (optional)</label>
            </FieldTitle>
            <Input
              id="copyDescription"
              onChange={(e) => {
                setDescription(e.target.value)
              }}
              placeholder="Description for the snapshot copy"
              value={description}
            />
            <FieldDescription>The copy is made in {region}.</FieldDescription>
          </Field>

          <CliCommandPanel
            commands={buildCopySnapshotCommands(
              snapshotId,
              region,
              description,
            )}
          />

          <FormActions
            error={copy.error}
            errorMsg="Failed to copy snapshot"
            isPending={copy.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Copying…"
            submitLabel="Copy snapshot"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}
