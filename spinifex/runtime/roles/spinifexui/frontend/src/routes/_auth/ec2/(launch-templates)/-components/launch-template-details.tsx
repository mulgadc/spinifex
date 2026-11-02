import type {
  LaunchTemplate,
  ResponseLaunchTemplateData,
} from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  Eye,
  Maximize2,
  Rocket,
  SlidersHorizontal,
  Star,
  Trash2,
} from "lucide-react"
import { Suspense, useState } from "react"
import { useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  useCreateLaunchTemplateVersion,
  useDeleteLaunchTemplate,
  useDeleteLaunchTemplateVersions,
  useModifyLaunchTemplate,
} from "@/mutations/ec2"
import { ec2LaunchTemplateVersionsQueryOptions } from "@/queries/ec2"
import {
  type LaunchTemplateFormData,
  createLaunchTemplateVersionSchema,
} from "@/types/ec2"

import { LaunchTemplateDataFields } from "./launch-template-data-fields"

function decodeUserData(encoded: string | undefined): string {
  if (!encoded) {
    return ""
  }

  try {
    const binary = atob(encoded)
    const bytes = new Uint8Array(binary.length)

    for (let i = 0; i < binary.length; i++) {
      bytes[i] = binary.codePointAt(i) ?? 0
    }

    return new TextDecoder().decode(bytes)
  } catch {
    return ""
  }
}

// versionToFormData maps a stored version's data back to the form subset so a
// new version can be cloned from it (clone-and-override).
function versionToFormData(
  data: ResponseLaunchTemplateData | undefined,
): Partial<LaunchTemplateFormData> {
  if (!data) {
    return {}
  }

  const primaryNic = data.NetworkInterfaces?.[0]
  const rootEbs = data.BlockDeviceMappings?.[0]?.Ebs

  return {
    imageId: data.ImageId ?? "",
    instanceType: data.InstanceType ?? "",
    keyName: data.KeyName,
    subnetId: primaryNic?.SubnetId,
    securityGroupIds: primaryNic?.Groups ?? data.SecurityGroupIds ?? [],
    userData: decodeUserData(data.UserData),
    rootVolumeSize: rootEbs?.VolumeSize,
    rootVolumeType: rootEbs?.VolumeType === "gp3" ? "gp3" : undefined,
  }
}

const BLANK_DATA_FIELDS: Partial<LaunchTemplateFormData> = {
  imageId: "",
  instanceType: "",
  keyName: undefined,
  subnetId: undefined,
  securityGroupIds: [],
  userData: "",
  rootVolumeSize: undefined,
  rootVolumeType: undefined,
}

function useSortedVersions(id: string) {
  const { data } = useSuspenseQuery(ec2LaunchTemplateVersionsQueryOptions(id))

  return (data.LaunchTemplateVersions ?? []).toSorted(
    (a, b) => (b.VersionNumber ?? 0) - (a.VersionNumber ?? 0),
  )
}

function templateLabel(lt: LaunchTemplate): string {
  return lt.LaunchTemplateName ?? lt.LaunchTemplateId ?? ""
}

function Versions({ lt }: { lt: LaunchTemplate }) {
  const id = lt.LaunchTemplateId ?? ""
  const versions = useSortedVersions(id)
  const setDefault = useModifyLaunchTemplate()
  const deleteVersions = useDeleteLaunchTemplateVersions()
  const [versionToDelete, setVersionToDelete] = useState<number | null>(null)
  const defaultVersion = lt.DefaultVersionNumber
  const rowError = setDefault.error ?? deleteVersions.error

  async function handleSetDefault(versionNumber: number) {
    try {
      await setDefault.mutateAsync({
        launchTemplateId: id,
        defaultVersion: String(versionNumber),
      })
    } catch {
      // Shown by the banner above the versions.
    }
  }

  async function handleDeleteVersion() {
    if (versionToDelete === null) {
      return
    }

    try {
      await deleteVersions.mutateAsync({
        launchTemplateId: id,
        versions: [String(versionToDelete)],
      })
    } catch {
      // Shown by the banner above the versions.
    } finally {
      setVersionToDelete(null)
    }
  }

  return (
    <PanelSection title="Versions">
      {rowError && (
        <ErrorBanner error={rowError} msg="Failed to update version" />
      )}
      {versions.length ? (
        <ul className="divide-y text-sm">
          {versions.map((version) => {
            const versionNumber = version.VersionNumber
            const isDefault = versionNumber === defaultVersion

            return (
              <li
                className="flex items-center justify-between gap-3 py-2"
                key={versionNumber}
              >
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="font-mono">v{versionNumber}</span>
                    {isDefault && <Badge variant="secondary">Default</Badge>}
                  </div>
                  <p className="truncate text-xs text-muted-foreground">
                    {version.VersionDescription ?? "No description"} ·{" "}
                    {formatDateTime(version.CreateTime)}
                  </p>
                </div>
                <div className="flex shrink-0 items-center gap-1">
                  <Link
                    aria-label={`Launch version ${versionNumber}`}
                    className={buttonVariants({
                      size: "icon-sm",
                      variant: "ghost",
                    })}
                    search={{
                      create: true,
                      template: id,
                      templateVersion: String(versionNumber),
                    }}
                    to="/ec2/describe-instances"
                  >
                    <Rocket />
                  </Link>
                  <Button
                    aria-label={`Set version ${versionNumber} as default`}
                    disabled={
                      isDefault ||
                      setDefault.isPending ||
                      versionNumber === undefined
                    }
                    onClick={() => {
                      if (versionNumber !== undefined) {
                        void handleSetDefault(versionNumber)
                      }
                    }}
                    size="icon-sm"
                    variant="ghost"
                  >
                    <Star />
                  </Button>
                  <Button
                    aria-label={`Delete version ${versionNumber}`}
                    disabled={
                      isDefault ||
                      deleteVersions.isPending ||
                      versionNumber === undefined
                    }
                    onClick={() => {
                      if (versionNumber !== undefined) {
                        setVersionToDelete(versionNumber)
                      }
                    }}
                    size="icon-sm"
                    variant="ghost"
                  >
                    <Trash2 />
                  </Button>
                </div>
              </li>
            )
          })}
        </ul>
      ) : (
        <p className="text-sm text-muted-foreground">No versions.</p>
      )}
      <DeleteConfirmationDialog
        description={`Delete version ${versionToDelete} of "${templateLabel(lt)}"? This action cannot be undone.`}
        isPending={deleteVersions.isPending}
        onConfirm={handleDeleteVersion}
        onOpenChange={(open) => {
          if (!open) {
            setVersionToDelete(null)
          }
        }}
        open={versionToDelete !== null}
        title="Delete Launch Template Version"
      />
    </PanelSection>
  )
}

function Overview({ lt }: { lt: LaunchTemplate }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField
            copy={lt.LaunchTemplateId}
            label="Launch template ID"
            mono
          >
            {lt.LaunchTemplateId}
          </PanelField>
          <PanelField label="Name">{lt.LaunchTemplateName}</PanelField>
          <PanelField label="Default version">
            {lt.DefaultVersionNumber}
          </PanelField>
          <PanelField label="Latest version">
            {lt.LatestVersionNumber}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(lt.CreateTime)}
          </PanelField>
          <PanelField label="Created by" mono>
            {lt.CreatedBy}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <Versions lt={lt} />
      </Suspense>
    </>
  )
}

function CreateVersion({ lt }: { lt: LaunchTemplate }) {
  const id = lt.LaunchTemplateId ?? ""
  const versions = useSortedVersions(id)
  const createVersion = useCreateLaunchTemplateVersion()

  const {
    control,
    handleSubmit,
    register,
    getValues,
    reset,
    formState: { errors, isSubmitting },
  } = useForm<LaunchTemplateFormData>({
    resolver: zodResolver(createLaunchTemplateVersionSchema),
    defaultValues: {
      versionDescription: "",
      ...BLANK_DATA_FIELDS,
    },
  })

  function applySourceVersion(value: string) {
    if (value === "none") {
      reset({
        ...getValues(),
        sourceVersion: undefined,
        ...BLANK_DATA_FIELDS,
      })

      return
    }

    const src = versions.find((v) => String(v.VersionNumber) === value)
    reset({
      ...getValues(),
      sourceVersion: value,
      ...versionToFormData(src?.LaunchTemplateData),
    })
  }

  async function onCreateVersion(data: LaunchTemplateFormData) {
    try {
      await createVersion.mutateAsync({ ...data, launchTemplateId: id })
    } catch {
      // Surfaced in the error banner through the mutation's error.
      return
    }

    reset({
      versionDescription: "",
      sourceVersion: undefined,
      ...BLANK_DATA_FIELDS,
    })
  }

  return (
    <PanelSection title="Create version">
      {createVersion.error && (
        <ErrorBanner
          error={createVersion.error}
          msg="Failed to create version"
        />
      )}
      <form className="space-y-6" onSubmit={handleSubmit(onCreateVersion)}>
        <Field>
          <FieldTitle>
            <label htmlFor="sourceVersion">Clone from version</label>
          </FieldTitle>
          <Select
            onValueChange={(value) => {
              applySourceVersion(value ?? "none")
            }}
            value={getValues("sourceVersion") ?? "none"}
          >
            <SelectTrigger className="w-full" id="sourceVersion">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="none">none (start blank)</SelectItem>
              {versions.map((version) => (
                <SelectItem
                  key={version.VersionNumber}
                  value={String(version.VersionNumber)}
                >
                  v{version.VersionNumber}
                  {version.VersionDescription
                    ? ` — ${version.VersionDescription}`
                    : ""}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="versionDescription">Version Description</label>
          </FieldTitle>
          <Input
            id="versionDescription"
            placeholder="What changed in this version"
            {...register("versionDescription")}
          />
        </Field>

        <LaunchTemplateDataFields
          control={control}
          errors={errors}
          register={register}
        />

        <Button
          disabled={isSubmitting || createVersion.isPending}
          type="submit"
        >
          {isSubmitting || createVersion.isPending
            ? "Creating…"
            : "Create Version"}
        </Button>
      </form>
    </PanelSection>
  )
}

function Settings({
  lt,
  onDeleted,
}: {
  lt: LaunchTemplate
  onDeleted: () => void
}) {
  const id = lt.LaunchTemplateId ?? ""
  const remove = useDeleteLaunchTemplate()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Suspense fallback={<PanelSpinner />}>
        <CreateVersion lt={lt} />
      </Suspense>
      <Ec2Tags
        id={lt.LaunchTemplateId}
        invalidate={["ec2", "launchTemplates"]}
        tags={lt.Tags}
      />
      <PanelSection title="Manage">
        <Button
          disabled={remove.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete launch template
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Are you sure you want to delete the launch template "${templateLabel(lt)}"? All versions will be deleted. This action cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete Launch Template"
      />
    </>
  )
}

interface LaunchTemplateDetailsProps {
  launchTemplate: LaunchTemplate
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One launch template's details, as the list's side panel or as its own page.
export function LaunchTemplateDetails({
  launchTemplate: lt,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: LaunchTemplateDetailsProps) {
  const id = lt.LaunchTemplateId ?? ""
  const name = lt.LaunchTemplateName

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to launch templates"
          : "Close launch template details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open launch template page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-launch-templates/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Launch template details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={
        <Badge variant="secondary">default v{lt.DefaultVersionNumber}</Badge>
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview lt={lt} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings lt={lt} onDeleted={onClose} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
