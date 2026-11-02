import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useScaleNodegroup } from "@/mutations/eks"

function buildScaleCommands(
  clusterName: string,
  nodegroupName: string,
  minSize: number,
  desiredSize: number,
  maxSize: number,
): CliCommand[] {
  return [
    {
      label: "Scale node group",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws eks update-nodegroup-config",
        },
        ...commandFlag("\\\n  --cluster-name", clusterName),
        ...commandFlag("\\\n  --nodegroup-name", nodegroupName),
        ...commandFlag(
          "\\\n  --scaling-config",
          `minSize=${minSize},maxSize=${maxSize},desiredSize=${desiredSize}`,
        ),
      ],
    },
  ]
}

export function ScaleNodegroupForm({
  clusterName,
  nodegroupName,
  current,
  onClose,
}: {
  clusterName: string
  nodegroupName: string
  current: { minSize?: number; desiredSize?: number; maxSize?: number }
  onClose: () => void
}) {
  const scaleNodegroup = useScaleNodegroup()
  const [minSize, setMinSize] = useState(current.minSize ?? 0)
  const [desiredSize, setDesiredSize] = useState(current.desiredSize ?? 0)
  const [maxSize, setMaxSize] = useState(current.maxSize ?? 0)
  const [error, setError] = useState<string | undefined>()

  const handleConfirm = () => {
    if (!(minSize <= desiredSize && desiredSize <= maxSize)) {
      setError("Sizes must satisfy min ≤ desired ≤ max")

      return
    }

    setError(undefined)
    scaleNodegroup.mutate(
      { clusterName, nodegroupName, minSize, desiredSize, maxSize },
      { onSuccess: onClose },
    )
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={clusterName}
      title={`Scale ${nodegroupName}`}
    >
      <form
        className="space-y-6"
        onSubmit={(event) => {
          event.preventDefault()
          handleConfirm()
        }}
      >
        <FormLock className="space-y-2" locked={scaleNodegroup.isPending}>
          <div className="grid grid-cols-3 gap-3">
            <Field>
              <FieldTitle>
                <label htmlFor="scale-min">Min</label>
              </FieldTitle>
              <Input
                id="scale-min"
                onChange={(e) => {
                  setMinSize(Number(e.target.value))
                }}
                type="number"
                value={minSize}
              />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="scale-desired">Desired</label>
              </FieldTitle>
              <Input
                id="scale-desired"
                onChange={(e) => {
                  setDesiredSize(Number(e.target.value))
                }}
                type="number"
                value={desiredSize}
              />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="scale-max">Max</label>
              </FieldTitle>
              <Input
                id="scale-max"
                onChange={(e) => {
                  setMaxSize(Number(e.target.value))
                }}
                type="number"
                value={maxSize}
              />
            </Field>
          </div>
          {error && <p className="text-xs text-destructive">{error}</p>}
        </FormLock>

        <CliCommandPanel
          commands={buildScaleCommands(
            clusterName,
            nodegroupName,
            minSize,
            desiredSize,
            maxSize,
          )}
        />

        <FormActions
          error={scaleNodegroup.error}
          errorMsg="Failed to scale the node group"
          isPending={scaleNodegroup.isPending}
          isSubmitting={false}
          onCancel={onClose}
          pendingLabel="Scaling…"
          submitLabel="Scale"
        />
      </form>
    </FormPanel>
  )
}
