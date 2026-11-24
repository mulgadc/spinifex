import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import {
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { PanelSection } from "@/components/detail-panel"
import { subnetSwitchItems } from "@/components/elbv2/subnets-editor"
import { FormActions } from "@/components/form-actions"
import { SelectField, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { PickerNoticeText } from "@/components/picker-notice"
import { SwitchList } from "@/components/switch-list"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { VpcSelectField } from "@/components/vpc-select-field"
import { nameTagSpecificationArg, onePerZone } from "@/lib/transit-gateway"
import { getNameTag } from "@/lib/utils"
import { useCreateTransitGatewayVpcAttachment } from "@/mutations/ec2"
import {
  ec2SubnetsQueryOptions,
  ec2TransitGatewaysQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  type CreateTransitGatewayVpcAttachmentFormData,
  createTransitGatewayVpcAttachmentSchema,
} from "@/types/ec2"

interface CreateTransitGatewayVpcAttachmentPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateTransitGatewayVpcAttachmentPage({
  onClose,
}: CreateTransitGatewayVpcAttachmentPageProps) {
  const { data: tgwData } = useSuspenseQuery(ec2TransitGatewaysQueryOptions)
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const createMutation = useCreateTransitGatewayVpcAttachment()

  // A pending gateway refuses attachments until it is available.
  const gateways = (tgwData.TransitGateways ?? []).filter(
    (tgw) => tgw.State === "available",
  )

  const vpcs = vpcsData.Vpcs ?? []

  const {
    control,
    handleSubmit,
    register,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm<CreateTransitGatewayVpcAttachmentFormData>({
    resolver: zodResolver(createTransitGatewayVpcAttachmentSchema),
    defaultValues: {
      name: "",
      transitGatewayId: "",
      vpcId: "",
      subnetIds: [],
    },
  })

  const values = useWatch({ control })

  const vpcSubnets = (subnetsData.Subnets ?? []).filter(
    (subnet) => subnet.VpcId === values.vpcId,
  )

  const selectedSubnets = (values.subnetIds ?? []).filter(
    (id): id is string => id !== undefined,
  )

  const onSubmit = async (data: CreateTransitGatewayVpcAttachmentFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync({
        transitGatewayId: data.transitGatewayId,
        vpcId: data.vpcId,
        subnetIds: data.subnetIds,
        name: data.name?.trim() || undefined,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.TransitGatewayVpcAttachment?.TransitGatewayAttachmentId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <TextField
          id="name"
          label="Name (optional)"
          placeholder="my-vpc-attachment"
          {...register("name")}
        />

        <SelectField
          control={control}
          id="transitGatewayId"
          label="Transit gateway"
          name="transitGatewayId"
          notice={
            gateways.length === 0
              ? {
                  tone: "muted",
                  text: "No transit gateway is available. Create one first.",
                }
              : undefined
          }
          options={gateways.map((tgw) => {
            const id = tgw.TransitGatewayId ?? ""
            const name = getNameTag(tgw.Tags)

            return { value: id, label: name ? `${id} (${name})` : id }
          })}
          placeholder="Select a transit gateway"
        />

        <PanelSection title="VPC">
          <div className="space-y-6">
            <VpcSelectField
              control={control}
              id="vpcId"
              name="vpcId"
              onValueChange={(vpcId) => {
                // A subnet belongs to one VPC, so a new VPC clears them.
                setValue("vpcId", vpcId, { shouldValidate: true })
                setValue("subnetIds", [])
              }}
              placeholder="Select a VPC"
              vpcs={vpcs}
            />

            <Field>
              <FieldTitle>Subnets</FieldTitle>
              {values.vpcId ? (
                <SwitchList
                  emptyText="No subnets in the selected VPC."
                  itemLabel="Subnet"
                  items={subnetSwitchItems(vpcSubnets)}
                  onChange={(next) => {
                    setValue(
                      "subnetIds",
                      onePerZone(next, selectedSubnets, vpcSubnets),
                      { shouldValidate: true },
                    )
                  }}
                  selected={selectedSubnets}
                />
              ) : (
                <PickerNoticeText
                  notice={{
                    tone: "muted",
                    text: "Select a VPC to choose its subnets.",
                  }}
                />
              )}
              <FieldDescription>
                One subnet in each Availability Zone. Traffic reaches the
                gateway only from the zones you choose.
              </FieldDescription>
              <FieldError errors={[errors.subnetIds]} />
            </Field>
          </div>
        </PanelSection>

        <CliCommandPanel
          commands={buildCreateAttachmentCommands(values, selectedSubnets)}
        />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create VPC attachment"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitDisabled={gateways.length === 0}
          submitLabel="Create VPC attachment"
        />
      </FormLock>
    </form>
  )
}

function buildCreateAttachmentCommands(
  values: DeepPartialSkipArrayKey<CreateTransitGatewayVpcAttachmentFormData>,
  subnetIds: readonly string[],
): CliCommand[] {
  const tagName = values.name?.trim()

  return [
    {
      label: "Create VPC attachment",
      parts: [
        {
          type: "bin",
          value:
            "AWS_PROFILE=spinifex aws ec2 create-transit-gateway-vpc-attachment",
        },
        ...commandFlag(
          "\\\n  --transit-gateway-id",
          cliPlaceholder(values.transitGatewayId ?? "", "TransitGatewayId"),
        ),
        ...commandFlag(
          "\\\n  --vpc-id",
          cliPlaceholder(values.vpcId ?? "", "VpcId"),
        ),
        ...commandFlag(
          "\\\n  --subnet-ids",
          cliPlaceholder(subnetIds.join(" "), "SubnetIds"),
        ),
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName
            ? nameTagSpecificationArg("transit-gateway-attachment", tagName)
            : undefined,
        ),
      ],
    },
  ]
}
