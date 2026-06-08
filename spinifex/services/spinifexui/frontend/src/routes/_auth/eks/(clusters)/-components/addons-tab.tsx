import type { AddonInfo } from "@aws-sdk/client-eks"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { ArrowUpCircle, Trash2 } from "lucide-react"
import { useEffect, useState } from "react"
import { useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { useCreateAddon, useDeleteAddon, useUpdateAddon } from "@/mutations/eks"
import {
  eksAddonQueryOptions,
  eksAddonsQueryOptions,
  eksAddonVersionsQueryOptions,
} from "@/queries/eks"
import {
  addonStatusVariant,
  type CreateAddonFormValues,
  createAddonSchema,
} from "@/types/eks"

const IN_PROGRESS_STATUSES = new Set(["CREATING", "UPDATING", "DELETING"])
const STATUS_POLL_MS = 5000

const selectClassName =
  "h-7 w-full rounded-md border border-input bg-input/20 px-2 text-sm"

function isValidJson(value: string): boolean {
  if (value.trim() === "") {
    return true
  }
  try {
    JSON.parse(value)
    return true
  } catch {
    return false
  }
}

function AddAddonDialog({
  clusterName,
  catalog,
  installed,
  open,
  onOpenChange,
}: {
  clusterName: string
  catalog: AddonInfo[]
  installed: string[]
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const createAddon = useCreateAddon()
  const available = catalog.filter(
    (a) => !installed.includes(a.addonName ?? ""),
  )

  const {
    formState: { errors },
    handleSubmit,
    register,
    reset,
    setValue,
    watch,
  } = useForm<CreateAddonFormValues>({
    resolver: zodResolver(createAddonSchema),
    defaultValues: {
      addonName: "",
      addonVersion: "",
      serviceAccountRoleArn: "",
      configurationValues: "",
    },
  })

  const addonName = watch("addonName")
  const addonVersion = watch("addonVersion")
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
    await createAddon.mutateAsync({
      clusterName,
      addonName: data.addonName,
      addonVersion: data.addonVersion || undefined,
      serviceAccountRoleArn: data.serviceAccountRoleArn.trim() || undefined,
      configurationValues: data.configurationValues.trim() || undefined,
    })
    reset()
    onOpenChange(false)
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>Add add-on</AlertDialogTitle>
        </AlertDialogHeader>

        <form className="space-y-4" id="add-addon-form">
          <Field>
            <FieldTitle>
              <label htmlFor="addon-name">Add-on</label>
            </FieldTitle>
            <select
              aria-invalid={!!errors.addonName}
              className={selectClassName}
              id="addon-name"
              onChange={(e) => handleAddonChange(e.target.value)}
              value={addonName}
            >
              <option value="">Select add-on</option>
              {available.map((a) => (
                <option key={a.addonName} value={a.addonName ?? ""}>
                  {a.addonName}
                </option>
              ))}
            </select>
            <FieldError errors={[errors.addonName]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="addon-version">Version</label>
            </FieldTitle>
            <select
              aria-invalid={!!errors.addonVersion}
              className={selectClassName}
              disabled={versions.length === 0}
              id="addon-version"
              onChange={(e) =>
                setValue("addonVersion", e.target.value, {
                  shouldValidate: true,
                })
              }
              value={addonVersion}
            >
              {versions.map((v, i) => (
                <option key={v.addonVersion} value={v.addonVersion ?? ""}>
                  {v.addonVersion}
                  {i === 0 ? " (latest)" : ""}
                </option>
              ))}
            </select>
            <FieldError errors={[errors.addonVersion]} />
          </Field>

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
            <Textarea
              aria-invalid={!!errors.configurationValues}
              id="addon-config"
              placeholder='{"replicaCount": 2}'
              rows={4}
              {...register("configurationValues")}
            />
            <FieldError errors={[errors.configurationValues]} />
          </Field>
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={createAddon.isPending}
            onClick={(e) => {
              e.preventDefault()
              void handleSubmit(onSubmit)()
            }}
          >
            {createAddon.isPending ? "Adding…" : "Add"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

function UpdateAddonDialog({
  clusterName,
  addonName,
  current,
  versions,
  open,
  onOpenChange,
}: {
  clusterName: string
  addonName: string
  current: {
    addonVersion?: string
    serviceAccountRoleArn?: string
    configurationValues?: string
  }
  versions: string[]
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const updateAddon = useUpdateAddon()
  const [version, setVersion] = useState(current.addonVersion ?? "")
  const [role, setRole] = useState(current.serviceAccountRoleArn ?? "")
  const [config, setConfig] = useState(current.configurationValues ?? "")
  const [error, setError] = useState<string | undefined>()

  useEffect(() => {
    if (open) {
      setVersion(current.addonVersion ?? "")
      setRole(current.serviceAccountRoleArn ?? "")
      setConfig(current.configurationValues ?? "")
      setError(undefined)
    }
  }, [
    open,
    current.addonVersion,
    current.serviceAccountRoleArn,
    current.configurationValues,
  ])

  const handleConfirm = () => {
    if (!isValidJson(config)) {
      setError("Configuration must be valid JSON")
      return
    }
    updateAddon.mutate(
      {
        clusterName,
        addonName,
        addonVersion: version || undefined,
        serviceAccountRoleArn: role.trim() || undefined,
        configurationValues: config.trim() || undefined,
      },
      { onSuccess: () => onOpenChange(false) },
    )
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>Update {addonName}</AlertDialogTitle>
        </AlertDialogHeader>

        <div className="space-y-4">
          <Field>
            <FieldTitle>
              <label htmlFor="update-addon-version">Version</label>
            </FieldTitle>
            <select
              className={selectClassName}
              disabled={versions.length === 0}
              id="update-addon-version"
              onChange={(e) => setVersion(e.target.value)}
              value={version}
            >
              {versions.map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </select>
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="update-addon-role">
                Service account IAM role ARN
              </label>
            </FieldTitle>
            <Input
              id="update-addon-role"
              onChange={(e) => setRole(e.target.value)}
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
            <Textarea
              id="update-addon-config"
              onChange={(e) => setConfig(e.target.value)}
              rows={4}
              value={config}
            />
          </Field>
        </div>
        {error && <p className="mt-2 text-xs text-destructive">{error}</p>}

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={updateAddon.isPending}
            onClick={(e) => {
              e.preventDefault()
              handleConfirm()
            }}
          >
            {updateAddon.isPending ? "Updating…" : "Update"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

function AddonRow({
  clusterName,
  addonName,
  catalog,
  onDelete,
}: {
  clusterName: string
  addonName: string
  catalog: AddonInfo[]
  onDelete: (name: string) => void
}) {
  const { data } = useQuery({
    ...eksAddonQueryOptions(clusterName, addonName),
    refetchInterval: (query) =>
      IN_PROGRESS_STATUSES.has(query.state.data?.addon?.status ?? "")
        ? STATUS_POLL_MS
        : false,
  })
  const [showUpdate, setShowUpdate] = useState(false)

  const addon = data?.addon
  const issue = addon?.health?.issues?.[0]?.message
  const versions =
    catalog
      .find((a) => a.addonName === addonName)
      ?.addonVersions?.map((v) => v.addonVersion ?? "")
      .filter(Boolean) ?? []

  return (
    <DetailCard>
      <DetailCard.Header>
        <div className="flex items-center justify-between">
          <span>{addonName}</span>
          <div className="flex items-center gap-1">
            <Badge variant={addonStatusVariant(addon?.status)}>
              {addon?.status ?? "UNKNOWN"}
            </Badge>
            <Button
              aria-label="Update add-on"
              onClick={() => setShowUpdate(true)}
              size="icon"
              variant="ghost"
            >
              <ArrowUpCircle className="size-4" />
            </Button>
            <Button
              aria-label="Remove add-on"
              onClick={() => onDelete(addonName)}
              size="icon"
              variant="ghost"
            >
              <Trash2 className="size-4" />
            </Button>
          </div>
        </div>
      </DetailCard.Header>
      <DetailCard.Content>
        <DetailRow label="Version" value={addon?.addonVersion} />
        <DetailRow label="IRSA role" value={addon?.serviceAccountRoleArn} />
        {issue && (
          <DetailRow
            label="Issue"
            value={<span className="text-destructive">{issue}</span>}
          />
        )}
      </DetailCard.Content>

      <UpdateAddonDialog
        addonName={addonName}
        clusterName={clusterName}
        current={{
          addonVersion: addon?.addonVersion,
          serviceAccountRoleArn: addon?.serviceAccountRoleArn,
          configurationValues: addon?.configurationValues,
        }}
        onOpenChange={setShowUpdate}
        open={showUpdate}
        versions={versions}
      />
    </DetailCard>
  )
}

export function AddonsTab({ clusterName }: { clusterName: string }) {
  const { data } = useSuspenseQuery(eksAddonsQueryOptions(clusterName))
  const { data: catalogData } = useQuery(eksAddonVersionsQueryOptions)
  const deleteAddon = useDeleteAddon()
  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | undefined>()

  const addons = data.addons ?? []
  const catalog = catalogData?.addons ?? []

  return (
    <>
      <div className="mb-4 flex items-center justify-between gap-4">
        <p className="text-xs text-muted-foreground">
          Add-on manifests are staged; activation completes when the cluster
          reports the workload ready.
        </p>
        <Button onClick={() => setShowAdd(true)} size="sm">
          Add add-on
        </Button>
      </div>

      {addons.length > 0 ? (
        <div className="space-y-4">
          {addons.map((name) => (
            <AddonRow
              addonName={name}
              catalog={catalog}
              clusterName={clusterName}
              key={name}
              onDelete={setPendingDelete}
            />
          ))}
        </div>
      ) : (
        <p className="text-muted-foreground">No add-ons installed.</p>
      )}

      <AddAddonDialog
        catalog={catalog}
        clusterName={clusterName}
        installed={addons}
        onOpenChange={setShowAdd}
        open={showAdd}
      />

      <DeleteConfirmationDialog
        confirmLabel="Remove"
        description={`Remove add-on "${pendingDelete ?? ""}" from the cluster?`}
        isPending={deleteAddon.isPending}
        onConfirm={() => {
          if (!pendingDelete) {
            return
          }
          deleteAddon.mutate(
            { clusterName, addonName: pendingDelete },
            { onSuccess: () => setPendingDelete(undefined) },
          )
        }}
        onOpenChange={(o) => !o && setPendingDelete(undefined)}
        open={pendingDelete !== undefined}
        pendingLabel="Removing…"
        title="Remove add-on"
      />
    </>
  )
}
