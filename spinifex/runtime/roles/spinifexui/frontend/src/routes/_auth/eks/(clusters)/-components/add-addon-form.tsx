import type { AddonInfo } from "@aws-sdk/client-eks"
import { zodResolver } from "@hookform/resolvers/zod"
import { Controller, useForm, useWatch } from "react-hook-form"

import { CliCommandPanel } from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { JsonEditor } from "@/components/ui/json-editor"
import { useCreateAddon } from "@/mutations/eks"
import { type CreateAddonFormValues, createAddonSchema } from "@/types/eks"

import { buildAddonCommands } from "./addon-commands"

export function AddAddonForm({
  clusterName,
  catalog,
  installed,
  onClose,
}: {
  clusterName: string
  catalog: AddonInfo[]
  installed: string[]
  onClose: () => void
}) {
  const createAddon = useCreateAddon()
  const installedSet = new Set(installed)
  const available = catalog.filter((a) => !installedSet.has(a.addonName ?? ""))

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<CreateAddonFormValues>({
    resolver: zodResolver(createAddonSchema),
    defaultValues: {
      addonName: "",
      addonVersion: "",
      serviceAccountRoleArn: "",
      configurationValues: "",
    },
  })

  const [addonName, addonVersion, serviceAccountRoleArn, configurationValues] =
    useWatch({
      control,
      name: [
        "addonName",
        "addonVersion",
        "serviceAccountRoleArn",
        "configurationValues",
      ],
    })

  const selectedAddon = catalog.find((a) => a.addonName === addonName)
  const versions = selectedAddon?.addonVersions ?? []
  const selectedVersion = versions.find((v) => v.addonVersion === addonVersion)
  const requiresIam = selectedVersion?.requiresIamPermissions ?? false

  const handleAddonChange = (name: string) => {
    const firstVersion =
      catalog.find((a) => a.addonName === name)?.addonVersions?.[0]
        ?.addonVersion ?? ""

    setValue("addonName", name, { shouldValidate: true })
    setValue("addonVersion", firstVersion, { shouldValidate: true })
  }

  const onSubmit = async (data: CreateAddonFormValues) => {
    try {
      await createAddon.mutateAsync({
        clusterName,
        addonName: data.addonName,
        addonVersion: data.addonVersion || undefined,
        serviceAccountRoleArn: data.serviceAccountRoleArn.trim() || undefined,
        configurationValues: data.configurationValues.trim() || undefined,
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel onClose={onClose} subtitle={clusterName} title="Add add-on">
      <form className="space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <FormLock className="space-y-4" locked={createAddon.isPending}>
          <SelectField
            control={control}
            id="addon-name"
            label="Add-on"
            name="addonName"
            onValueChange={handleAddonChange}
            options={available.map((a) => ({
              value: a.addonName ?? "",
              label: a.addonName ?? "",
            }))}
            placeholder="Select add-on"
          />

          <SelectField
            control={control}
            id="addon-version"
            label="Version"
            name="addonVersion"
            notice={
              versions.length === 0
                ? {
                    tone: "muted",
                    text: "Select an add-on to list its versions.",
                  }
                : undefined
            }
            options={versions.map((v, i) => ({
              value: v.addonVersion ?? "",
              label: `${v.addonVersion ?? ""}${i === 0 ? " (latest)" : ""}`,
            }))}
          />

          <Field>
            <FieldTitle>
              <label htmlFor="addon-role">Service account IAM role ARN</label>
            </FieldTitle>
            <Input
              id="addon-role"
              placeholder="arn:aws:iam::…:role/…"
              {...register("serviceAccountRoleArn")}
            />
            {requiresIam && (
              <p className="text-xs text-muted-foreground">
                This add-on needs IAM permissions — provide an IRSA role ARN.
              </p>
            )}
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="addon-config">Configuration values (JSON)</label>
            </FieldTitle>
            <Controller
              control={control}
              name="configurationValues"
              render={({ field: { onBlur, onChange, ref, value } }) => (
                <JsonEditor
                  error={!!errors.configurationValues}
                  id="addon-config"
                  onBlur={onBlur}
                  onChange={onChange}
                  placeholder='{"replicaCount": 2}'
                  ref={ref}
                  rows={4}
                  value={value}
                />
              )}
            />
            <FieldError errors={[errors.configurationValues]} />
          </Field>
        </FormLock>

        <CliCommandPanel
          commands={buildAddonCommands(
            "create-addon",
            "Add add-on",
            clusterName,
            addonName,
            { addonVersion, serviceAccountRoleArn, configurationValues },
          )}
        />

        <FormActions
          error={createAddon.error}
          errorMsg="Failed to add the add-on"
          isPending={createAddon.isPending}
          isSubmitting={isSubmitting}
          onCancel={onClose}
          pendingLabel="Adding…"
          submitLabel="Add"
        />
      </form>
    </FormPanel>
  )
}
