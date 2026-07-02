import { useQuery } from "@tanstack/react-query"
import { useState } from "react"

import { ErrorBanner } from "@/components/error-banner"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useProvisionCapacity } from "@/mutations/ecs"
import {
  ec2KeyPairsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"

interface ProvisionCapacityDialogProps {
  clusterName: string
  open: boolean
  onOpenChange: (open: boolean) => void
}

const SELECT_CLASS =
  "h-7 w-full rounded-md border border-input bg-input/20 px-2 text-sm"

export function ProvisionCapacityDialog({
  clusterName,
  open,
  onOpenChange,
}: ProvisionCapacityDialogProps) {
  const provision = useProvisionCapacity()
  const { data: subnetsData } = useQuery(ec2SubnetsQueryOptions)
  const { data: sgData } = useQuery(ec2SecurityGroupsQueryOptions)
  const { data: keyPairsData } = useQuery(ec2KeyPairsQueryOptions)

  const subnets = subnetsData?.Subnets ?? []
  const securityGroups = sgData?.SecurityGroups ?? []
  const keyPairs = keyPairsData?.KeyPairs ?? []

  const [instanceType, setInstanceType] = useState("t3.small")
  const [count, setCount] = useState(1)
  const [subnetId, setSubnetId] = useState("")
  const [securityGroupId, setSecurityGroupId] = useState("")
  const [keyName, setKeyName] = useState("")

  function handleClose(nextOpen: boolean) {
    if (!nextOpen) {
      setInstanceType("t3.small")
      setCount(1)
      setSubnetId("")
      setSecurityGroupId("")
      setKeyName("")
      provision.reset()
    }
    onOpenChange(nextOpen)
  }

  async function handleProvision() {
    try {
      await provision.mutateAsync({
        Cluster: clusterName,
        InstanceType: instanceType || undefined,
        Count: count,
        SubnetID: subnetId,
        SecurityGroupID: securityGroupId,
        KeyName: keyName || undefined,
      })
      handleClose(false)
    } catch {
      // error shown via provision.error
    }
  }

  const disabled =
    provision.isPending || subnetId === "" || securityGroupId === ""

  return (
    <AlertDialog onOpenChange={handleClose} open={open}>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>Provision EC2 capacity</AlertDialogTitle>
          <AlertDialogDescription>
            Launch container instances into cluster &ldquo;{clusterName}&rdquo;.
            They register automatically once booted.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <div className="space-y-4">
          <Field>
            <FieldTitle>
              <label htmlFor="provision-instance-type">Instance type</label>
            </FieldTitle>
            <Input
              id="provision-instance-type"
              onChange={(e) => setInstanceType(e.target.value)}
              placeholder="t3.small"
              value={instanceType}
            />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="provision-count">Count</label>
            </FieldTitle>
            <Input
              id="provision-count"
              max={10}
              min={1}
              onChange={(e) => setCount(Number(e.target.value))}
              type="number"
              value={count}
            />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="provision-subnet">Subnet</label>
            </FieldTitle>
            <select
              className={SELECT_CLASS}
              id="provision-subnet"
              onChange={(e) => setSubnetId(e.target.value)}
              value={subnetId}
            >
              <option value="">Select a subnet</option>
              {subnets.map((s) => (
                <option key={s.SubnetId} value={s.SubnetId ?? ""}>
                  {s.SubnetId} ({s.CidrBlock}
                  {s.AvailabilityZone ? `, ${s.AvailabilityZone}` : ""})
                </option>
              ))}
            </select>
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="provision-sg">Security group</label>
            </FieldTitle>
            <select
              className={SELECT_CLASS}
              id="provision-sg"
              onChange={(e) => setSecurityGroupId(e.target.value)}
              value={securityGroupId}
            >
              <option value="">Select a security group</option>
              {securityGroups.map((sg) => (
                <option key={sg.GroupId} value={sg.GroupId ?? ""}>
                  {sg.GroupId} ({sg.GroupName})
                </option>
              ))}
            </select>
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="provision-key">Key pair</label>
            </FieldTitle>
            <select
              className={SELECT_CLASS}
              id="provision-key"
              onChange={(e) => setKeyName(e.target.value)}
              value={keyName}
            >
              <option value="">None</option>
              {keyPairs.map((kp) => (
                <option key={kp.KeyPairId} value={kp.KeyName ?? ""}>
                  {kp.KeyName}
                </option>
              ))}
            </select>
          </Field>
        </div>

        {provision.error && (
          <ErrorBanner
            error={
              provision.error instanceof Error ? provision.error : undefined
            }
            msg="Failed to provision capacity."
          />
        )}

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={disabled}
            onClick={(e) => {
              e.preventDefault()
              void handleProvision()
            }}
          >
            {provision.isPending ? "Provisioning…" : "Provision"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
