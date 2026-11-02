import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"
import { useForm, useWatch } from "react-hook-form"

import { CliCommandPanel } from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import {
  type Architecture,
  buildImageCatalog,
  imageArchitecture,
  imageArchitectures,
  instanceTypeSupports,
  latestOfDistro,
  parseImageName,
  preferredImage,
} from "@/lib/os-images"
import { isSystemManagedImage } from "@/lib/system-managed"
import { useCreateInstance } from "@/mutations/ec2"
import { instanceTypeCapacityQueryOptions } from "@/queries/admin"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2LaunchTemplatesQueryOptions,
  ec2PlacementGroupsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import type { CreateInstanceFormData, CreateInstanceParams } from "@/types/ec2"

import { buildRunInstancesCommands } from "./run-instances-cli"
import {
  defaultTypeName,
  imageRootMapping,
  initialArchitecture,
  resolveSubnet,
  runInstancesDefaults,
  runInstancesResolver,
  typesForArchitecture,
  uniqueTypeNames,
} from "./run-instances-helpers"
import {
  CountField,
  ImageSection,
  InstanceSection,
  LaunchTemplateSection,
  NetworkSection,
  RootVolumeSection,
} from "./run-instances-sections"

export { describeTypeSize } from "./run-instances-helpers"

interface RunInstancesPageProps {
  launchTemplateId?: string
  launchTemplateVersion?: string
  // Called with the new instance's ID when one was launched, or with nothing
  // on cancel or for several.
  onClose: (instanceId?: string) => void
}

function useRunInstancesData() {
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)
  const { data: keyPairsData } = useSuspenseQuery(ec2KeyPairsQueryOptions)

  const { data: instanceTypesData } = useSuspenseQuery(
    ec2InstanceTypesQueryOptions,
  )

  // Not a suspense query: the figure is a refinement of the form, so a session
  // whose policy does not grant the action still gets a usable launch panel.
  const { data: capacityData } = useQuery(instanceTypeCapacityQueryOptions)

  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: pgData } = useSuspenseQuery(ec2PlacementGroupsQueryOptions)
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: sgData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const { data: ltData } = useSuspenseQuery(ec2LaunchTemplatesQueryOptions)

  const images = (imagesData.Images ?? []).filter(
    (image) => !isSystemManagedImage(image),
  )

  const subnets = subnetsData.Subnets ?? []
  const vpcs = vpcsData.Vpcs ?? []

  // RunInstances with no subnet lands in the account's default subnet, which
  // DescribeSubnets marks DefaultForAz.
  const defaultSubnet = subnets.find((s) => s.DefaultForAz)

  return {
    images,
    allInstanceTypes: instanceTypesData.InstanceTypes ?? [],
    instanceTypeCapacity: capacityData?.available ?? {},
    keyPairs: keyPairsData.KeyPairs ?? [],
    subnets,
    placementGroups: pgData.PlacementGroups ?? [],
    securityGroups: sgData.SecurityGroups ?? [],
    launchTemplates: ltData.LaunchTemplates ?? [],
    defaultVpcId: (vpcs.find((v) => v.IsDefault) ?? vpcs[0])?.VpcId,
    defaultSubnet,
    otherSubnets: subnets.filter((s) => s !== defaultSubnet),
  }
}

export function RunInstancesPage({
  launchTemplateId,
  launchTemplateVersion,
  onClose,
}: RunInstancesPageProps) {
  const {
    images,
    allInstanceTypes,
    instanceTypeCapacity,
    keyPairs,
    subnets,
    placementGroups,
    securityGroups,
    launchTemplates,
    defaultVpcId,
    defaultSubnet,
    otherSubnets,
  } = useRunInstancesData()

  const createMutation = useCreateInstance()
  // The catalogue answers whether anything is launchable; the capacity figure
  // is a separate query that may not have been granted or may not have landed.
  const hasCompute = allInstanceTypes.length > 0

  // The architecture choice appears only when the catalog holds both; it
  // narrows the images and the instance types together, so a launch cannot
  // pair an arm64 type with an x86_64 image.
  const architectures = imageArchitectures(images)
  const initialArch = initialArchitecture(architectures, allInstanceTypes)
  const [arch, setArch] = useState<Architecture | undefined>(initialArch)
  const archTypes = typesForArchitecture(allInstanceTypes, arch)
  const archTypeNames = uniqueTypeNames(archTypes)

  const {
    control,
    handleSubmit,
    register,
    setValue,
    getValues,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: runInstancesResolver(
      images,
      allInstanceTypes,
      instanceTypeCapacity,
    ),
    defaultValues: runInstancesDefaults({
      images,
      arch,
      archTypeNames,
      keyPairs,
      securityGroups,
      launchTemplateId,
      launchTemplateVersion,
    }),
  })

  const values = useWatch({ control })
  const selectedSubnetId = useWatch({ control, name: "subnetId" })

  // Launch template: when one is selected the instance is launched from it and
  // the direct-config fields below are hidden — the template supplies them.
  const selectedTemplateId = useWatch({ control, name: "launchTemplateId" })

  const effectiveSubnet = resolveSubnet(
    subnets,
    selectedSubnetId,
    defaultSubnet,
  )

  const effectiveVpcId = effectiveSubnet?.VpcId ?? defaultVpcId

  function selectImage(imageId: string) {
    setValue("imageId", imageId, { shouldValidate: !!errors.imageId })
    // Re-prefill DeviceName for the new AMI. Size, type and
    // delete-on-termination stay blank so an unchanged form
    // sends no BlockDeviceMappings.
    setValue("rootDeviceName", imageRootMapping(images, imageId).deviceName)
  }

  // Changing architecture keeps the chosen distro where the other
  // architecture has it, and swaps any type that cannot run the new images.
  function selectArchitecture(next: Architecture) {
    setArch(next)

    const nextCatalog = buildImageCatalog(images, next)
    const current = images.find((img) => img.ImageId === getValues("imageId"))
    const currentArch = current && imageArchitecture(current)

    if (!current || (currentArch && currentArch !== next)) {
      const replacement =
        latestOfDistro(nextCatalog, parseImageName(current?.Name)?.distro) ??
        preferredImage(nextCatalog)

      selectImage(replacement?.ImageId ?? "")
    }

    const type = allInstanceTypes.find(
      (t) => t.InstanceType === getValues("instanceType"),
    )

    if (!type || !instanceTypeSupports(type, next)) {
      setValue(
        "instanceType",
        defaultTypeName(
          uniqueTypeNames(typesForArchitecture(allInstanceTypes, next)),
        ) ?? "",
      )
    }
  }

  const onSubmit = async (data: CreateInstanceFormData) => {
    // The banner above says why; without compute only a template can launch.
    if (!data.launchTemplateId && !hasCompute) {
      return
    }

    // Launch wholly from the template (plus count). Build params from only the
    // fields that apply so no direct-config field — including hidden ones left
    // in form state — overrides what the template defines.
    const params: CreateInstanceParams = data.launchTemplateId
      ? {
          count: data.count,
          launchTemplateId: data.launchTemplateId,
          launchTemplateVersion: data.launchTemplateVersion,
          resolvedVpcId: effectiveVpcId,
        }
      : { ...data, resolvedVpcId: effectiveVpcId }

    let result

    try {
      result = await createMutation.mutateAsync(params)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    const launched = result.Instances ?? []

    onClose(launched.length === 1 ? launched[0]?.InstanceId : undefined)
  }

  const showTemplates = launchTemplates.length > 0 || !!launchTemplateId

  const countField = (
    <CountField
      control={control}
      errors={errors}
      instanceTypeCapacity={instanceTypeCapacity}
      register={register}
    />
  )

  return (
    <>
      {!hasCompute && !selectedTemplateId && (
        <ErrorBanner msg="No compute available. No new instances can be created until compute is available." />
      )}

      <form onSubmit={handleSubmit(onSubmit)}>
        <FormLock
          className="flex flex-col gap-4"
          locked={isSubmitting || createMutation.isPending}
        >
          {showTemplates && (
            <LaunchTemplateSection
              control={control}
              countField={countField}
              launchTemplates={launchTemplates}
              selectedTemplateId={selectedTemplateId}
            />
          )}

          {!selectedTemplateId && (
            <>
              <ImageSection
                arch={arch}
                architectures={architectures}
                control={control}
                errors={errors}
                images={images}
                onSelectArchitecture={selectArchitecture}
                onSelectImage={selectImage}
              />

              <InstanceSection
                allInstanceTypes={allInstanceTypes}
                arch={arch}
                archTypeNames={archTypeNames}
                archTypes={archTypes}
                control={control}
                countField={countField}
                errors={errors}
                hasCompute={hasCompute}
                instanceTypeCapacity={instanceTypeCapacity}
                keyPairs={keyPairs}
                placementGroups={placementGroups}
                register={register}
              />

              <NetworkSection
                control={control}
                defaultSubnet={defaultSubnet}
                effectiveSubnet={effectiveSubnet}
                effectiveVpcId={effectiveVpcId}
                errors={errors}
                otherSubnets={otherSubnets}
                register={register}
                securityGroups={securityGroups}
                selectedSubnetId={selectedSubnetId}
                setValue={setValue}
              />

              <RootVolumeSection
                control={control}
                errors={errors}
                images={images}
                register={register}
              />
            </>
          )}

          <CliCommandPanel
            commands={buildRunInstancesCommands(values, effectiveVpcId)}
          />

          <FormActions
            error={createMutation.error}
            errorMsg="Failed to create instance"
            isPending={createMutation.isPending}
            isSubmitting={isSubmitting}
            onCancel={() => {
              onClose()
            }}
            pendingLabel="Creating…"
            submitDisabled={!hasCompute && !selectedTemplateId}
            submitLabel="Create Instance"
          />
        </FormLock>
      </form>
    </>
  )
}
