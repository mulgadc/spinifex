import type { Subnet } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { TagsFieldArray } from "@/components/tags-field-array"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { getNameTag } from "@/lib/utils"
import { useCreateDBSubnetGroup } from "@/mutations/rds"
import { ec2SubnetsQueryOptions, ec2VpcsQueryOptions } from "@/queries/ec2"
import {
  type CreateDBSubnetGroupFormData,
  createDBSubnetGroupSchema,
  MAX_SUBNETS_PER_GROUP,
} from "@/types/rds"

function subnetLabel(subnet: Subnet): string {
  const name = getNameTag(subnet.Tags)
  const parts = [subnet.CidrBlock, subnet.AvailabilityZone, name].filter(
    Boolean,
  )
  return `${subnet.SubnetId} (${parts.join(", ")})`
}

export function CreateDBSubnetGroupPage() {
  const navigate = useNavigate()
  const createGroup = useCreateDBSubnetGroup()
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<CreateDBSubnetGroupFormData>({
    resolver: zodResolver(createDBSubnetGroupSchema),
    defaultValues: {
      dbSubnetGroupName: "",
      dbSubnetGroupDescription: "",
      subnetIds: [],
      tags: [],
    },
  })

  const values = useWatch({ control })
  const selectedSubnets = values.subnetIds ?? []
  const selectedSubnetSet = new Set(selectedSubnets)

  const subnets = subnetsData.Subnets ?? []
  const vpcs = vpcsData.Vpcs ?? []

  // resolveGroupSubnets refuses a group spanning two VPCs, so once one subnet
  // is chosen the rest of the account's VPCs go out of reach rather than
  // staying selectable and failing at submit.
  const pinnedVpc = subnets.find(
    (s) => s.SubnetId === selectedSubnets[0],
  )?.VpcId

  const vpcIds = [...new Set(subnets.map((s) => s.VpcId ?? "").filter(Boolean))]

  const toggleSubnet = (subnetId: string) => {
    const next = selectedSubnets.includes(subnetId)
      ? selectedSubnets.filter((id) => id !== subnetId)
      : [...selectedSubnets, subnetId]
    setValue("subnetIds", next, { shouldValidate: true })
  }

  const onSubmit = async (data: CreateDBSubnetGroupFormData) => {
    await createGroup.mutateAsync(data)
    await navigate({ to: "/rds/describe-db-subnet-groups" })
  }

  return (
    <>
      <BackLink to="/rds/describe-db-subnet-groups">
        Back to subnet groups
      </BackLink>
      <PageHeading title="Create DB Subnet Group" />

      {createGroup.error && (
        <ErrorBanner
          error={createGroup.error}
          msg="Failed to create the DB subnet group"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="subnet-group-name">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.dbSubnetGroupName}
            id="subnet-group-name"
            placeholder="orders-db-subnets"
            {...register("dbSubnetGroupName")}
          />
          <FieldDescription>
            Letters, digits and hyphens, starting with a letter. The name cannot
            be changed later, and there is no ModifyDBSubnetGroup — editing a
            group means deleting and recreating it.
          </FieldDescription>
          <FieldError errors={[errors.dbSubnetGroupName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="subnet-group-description">Description</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.dbSubnetGroupDescription}
            id="subnet-group-description"
            placeholder="Private subnets for the orders database"
            {...register("dbSubnetGroupDescription")}
          />
          <FieldError errors={[errors.dbSubnetGroupDescription]} />
        </Field>

        <Field>
          <FieldTitle>Subnets</FieldTitle>
          {subnets.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No subnets in this account. Create one before creating a subnet
              group.
            </p>
          ) : (
            <div className="space-y-4">
              {vpcIds.map((vpcId) => {
                const vpcName = getNameTag(
                  vpcs.find((v) => v.VpcId === vpcId)?.Tags,
                )
                const unreachable =
                  pinnedVpc !== undefined && pinnedVpc !== vpcId
                return (
                  <div key={vpcId}>
                    <p className="mb-1 font-mono text-xs text-muted-foreground">
                      {vpcId}
                      {vpcName && ` (${vpcName})`}
                      {unreachable &&
                        " — a subnet group must span one VPC, so this one is out of reach"}
                    </p>
                    <div className="space-y-1 pl-3">
                      {subnets
                        .filter((s) => s.VpcId === vpcId)
                        .map((subnet) => (
                          <label
                            className="flex items-center gap-2 text-xs"
                            key={subnet.SubnetId}
                          >
                            <input
                              aria-label={`Subnet ${subnetLabel(subnet)}`}
                              checked={selectedSubnetSet.has(
                                subnet.SubnetId ?? "",
                              )}
                              disabled={unreachable}
                              onChange={() =>
                                toggleSubnet(subnet.SubnetId ?? "")
                              }
                              type="checkbox"
                            />
                            <span className="font-mono">
                              {subnetLabel(subnet)}
                            </span>
                          </label>
                        ))}
                    </div>
                  </div>
                )
              })}
            </div>
          )}
          <FieldDescription>
            At most {MAX_SUBNETS_PER_GROUP} subnets, all in one VPC. This
            platform is single-AZ, so the two-AZ rule AWS enforces does not
            apply here.
          </FieldDescription>
          <FieldError errors={[errors.subnetIds]} />
        </Field>

        <TagsFieldArray control={control} name="tags" />

        <CliCommandPanel
          commands={buildCreateSubnetGroupCommands({
            dbSubnetGroupName: values.dbSubnetGroupName ?? "",
            dbSubnetGroupDescription: values.dbSubnetGroupDescription ?? "",
            subnetIds: selectedSubnets,
          })}
        />

        <FormActions
          isPending={createGroup.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/rds/describe-db-subnet-groups" })
          }
          pendingLabel="Creating…"
          submitLabel="Create Subnet Group"
        />
      </form>
    </>
  )
}

interface SubnetGroupCliValues {
  dbSubnetGroupName: string
  dbSubnetGroupDescription: string
  subnetIds: string[]
}

function buildCreateSubnetGroupCommands(
  values: SubnetGroupCliValues,
): CliCommand[] {
  return [
    {
      label: "Create DB Subnet Group",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws rds create-db-subnet-group",
        },
        ...commandFlag(
          "--db-subnet-group-name",
          cliPlaceholder(values.dbSubnetGroupName, "DBSubnetGroupName"),
        ),
        ...commandFlag(
          "--db-subnet-group-description",
          `"${cliPlaceholder(
            values.dbSubnetGroupDescription,
            "DBSubnetGroupDescription",
          )}"`,
        ),
        ...commandFlag(
          "--subnet-ids",
          cliPlaceholder(values.subnetIds.join(" "), "SubnetIds"),
        ),
      ],
    },
  ]
}
