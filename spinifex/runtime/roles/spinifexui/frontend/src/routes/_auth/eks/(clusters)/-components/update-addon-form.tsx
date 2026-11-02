import { useState } from "react"

import { CliCommandPanel } from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { JsonEditor } from "@/components/ui/json-editor"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useUpdateAddon } from "@/mutations/eks"
import { addonConfigurationSchema } from "@/types/eks"

import { buildAddonCommands } from "./addon-commands"

export function UpdateAddonForm({
  clusterName,
  addonName,
  current,
  versions,
  onClose,
}: {
  clusterName: string
  addonName: string
  current: {
    addonVersion?: string
    serviceAccountRoleArn?: string
    configurationValues?: string
  }
  versions: string[]
  onClose: () => void
}) {
  const updateAddon = useUpdateAddon()
  const [version, setVersion] = useState(current.addonVersion ?? "")
  const [role, setRole] = useState(current.serviceAccountRoleArn ?? "")
  const [config, setConfig] = useState(current.configurationValues ?? "")
  const [error, setError] = useState<string | undefined>()

  const handleConfirm = () => {
    const result = addonConfigurationSchema.safeParse(config)

    if (!result.success) {
      setError(
        result.error.issues[0]?.message ?? "Configuration must be valid JSON",
      )

      return
    }

    setError(undefined)
    updateAddon.mutate(
      {
        clusterName,
        addonName,
        addonVersion: version || undefined,
        serviceAccountRoleArn: role.trim() || undefined,
        configurationValues: config.trim() || undefined,
      },
      { onSuccess: onClose },
    )
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={clusterName}
      title={`Update ${addonName}`}
    >
      <form
        className="space-y-6"
        onSubmit={(event) => {
          event.preventDefault()
          handleConfirm()
        }}
      >
        <FormLock className="space-y-4" locked={updateAddon.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="update-addon-version">Version</label>
            </FieldTitle>
            <Select
              disabled={versions.length === 0}
              onValueChange={(next: string | null) => {
                setVersion(next ?? "")
              }}
              value={version}
            >
              <SelectTrigger className="w-full" id="update-addon-version">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {versions.map((v) => (
                  <SelectItem key={v} value={v}>
                    {v}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="update-addon-role">
                Service account IAM role ARN
              </label>
            </FieldTitle>
            <Input
              id="update-addon-role"
              onChange={(e) => {
                setRole(e.target.value)
              }}
              placeholder="arn:aws:iam::…:role/…"
              value={role}
            />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="update-addon-config">
                Configuration values (JSON)
              </label>
            </FieldTitle>
            <JsonEditor
              error={!!error}
              id="update-addon-config"
              onChange={setConfig}
              rows={4}
              value={config}
            />
          </Field>
          {error && <p className="text-xs text-destructive">{error}</p>}
        </FormLock>

        <CliCommandPanel
          commands={buildAddonCommands(
            "update-addon",
            "Update add-on",
            clusterName,
            addonName,
            {
              addonVersion: version,
              serviceAccountRoleArn: role,
              configurationValues: config,
            },
          )}
        />

        <FormActions
          error={updateAddon.error}
          errorMsg="Failed to update the add-on"
          isPending={updateAddon.isPending}
          isSubmitting={false}
          onCancel={onClose}
          pendingLabel="Updating…"
          submitLabel="Update"
        />
      </form>
    </FormPanel>
  )
}
