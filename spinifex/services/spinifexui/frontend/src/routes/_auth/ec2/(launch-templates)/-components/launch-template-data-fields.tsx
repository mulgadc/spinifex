import { useSuspenseQuery } from "@tanstack/react-query"
import { ChevronDown } from "lucide-react"
import type { Control, FieldErrors, UseFormRegister } from "react-hook-form"
import { Controller, useWatch } from "react-hook-form"

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
import { Textarea } from "@/components/ui/textarea"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import { type LaunchTemplateFormData, VOLUME_TYPES } from "@/types/ec2"

const DEFAULT_VOLUME_TYPE = "gp3"
const MIN_VOLUME_SIZE_GIB = 1
const MAX_VOLUME_SIZE_GIB = 16_384

interface LaunchTemplateDataFieldsProps {
  control: Control<LaunchTemplateFormData>
  register: UseFormRegister<LaunchTemplateFormData>
  errors: FieldErrors<LaunchTemplateFormData>
}

// LaunchTemplateDataFields renders the launch-data subset shared by the
// create-template and create-version forms: image, instance type, key pair,
// subnet, security groups, user data and the root volume.
export function LaunchTemplateDataFields({
  control,
  register,
  errors,
}: LaunchTemplateDataFieldsProps) {
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)
  const { data: instanceTypesData } = useSuspenseQuery(
    ec2InstanceTypesQueryOptions,
  )
  const { data: keyPairsData } = useSuspenseQuery(ec2KeyPairsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: sgData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)

  const images = imagesData.Images ?? []
  const keyPairs = keyPairsData.KeyPairs ?? []
  const subnets = subnetsData.Subnets ?? []
  const securityGroups = sgData.SecurityGroups ?? []
  const instanceTypes = [
    ...new Set(
      (instanceTypesData.InstanceTypes ?? [])
        .map((t) => t.InstanceType)
        .filter((t): t is NonNullable<typeof t> => Boolean(t)),
    ),
  ].toSorted()

  const selectedSubnetId = useWatch({ control, name: "subnetId" })
  const effectiveVpcId = subnets.find(
    (s) => s.SubnetId === selectedSubnetId,
  )?.VpcId
  const vpcSecurityGroups = effectiveVpcId
    ? securityGroups.filter((sg) => sg.VpcId === effectiveVpcId)
    : securityGroups

  return (
    <>
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
                    <SelectItem key={image.ImageId} value={image.ImageId ?? ""}>
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
                {instanceTypes.map((type) => (
                  <SelectItem key={type} value={type}>
                    {type}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
        />
        <FieldError errors={[errors.instanceType]} />
      </Field>

      <Field>
        <FieldTitle>
          <label htmlFor="keyName">Key Pair</label>
        </FieldTitle>
        <Controller
          control={control}
          name="keyName"
          render={({ field }) => (
            <Select
              onValueChange={(value) =>
                field.onChange(value === "none" ? undefined : value)
              }
              value={field.value ?? "none"}
            >
              <SelectTrigger className="w-full" id="keyName">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">none</SelectItem>
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
      </Field>

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

      <Field>
        <FieldTitle>Security Groups</FieldTitle>
        <FieldDescription>
          {effectiveVpcId
            ? `Applies to VPC ${effectiveVpcId}`
            : "Showing all security groups — select a subnet to filter by VPC"}
        </FieldDescription>
        <Controller
          control={control}
          name="securityGroupIds"
          render={({ field }) => {
            const selected = field.value ?? []
            const toggle = (sgId: string) => {
              field.onChange(
                selected.includes(sgId)
                  ? selected.filter((id) => id !== sgId)
                  : [...selected, sgId],
              )
            }
            if (vpcSecurityGroups.length === 0) {
              return (
                <p className="text-xs text-muted-foreground">
                  No security groups available.
                </p>
              )
            }
            return (
              <div className="space-y-1">
                {vpcSecurityGroups.map((sg) => (
                  <label
                    className="flex items-center gap-2 text-xs"
                    key={sg.GroupId}
                  >
                    <input
                      aria-label={`Security group ${sg.GroupId} (${sg.GroupName})`}
                      checked={selected.includes(sg.GroupId ?? "")}
                      onChange={() => toggle(sg.GroupId ?? "")}
                      type="checkbox"
                    />
                    <span className="font-mono">
                      {sg.GroupId} ({sg.GroupName})
                    </span>
                  </label>
                ))}
              </div>
            )
          }}
        />
      </Field>

      <Field>
        <FieldTitle>
          <label htmlFor="userData">User Data</label>
        </FieldTitle>
        <FieldDescription>
          Cloud-init or shell script run at first boot.
        </FieldDescription>
        <Textarea
          className="min-h-32 font-mono"
          id="userData"
          placeholder="#!/bin/bash&#10;echo hello"
          {...register("userData")}
        />
      </Field>

      <Collapsible>
        <CollapsibleTrigger
          className="group flex h-7 w-full items-center justify-between rounded-md border border-input bg-input/20 px-2 py-0.5 text-sm transition-colors outline-none hover:bg-input/40 focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30 md:text-xs/relaxed dark:bg-input/30 dark:hover:bg-input/50"
          render={
            <button
              aria-label="Block Device Mappings (root volume)"
              type="button"
            />
          }
        >
          <span>Block Device Mappings (root volume)</span>
          <ChevronDown className="size-3.5 text-muted-foreground transition-transform group-data-[panel-open]:rotate-180" />
        </CollapsibleTrigger>
        <CollapsibleContent className="mt-3 space-y-4 rounded-md border border-border p-4">
          <Field>
            <FieldTitle>
              <label htmlFor="rootVolumeSize">Volume Size (GiB)</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.rootVolumeSize}
              id="rootVolumeSize"
              max={MAX_VOLUME_SIZE_GIB}
              min={MIN_VOLUME_SIZE_GIB}
              placeholder="use AMI default"
              type="number"
              {...register("rootVolumeSize", {
                setValueAs: (value: string) =>
                  value === "" || value === null || value === undefined
                    ? undefined
                    : Number(value),
              })}
            />
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
        </CollapsibleContent>
      </Collapsible>
    </>
  )
}
