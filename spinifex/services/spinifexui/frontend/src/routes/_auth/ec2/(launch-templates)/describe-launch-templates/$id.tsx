import type { ResponseLaunchTemplateData } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { Rocket, Star, Trash2 } from "lucide-react"
import { useState } from "react"
import { useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { formatDateTime } from "@/lib/utils"
import {
  useCreateLaunchTemplateVersion,
  useDeleteLaunchTemplate,
  useDeleteLaunchTemplateVersions,
  useModifyLaunchTemplate,
} from "@/mutations/ec2"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2LaunchTemplatesQueryOptions,
  ec2LaunchTemplateVersionsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  type LaunchTemplateFormData,
  createLaunchTemplateVersionSchema,
} from "@/types/ec2"

import { LaunchTemplateDataFields } from "../-components/launch-template-data-fields"

export const Route = createFileRoute(
  "/_auth/ec2/(launch-templates)/describe-launch-templates/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2LaunchTemplatesQueryOptions),
      context.queryClient.ensureQueryData(
        ec2LaunchTemplateVersionsQueryOptions(params.id),
      ),
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
      context.queryClient.ensureQueryData(ec2InstanceTypesQueryOptions),
      context.queryClient.ensureQueryData(ec2KeyPairsQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Launch Template | Mulga`,
      },
    ],
  }),
  component: LaunchTemplateDetail,
})

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

function LaunchTemplateDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data: listData } = useSuspenseQuery(ec2LaunchTemplatesQueryOptions)
  const { data: versionsData } = useSuspenseQuery(
    ec2LaunchTemplateVersionsQueryOptions(id),
  )

  const setDefault = useModifyLaunchTemplate()
  const deleteVersions = useDeleteLaunchTemplateVersions()
  const deleteTemplate = useDeleteLaunchTemplate()
  const createVersion = useCreateLaunchTemplateVersion()

  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const lt = listData.LaunchTemplates?.find((t) => t.LaunchTemplateId === id)
  const versions = (versionsData.LaunchTemplateVersions ?? []).toSorted(
    (a, b) => (b.VersionNumber ?? 0) - (a.VersionNumber ?? 0),
  )

  const versionForm = useForm<LaunchTemplateFormData>({
    resolver: zodResolver(createLaunchTemplateVersionSchema),
    defaultValues: {
      versionDescription: "",
      ...BLANK_DATA_FIELDS,
    },
  })
  const {
    control,
    handleSubmit,
    register,
    getValues,
    reset,
    formState: { errors, isSubmitting },
  } = versionForm

  if (!lt?.LaunchTemplateId) {
    return (
      <>
        <BackLink to="/ec2/describe-launch-templates">
          Back to launch templates
        </BackLink>
        <p className="text-muted-foreground">Launch template not found.</p>
      </>
    )
  }

  const defaultVersion = lt.DefaultVersionNumber

  const applySourceVersion = (value: string) => {
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

  const handleSetDefault = async (versionNumber: number) => {
    await setDefault.mutateAsync({
      launchTemplateId: id,
      defaultVersion: String(versionNumber),
    })
  }

  const handleDeleteVersion = async (versionNumber: number) => {
    await deleteVersions.mutateAsync({
      launchTemplateId: id,
      versions: [String(versionNumber)],
    })
  }

  const handleDeleteTemplate = async () => {
    try {
      await deleteTemplate.mutateAsync(id)
      navigate({ to: "/ec2/describe-launch-templates" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const onCreateVersion = async (data: LaunchTemplateFormData) => {
    await createVersion.mutateAsync({ ...data, launchTemplateId: id })
    reset({
      versionDescription: "",
      sourceVersion: undefined,
      ...BLANK_DATA_FIELDS,
    })
  }

  const rowError = setDefault.error ?? deleteVersions.error

  return (
    <>
      <BackLink to="/ec2/describe-launch-templates">
        Back to launch templates
      </BackLink>

      {deleteTemplate.error && (
        <ErrorBanner
          error={deleteTemplate.error}
          msg="Failed to delete launch template"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                disabled={deleteTemplate.isPending}
                onClick={() => setShowDeleteDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
              <Badge variant="secondary">default v{defaultVersion}</Badge>
            </div>
          }
          subtitle="Launch Template Details"
          title={lt.LaunchTemplateName ?? lt.LaunchTemplateId}
        />

        <DetailCard>
          <DetailCard.Header>Launch Template Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Launch Template ID" value={lt.LaunchTemplateId} />
            <DetailRow label="Name" value={lt.LaunchTemplateName} />
            <DetailRow
              label="Default Version"
              value={String(lt.DefaultVersionNumber)}
            />
            <DetailRow
              label="Latest Version"
              value={String(lt.LatestVersionNumber)}
            />
            <DetailRow label="Created" value={formatDateTime(lt.CreateTime)} />
          </DetailCard.Content>
        </DetailCard>

        <DetailCard>
          <DetailCard.Header>Versions</DetailCard.Header>
          <div className="space-y-4 p-4">
            {rowError && (
              <ErrorBanner error={rowError} msg="Failed to update version" />
            )}
            <div className="overflow-x-auto rounded-md border">
              <table className="w-full text-sm">
                <thead className="bg-muted/50 text-left text-muted-foreground">
                  <tr>
                    <th className="p-2 font-medium">Version</th>
                    <th className="p-2 font-medium">Description</th>
                    <th className="p-2 font-medium">Default</th>
                    <th className="p-2 font-medium">Created</th>
                    <th className="p-2">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {versions.map((version) => {
                    const versionNumber = version.VersionNumber
                    const isDefault = versionNumber === defaultVersion
                    return (
                      <tr className="border-t" key={versionNumber}>
                        <td className="p-2 font-mono">{versionNumber}</td>
                        <td className="p-2">
                          {version.VersionDescription ?? "—"}
                        </td>
                        <td className="p-2">
                          {isDefault ? (
                            <Badge variant="secondary">Default</Badge>
                          ) : (
                            "—"
                          )}
                        </td>
                        <td className="p-2">
                          {formatDateTime(version.CreateTime)}
                        </td>
                        <td className="p-2">
                          <div className="flex items-center justify-end gap-1">
                            <Link
                              search={{
                                launchTemplateId: id,
                                launchTemplateVersion: String(versionNumber),
                              }}
                              to="/ec2/run-instances"
                            >
                              <Button size="sm" variant="ghost">
                                <Rocket className="size-4" />
                                Launch
                              </Button>
                            </Link>
                            {!isDefault && versionNumber !== undefined && (
                              <Button
                                disabled={setDefault.isPending}
                                onClick={async () =>
                                  await handleSetDefault(versionNumber)
                                }
                                size="sm"
                                variant="ghost"
                              >
                                <Star className="size-4" />
                                Set default
                              </Button>
                            )}
                            <Button
                              disabled={
                                isDefault ||
                                deleteVersions.isPending ||
                                versionNumber === undefined
                              }
                              onClick={async () => {
                                if (versionNumber !== undefined) {
                                  await handleDeleteVersion(versionNumber)
                                }
                              }}
                              size="sm"
                              variant="ghost"
                            >
                              <Trash2 className="size-4" />
                            </Button>
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          </div>
        </DetailCard>

        <DetailCard>
          <DetailCard.Header>Create Version</DetailCard.Header>
          <div className="p-4">
            {createVersion.error && (
              <ErrorBanner
                error={createVersion.error}
                msg="Failed to create version"
              />
            )}
            <form
              className="space-y-6"
              onSubmit={handleSubmit(onCreateVersion)}
            >
              <Field>
                <FieldTitle>
                  <label htmlFor="sourceVersion">Clone from version</label>
                </FieldTitle>
                <Select
                  onValueChange={(value) => applySourceVersion(value ?? "none")}
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
                  <label htmlFor="versionDescription">
                    Version Description
                  </label>
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
          </div>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the launch template "${lt.LaunchTemplateName ?? lt.LaunchTemplateId}"? All versions will be deleted. This action cannot be undone.`}
        isPending={deleteTemplate.isPending}
        onConfirm={handleDeleteTemplate}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Launch Template"
      />
    </>
  )
}
