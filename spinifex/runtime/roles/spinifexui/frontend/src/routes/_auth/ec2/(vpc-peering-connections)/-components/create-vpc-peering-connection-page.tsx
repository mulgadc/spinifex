import type { Vpc } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Building2, User } from "lucide-react"
import {
  Controller,
  type Control,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import { ChoiceTile, ChoiceTiles } from "@/components/choice-tile"
import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
import { useCreateVpcPeeringConnection } from "@/mutations/ec2"
import { ec2VpcsQueryOptions } from "@/queries/ec2"
import {
  type CreateVpcPeeringConnectionFormData,
  createVpcPeeringConnectionSchema,
} from "@/types/ec2"

function vpcLabel(vpc: Vpc): string {
  const name = getNameTag(vpc.Tags)

  return `${vpc.VpcId} (${name ?? vpc.CidrBlock})`
}

function VpcSelect({
  control,
  name,
  vpcs,
  placeholder,
  invalid,
}: {
  control: Control<CreateVpcPeeringConnectionFormData>
  name: "vpcId" | "peerVpcId"
  vpcs: readonly Vpc[]
  placeholder: string
  invalid: boolean
}) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field }) => (
        <Select onValueChange={field.onChange} value={field.value}>
          <SelectTrigger aria-invalid={invalid} className="w-full" id={name}>
            <SelectValue placeholder={placeholder} />
          </SelectTrigger>
          <SelectContent>
            {vpcs.map((vpc) => (
              <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                {vpcLabel(vpc)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
    />
  )
}

interface CreateVpcPeeringConnectionPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateVpcPeeringConnectionPage({
  onClose,
}: CreateVpcPeeringConnectionPageProps) {
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const createMutation = useCreateVpcPeeringConnection()
  const vpcs = vpcsData.Vpcs ?? []

  const {
    control,
    handleSubmit,
    register,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm<CreateVpcPeeringConnectionFormData>({
    resolver: zodResolver(createVpcPeeringConnectionSchema),
    defaultValues: {
      name: "",
      vpcId: "",
      account: "self",
      peerOwnerId: "",
      peerVpcId: "",
    },
  })

  const values = useWatch({ control })
  const other = values.account === "other"

  const onSubmit = async (data: CreateVpcPeeringConnectionFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync({
        vpcId: data.vpcId,
        peerVpcId: data.peerVpcId,
        peerOwnerId: data.account === "other" ? data.peerOwnerId : undefined,
        name: data.name?.trim() ?? undefined,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.VpcPeeringConnection?.VpcPeeringConnectionId)
  }

  // Switching account clears the peer, since a VPC ID means nothing across.
  const chooseAccount = (account: "self" | "other") => {
    setValue("account", account)
    setValue("peerVpcId", "")
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="name">Name (optional)</label>
          </FieldTitle>
          <Input id="name" placeholder="my-peering" {...register("name")} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="vpcId">Requester VPC</label>
          </FieldTitle>
          <VpcSelect
            control={control}
            invalid={!!errors.vpcId}
            name="vpcId"
            placeholder="Select your VPC"
            vpcs={vpcs}
          />
          <FieldError errors={[errors.vpcId]} />
        </Field>

        <Field>
          <FieldTitle>Peer VPC account</FieldTitle>
          <ChoiceTiles columns={2} label="Peer VPC account">
            <ChoiceTile
              checked={!other}
              icon={<User aria-hidden className="size-4" />}
              label="My account"
              layout="inline"
              onSelect={() => {
                chooseAccount("self")
              }}
            />
            <ChoiceTile
              checked={other}
              icon={<Building2 aria-hidden className="size-4" />}
              label="Another account"
              layout="inline"
              onSelect={() => {
                chooseAccount("other")
              }}
            />
          </ChoiceTiles>
        </Field>

        {other && (
          <Field>
            <FieldTitle>
              <label htmlFor="peerOwnerId">Account ID</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.peerOwnerId}
              id="peerOwnerId"
              inputMode="numeric"
              placeholder="123456789012"
              {...register("peerOwnerId")}
            />
            <FieldError errors={[errors.peerOwnerId]} />
          </Field>
        )}

        <Field>
          <FieldTitle>
            <label htmlFor="peerVpcId">Accepter VPC</label>
          </FieldTitle>
          {other ? (
            <Input
              aria-invalid={!!errors.peerVpcId}
              id="peerVpcId"
              placeholder="vpc-0123456789abcdef0"
              {...register("peerVpcId")}
            />
          ) : (
            <VpcSelect
              control={control}
              invalid={!!errors.peerVpcId}
              name="peerVpcId"
              placeholder="Select the VPC to peer with"
              vpcs={vpcs.filter((vpc) => vpc.VpcId !== values.vpcId)}
            />
          )}
          <FieldError errors={[errors.peerVpcId]} />
          <p className="text-xs text-muted-foreground">
            {other
              ? "The other account accepts the request before traffic flows."
              : "You accept the request yourself once it is created."}{" "}
            The two VPCs&apos; CIDR blocks must not overlap.
          </p>
        </Field>

        <CliCommandPanel
          commands={buildCreateVpcPeeringConnectionCommands(values)}
        />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create peering connection"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create peering connection"
        />
      </FormLock>
    </form>
  )
}

function buildCreateVpcPeeringConnectionCommands(
  values: DeepPartialSkipArrayKey<CreateVpcPeeringConnectionFormData>,
): CliCommand[] {
  const tagName = values.name?.trim()
  const other = values.account === "other"

  return [
    {
      label: "Create peering connection",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-vpc-peering-connection",
        },
        ...commandFlag(
          "\\\n  --vpc-id",
          cliPlaceholder(values.vpcId ?? "", "VpcId"),
        ),
        ...commandFlag(
          "\\\n  --peer-vpc-id",
          cliPlaceholder(values.peerVpcId?.trim() ?? "", "PeerVpcId"),
        ),
        ...(other
          ? commandFlag(
              "\\\n  --peer-owner-id",
              cliPlaceholder(values.peerOwnerId ?? "", "PeerOwnerId"),
            )
          : []),
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName &&
            `'ResourceType=vpc-peering-connection,Tags=[{Key=Name,Value=${tagName}}]'`,
        ),
      ],
    },
  ]
}
