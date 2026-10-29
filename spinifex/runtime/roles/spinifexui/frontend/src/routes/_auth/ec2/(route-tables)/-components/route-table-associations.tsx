import type { RouteTable } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Link2, Unlink } from "lucide-react"
import { useState } from "react"
import { Controller, useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
import {
  useAssociateRouteTable,
  useDisassociateRouteTable,
} from "@/mutations/ec2"
import { ec2SubnetsQueryOptions } from "@/queries/ec2"
import {
  type AssociateRouteTableFormData,
  associateRouteTableSchema,
} from "@/types/ec2"

interface PendingRemoval {
  associationId: string
  subnetId: string
}

// Explicit subnet associations, with associate and disassociate. Only subnets
// in the table's VPC that it does not already hold are offered.
export function RouteTableAssociations({ rtb }: { rtb: RouteTable }) {
  const routeTableId = rtb.RouteTableId ?? ""
  const { data: subnetData } = useQuery(ec2SubnetsQueryOptions)
  const associate = useAssociateRouteTable()
  const disassociate = useDisassociateRouteTable()
  const [toRemove, setToRemove] = useState<PendingRemoval | null>(null)

  const form = useForm<AssociateRouteTableFormData>({
    resolver: zodResolver(associateRouteTableSchema),
    defaultValues: { subnetId: "" },
  })

  const associations = (rtb.Associations ?? []).filter((a) => a.SubnetId)
  const associated = new Set(associations.map((a) => a.SubnetId))

  const available = (subnetData?.Subnets ?? []).filter(
    (s) => s.VpcId === rtb.VpcId && !associated.has(s.SubnetId),
  )

  const error = associate.error ?? disassociate.error

  async function onDisassociate() {
    if (!toRemove) {
      return
    }

    try {
      await disassociate.mutateAsync(toRemove.associationId)
    } catch {
      // Shown by the banner above the form.
    } finally {
      setToRemove(null)
    }
  }

  async function onAssociate(values: AssociateRouteTableFormData) {
    await associate.mutateAsync({ routeTableId, subnetId: values.subnetId })
    form.reset()
  }

  return (
    <PanelSection title={`Subnet associations (${associations.length})`}>
      <div className="flex flex-col gap-4">
        {associations.length > 0 ? (
          <ul className="flex flex-col gap-2">
            {associations.map((assoc) => {
              const associationId = assoc.RouteTableAssociationId

              return (
                <li
                  className="flex items-center justify-between gap-3 rounded-md border px-2 py-1 text-sm"
                  key={associationId}
                >
                  <Link
                    className="truncate font-mono text-xs text-primary hover:underline"
                    params={{ id: assoc.SubnetId ?? "" }}
                    to="/ec2/describe-subnets/$id"
                  >
                    {assoc.SubnetId}
                  </Link>
                  <Button
                    aria-label={`Disassociate ${assoc.SubnetId}`}
                    disabled={!associationId || disassociate.isPending}
                    onClick={() => {
                      if (associationId) {
                        setToRemove({
                          associationId,
                          subnetId: assoc.SubnetId ?? associationId,
                        })
                      }
                    }}
                    size="icon-sm"
                    variant="ghost"
                  >
                    <Unlink />
                  </Button>
                </li>
              )
            })}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">
            No subnet associations.
          </p>
        )}

        {error && (
          <ErrorBanner error={error} msg="Failed to update association" />
        )}

        <form
          className="flex items-end gap-2"
          onSubmit={form.handleSubmit(onAssociate)}
        >
          <Field className="min-w-0 flex-1">
            <FieldTitle>
              <label htmlFor="subnetId">Associate subnet</label>
            </FieldTitle>
            <Controller
              control={form.control}
              name="subnetId"
              render={({ field }) => (
                <Select onValueChange={field.onChange} value={field.value}>
                  <SelectTrigger
                    aria-invalid={!!form.formState.errors.subnetId}
                    className="w-full"
                    id="subnetId"
                  >
                    <SelectValue placeholder="Select a subnet" />
                  </SelectTrigger>
                  <SelectContent>
                    {available.map((subnet) => {
                      const name = getNameTag(subnet.Tags)

                      return (
                        <SelectItem
                          key={subnet.SubnetId}
                          value={subnet.SubnetId ?? ""}
                        >
                          {`${subnet.SubnetId} (${name ?? subnet.CidrBlock})`}
                        </SelectItem>
                      )
                    })}
                  </SelectContent>
                </Select>
              )}
            />
            <FieldError errors={[form.formState.errors.subnetId]} />
          </Field>
          <Button disabled={associate.isPending} size="sm" type="submit">
            <Link2 data-icon="inline-start" />
            Associate
          </Button>
        </form>
      </div>

      <DeleteConfirmationDialog
        confirmLabel="Disassociate"
        description={`Disassociate "${toRemove?.subnetId}" from "${routeTableId}"? The subnet stops using this table's routes.`}
        isPending={disassociate.isPending}
        onConfirm={() => {
          void onDisassociate()
        }}
        onOpenChange={(open) => {
          if (!open) {
            setToRemove(null)
          }
        }}
        open={toRemove !== null}
        pendingLabel="Disassociating…"
        title="Disassociate subnet"
      />
    </PanelSection>
  )
}
