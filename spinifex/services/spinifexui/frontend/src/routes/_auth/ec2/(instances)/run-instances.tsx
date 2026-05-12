import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { ChevronDown } from "lucide-react"
import { useEffect } from "react"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useCreateInstance } from "@/mutations/ec2"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2PlacementGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  type CreateInstanceFormData,
  VOLUME_TYPES,
  createInstanceSchema,
} from "@/types/ec2"

const DEFAULT_ROOT_DEVICE_NAME = "/dev/sda1"
const DEFAULT_VOLUME_TYPE = "gp3"
const MIN_VOLUME_SIZE_GIB = 1
const MAX_VOLUME_SIZE_GIB = 16_384

export const Route = createFileRoute("/_auth/ec2/(instances)/run-instances")({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
      context.queryClient.ensureQueryData(ec2KeyPairsQueryOptions),
      context.queryClient.ensureQueryData(ec2InstanceTypesQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2PlacementGroupsQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Run Instances | EC2 | Mulga",
      },
    ],
  }),
  component: CreateInstance,
})

function CreateInstance() {
  const navigate = useNavigate()
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)
  const { data: keyPairsData } = useSuspenseQuery(ec2KeyPairsQueryOptions)
  const { data: instanceTypesData } = useSuspenseQuery(
    ec2InstanceTypesQueryOptions,
  )
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: pgData } = useSuspenseQuery(ec2PlacementGroupsQueryOptions)
  const createMutation = useCreateInstance()
  const images = imagesData.Images ?? []
  const keyPairs = keyPairsData.KeyPairs ?? []
  const subnets = subnetsData.Subnets ?? []
  const placementGroups = pgData.PlacementGroups ?? []
  const instanceTypeCounts: Record<string, number> = {}
  for (const type of instanceTypesData.InstanceTypes ?? []) {
    const typeName = type.InstanceType
    if (typeName) {
      instanceTypeCounts[typeName] = (instanceTypeCounts[typeName] ?? 0) + 1
    }
  }

  const uniqueInstanceTypes = Object.keys(instanceTypeCounts).toSorted()

  // Compute default values from loaded data
  const defaultImageId = images[0]?.ImageId
  const defaultKeyName = keyPairs[0]?.KeyName
  const defaultInstanceType =
    uniqueInstanceTypes.find((type) => type.endsWith(".nano")) ??
    uniqueInstanceTypes[0]
  const defaultRoot = getRootMapping(
    images.find((img) => img.ImageId === defaultImageId),
  )

  const {
    control,
    handleSubmit,
    register,
    setValue,
    watch,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(
      createInstanceSchema
        .refine(
          (data) => data.count <= (instanceTypeCounts[data.instanceType] ?? 1),
          {
            message: "Cannot exceed available capacity",
            path: ["count"],
          },
        )
        .superRefine((data, ctx) => {
          const minSize = getRootMapping(
            images.find((img) => img.ImageId === data.imageId),
          ).snapshotSize
          if (
            data.rootVolumeSize !== undefined &&
            minSize !== undefined &&
            data.rootVolumeSize < minSize
          ) {
            ctx.addIssue({
              code: "custom",
              message: `Volume size must be at least ${minSize} GiB (AMI snapshot size)`,
              path: ["rootVolumeSize"],
            })
          }
        }),
    ),
    defaultValues: {
      count: 1,
      imageId: defaultImageId ?? "",
      keyName: defaultKeyName ?? "",
      instanceType: defaultInstanceType ?? "",
      rootDeviceName: defaultRoot.deviceName,
    },
  })

  const selectedInstanceType = watch("instanceType")
  const maxCount = selectedInstanceType
    ? (instanceTypeCounts[selectedInstanceType] ?? 1)
    : 1
  const selectedImageId = watch("imageId")
  const selectedRoot = getRootMapping(
    images.find((img) => img.ImageId === selectedImageId),
  )

  // Re-prefill DeviceName when the user picks a different AMI. Size / type /
  // delete-on-termination stay blank by default so an unchanged form sends
  // no BlockDeviceMappings (preserves today's backend default).
  useEffect(() => {
    setValue("rootDeviceName", selectedRoot.deviceName)
  }, [selectedImageId, selectedRoot.deviceName, setValue])

  const onSubmit = async (data: CreateInstanceFormData) => {
    await createMutation.mutateAsync(data)

    void navigate({ to: "/ec2/describe-instances" })
  }

  return (
    <>
      <BackLink to="/ec2/describe-instances">Back to instances</BackLink>
      <PageHeading title="Run EC2 Instances" />

      {/* Handle error when no instance types available */}
      {uniqueInstanceTypes.length === 0 && (
        <ErrorBanner msg="No compute available. No new instances can be created until compute is available." />
      )}

      {/* Handle error after submission */}
      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create instance"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        {/* ImageSelection */}
        <Field>
          <FieldTitle>
            <label htmlFor="imageId">Image</label>
          </FieldTitle>
          <Controller
            control={control}
            name="imageId"
            render={({ field }) => {
              const selectedImage = images.find(
                (img) => img.ImageId === field.value,
              )
              return (
                <Select
                  onValueChange={(value) => field.onChange(value)}
                  value={field.value ?? ""}
                >
                  <SelectTrigger
                    aria-invalid={!!errors.imageId}
                    className="w-full"
                    id="imageId"
                  >
                    <SelectValue>
                      {selectedImage
                        ? `${selectedImage.Name ?? "Unnamed"} (${selectedImage.Architecture})`
                        : ""}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {images.map((image) => (
                      <SelectItem
                        key={image.ImageId}
                        value={image.ImageId ?? ""}
                      >
                        {image.Name ?? "Unnamed"} ({image.Architecture})
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )
            }}
          />
          <FieldError errors={[errors.imageId]} />
        </Field>

        {/* Instance Type */}
        <Field>
          <FieldTitle>
            <label htmlFor="instanceType">Instance Type</label>
          </FieldTitle>
          <Controller
            control={control}
            name="instanceType"
            render={({ field }) => (
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value || ""}
              >
                <SelectTrigger
                  aria-invalid={!!errors.instanceType}
                  className="w-full"
                  id="instanceType"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {uniqueInstanceTypes.map((type) => (
                    <SelectItem key={type} value={type}>
                      {type} ({instanceTypeCounts[type]} available)
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.instanceType]} />
        </Field>

        {/* Key Pair */}
        <Field>
          <FieldTitle>
            <label htmlFor="keyName">Key Pair</label>
          </FieldTitle>
          <Controller
            control={control}
            name="keyName"
            render={({ field }) => (
              <Select
                onValueChange={(value) => field.onChange(value)}
                value={field.value || ""}
              >
                <SelectTrigger
                  aria-invalid={!!errors.keyName}
                  className="w-full"
                  id="keyName"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {keyPairs.map((keyPair) => (
                    <SelectItem
                      key={keyPair.KeyPairId}
                      value={keyPair.KeyName ?? ""}
                    >
                      {keyPair.KeyName}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.keyName]} />
        </Field>

        {/* Subnet */}
        <Field>
          <FieldTitle>
            <label htmlFor="subnetId">Subnet</label>
          </FieldTitle>
          <Controller
            control={control}
            name="subnetId"
            render={({ field }) => (
              <Select
                onValueChange={(value) =>
                  field.onChange(value === "none" ? undefined : value)
                }
                value={field.value ?? "none"}
              >
                <SelectTrigger className="w-full" id="subnetId">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">none</SelectItem>
                  {subnets.map((subnet) => (
                    <SelectItem
                      key={subnet.SubnetId}
                      value={subnet.SubnetId ?? ""}
                    >
                      {subnet.SubnetId}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
        </Field>

        {/* Placement Group */}
        <Field>
          <FieldTitle>
            <label htmlFor="placementGroupName">Placement Group</label>
          </FieldTitle>
          <Controller
            control={control}
            name="placementGroupName"
            render={({ field }) => (
              <Select
                onValueChange={(value) =>
                  field.onChange(value === "none" ? undefined : value)
                }
                value={field.value ?? "none"}
              >
                <SelectTrigger className="w-full" id="placementGroupName">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">none</SelectItem>
                  {placementGroups.map((pg) => (
                    <SelectItem key={pg.GroupId} value={pg.GroupName ?? ""}>
                      {pg.GroupName} ({pg.Strategy})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
        </Field>

        {/* Instance Count */}
        <Field>
          <FieldTitle>
            <label htmlFor="count">Number of Instances</label>
          </FieldTitle>
          <Input
            aria-describedby="count-description"
            aria-invalid={!!errors.count}
            id="count"
            type="number"
            {...register("count", { valueAsNumber: true })}
          />
          <p className="text-xs text-muted-foreground" id="count-description">
            {selectedInstanceType &&
              `Available capacity for ${selectedInstanceType}: ${maxCount}`}
          </p>
          <FieldError errors={[errors.count]} />
        </Field>

        {/* Block Device Mappings (root volume) — collapsed by default */}
        <Collapsible>
          <CollapsibleTrigger
            className="group flex h-7 w-full items-center justify-between rounded-md border border-input bg-input/20 px-2 py-0.5 text-sm transition-colors outline-none hover:bg-input/40 focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30 md:text-xs/relaxed dark:bg-input/30 dark:hover:bg-input/50"
            render={<button type="button" />}
          >
            <span>Block Device Mappings (root volume)</span>
            <ChevronDown className="size-3.5 text-muted-foreground transition-transform group-data-[panel-open]:rotate-180" />
          </CollapsibleTrigger>
          <CollapsibleContent className="mt-3 space-y-4 rounded-md border border-border p-4">
            <Field>
              <FieldTitle>
                <label htmlFor="rootDeviceName">Device Name</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.rootDeviceName}
                id="rootDeviceName"
                type="text"
                {...register("rootDeviceName")}
              />
              <FieldError errors={[errors.rootDeviceName]} />
            </Field>

            <Field>
              <FieldTitle>
                <label htmlFor="rootVolumeSize">Volume Size (GiB)</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.rootVolumeSize}
                id="rootVolumeSize"
                max={MAX_VOLUME_SIZE_GIB}
                min={selectedRoot.snapshotSize ?? MIN_VOLUME_SIZE_GIB}
                placeholder={
                  selectedRoot.snapshotSize
                    ? `${selectedRoot.snapshotSize} (AMI default)`
                    : "use AMI / backend default"
                }
                type="number"
                {...register("rootVolumeSize", {
                  setValueAs: (value: string) =>
                    value === "" || value === null || value === undefined
                      ? undefined
                      : Number(value),
                })}
              />
              {selectedRoot.snapshotSize !== undefined && (
                <FieldDescription>
                  AMI snapshot size: {selectedRoot.snapshotSize} GiB
                </FieldDescription>
              )}
              <FieldError errors={[errors.rootVolumeSize]} />
            </Field>

            <Field>
              <FieldTitle>
                <label htmlFor="rootVolumeType">Volume Type</label>
              </FieldTitle>
              <Controller
                control={control}
                name="rootVolumeType"
                render={({ field }) => (
                  <Select
                    onValueChange={(value) => field.onChange(value)}
                    value={field.value ?? DEFAULT_VOLUME_TYPE}
                  >
                    <SelectTrigger
                      aria-invalid={!!errors.rootVolumeType}
                      className="w-full"
                      id="rootVolumeType"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {VOLUME_TYPES.map((vt) => (
                        <SelectItem key={vt} value={vt}>
                          {vt}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
              <FieldError errors={[errors.rootVolumeType]} />
            </Field>

            <Field>
              <Controller
                control={control}
                name="rootDeleteOnTermination"
                render={({ field }) => (
                  <label className="flex items-center gap-2 text-sm">
                    <input
                      checked={field.value ?? true}
                      onChange={(e) => field.onChange(e.target.checked)}
                      type="checkbox"
                    />
                    Delete on termination
                  </label>
                )}
              />
            </Field>
          </CollapsibleContent>
        </Collapsible>

        <CliCommandPanel commands={buildRunInstancesCommands(watch)} />

        {/* Actions */}
        <div className="flex gap-2">
          <Button
            disabled={isSubmitting || createMutation.isPending}
            onClick={async () =>
              await navigate({ to: "/ec2/describe-instances" })
            }
            type="button"
            variant="outline"
          >
            Cancel
          </Button>
          <Button
            disabled={
              isSubmitting ||
              createMutation.isPending ||
              uniqueInstanceTypes.length === 0
            }
            type="submit"
          >
            {isSubmitting || createMutation.isPending
              ? "Creating…"
              : "Create Instance"}
          </Button>
        </div>
      </form>
    </>
  )
}

interface RootMappingDefaults {
  deviceName: string
  snapshotSize: number | undefined
}

interface ImageLike {
  RootDeviceName?: string
  BlockDeviceMappings?: { DeviceName?: string; Ebs?: { VolumeSize?: number } }[]
}

function getRootMapping(image: ImageLike | undefined): RootMappingDefaults {
  const deviceName = image?.RootDeviceName ?? DEFAULT_ROOT_DEVICE_NAME
  const mappings = image?.BlockDeviceMappings ?? []
  const root = mappings.find((m) => m.DeviceName === deviceName) ?? mappings[0]
  return {
    deviceName,
    snapshotSize: root?.Ebs?.VolumeSize,
  }
}

function buildRunInstancesCommands(
  watch: (name?: string) => unknown,
): CliCommand[] {
  const rawImageId = watch("imageId")
  const imageId = typeof rawImageId === "string" ? rawImageId : ""
  const rawInstanceType = watch("instanceType")
  const instanceType =
    typeof rawInstanceType === "string" ? rawInstanceType : ""
  const rawKeyName = watch("keyName")
  const keyName = typeof rawKeyName === "string" ? rawKeyName : ""
  const rawSubnetId = watch("subnetId")
  const subnetId = typeof rawSubnetId === "string" ? rawSubnetId : ""
  const rawPlacementGroupName = watch("placementGroupName")
  const placementGroupName =
    typeof rawPlacementGroupName === "string" ? rawPlacementGroupName : ""
  const rawCount = watch("count")
  const count = typeof rawCount === "number" ? rawCount : 0
  const rawRootDeviceName = watch("rootDeviceName")
  const rootDeviceName =
    typeof rawRootDeviceName === "string" ? rawRootDeviceName : ""
  const rawRootVolumeSize = watch("rootVolumeSize")
  const rootVolumeSize =
    typeof rawRootVolumeSize === "number" ? rawRootVolumeSize : undefined
  const rawRootVolumeType = watch("rootVolumeType")
  const rootVolumeType =
    typeof rawRootVolumeType === "string" ? rawRootVolumeType : ""
  const rawRootDeleteOnTermination = watch("rootDeleteOnTermination")
  const rootDeleteOnTermination =
    typeof rawRootDeleteOnTermination === "boolean"
      ? rawRootDeleteOnTermination
      : true

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws ec2 run-instances",
    },
    { type: "flag" as const, value: " \\\n  --image-id" },
    { type: "value" as const, value: ` ${imageId || "<ImageId>"}` },
    { type: "flag" as const, value: " \\\n  --instance-type" },
    { type: "value" as const, value: ` ${instanceType || "<InstanceType>"}` },
    { type: "flag" as const, value: " \\\n  --key-name" },
    { type: "value" as const, value: ` ${keyName || "<KeyName>"}` },
  ]

  if (subnetId) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --subnet-id" },
      { type: "value" as const, value: ` ${subnetId}` },
    )
  }

  if (placementGroupName) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --placement" },
      { type: "value" as const, value: ` GroupName=${placementGroupName}` },
    )
  }

  parts.push(
    { type: "flag" as const, value: " \\\n  --count" },
    { type: "value" as const, value: ` ${count || 1}` },
  )

  const hasStorageOverride =
    rootVolumeSize !== undefined ||
    rawRootVolumeType !== undefined ||
    rawRootDeleteOnTermination !== undefined
  if (hasStorageOverride) {
    const ebs: Record<string, unknown> = {}
    if (rootVolumeSize !== undefined) {
      ebs.VolumeSize = rootVolumeSize
    }
    if (rootVolumeType) {
      ebs.VolumeType = rootVolumeType
    }
    if (rawRootDeleteOnTermination !== undefined) {
      ebs.DeleteOnTermination = rootDeleteOnTermination
    }
    const bdm = JSON.stringify([
      {
        DeviceName: rootDeviceName || DEFAULT_ROOT_DEVICE_NAME,
        Ebs: ebs,
      },
    ])
    parts.push(
      { type: "flag" as const, value: " \\\n  --block-device-mappings" },
      { type: "value" as const, value: ` '${bdm}'` },
    )
  }

  return [{ label: "Run Instances", parts }]
}
