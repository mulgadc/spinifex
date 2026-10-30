import type { Instance, IpPermission, SecurityGroup } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { InfoList } from "@/components/info-tip"
import { useAdmin } from "@/hooks/use-admin"
import { formatDateTime } from "@/lib/utils"
import { adminVMsQueryOptions } from "@/queries/admin"
import {
  ec2AddressesQueryOptions,
  ec2ImageQueryOptions,
  ec2SecurityGroupsQueryOptions,
} from "@/queries/ec2"

import { GpuSection } from "../../-components/gpu-section"
import { IamRolePanel } from "../../-components/iam-role-panel"
import { SSHAccess } from "../../-components/ssh-access"

const LINK = "text-primary hover:underline"

// "TCP 22", "UDP 1000–2000", "All traffic", in the AWS console's words.
export function describeRulePorts(rule: IpPermission): string {
  const protocol = rule.IpProtocol ?? ""

  if (protocol === "-1") {
    return "All traffic"
  }

  const name = { "6": "TCP", "17": "UDP", "1": "ICMP" }[protocol]
  const label = name ?? protocol.toUpperCase()

  if (rule.FromPort === undefined || rule.FromPort === -1) {
    return label
  }

  return rule.FromPort === rule.ToPort
    ? `${label} ${rule.FromPort}`
    : `${label} ${rule.FromPort}–${rule.ToPort}`
}

export function describeRuleSources(rule: IpPermission): string {
  const sources = [
    ...(rule.IpRanges ?? []).map((range) => range.CidrIp),
    ...(rule.Ipv6Ranges ?? []).map((range) => range.CidrIpv6),
    ...(rule.UserIdGroupPairs ?? []).map((pair) => pair.GroupId),
    ...(rule.PrefixListIds ?? []).map((list) => list.PrefixListId),
  ].filter(Boolean)

  return sources.length ? sources.join(", ") : "—"
}

function RuleRows({
  direction,
  rules,
}: {
  direction: string
  rules: IpPermission[] | undefined
}) {
  if (!rules?.length) {
    return (
      <PanelField label={direction}>
        <span className="text-muted-foreground">No rules</span>
      </PanelField>
    )
  }

  return rules.map((rule) => {
    const ports = describeRulePorts(rule)
    const sources = describeRuleSources(rule)

    return (
      <PanelField key={`${direction}${ports}${sources}`} label={direction} mono>
        <span title={sources}>
          {ports} · {sources}
        </span>
      </PanelField>
    )
  })
}

function SecurityGroups({ instance }: { instance: Instance }) {
  const { data } = useQuery(ec2SecurityGroupsQueryOptions)
  const ids = new Set(instance.SecurityGroups?.map((group) => group.GroupId))

  const groups: SecurityGroup[] =
    data?.SecurityGroups?.filter((group) => ids.has(group.GroupId)) ??
    instance.SecurityGroups ??
    []

  if (!groups.length) {
    return null
  }

  return (
    <PanelSection title="Security groups">
      <div className="flex flex-col gap-3">
        {groups.map((group) => (
          <PanelFields key={group.GroupId}>
            <PanelField label="Group">
              {group.GroupId && (
                <Link
                  className={LINK}
                  params={{ id: group.GroupId }}
                  title={group.GroupId}
                  to="/ec2/describe-security-groups/$id"
                >
                  {group.GroupName ?? group.GroupId}
                </Link>
              )}
            </PanelField>
            {data && (
              <>
                <RuleRows direction="Inbound" rules={group.IpPermissions} />
                <RuleRows
                  direction="Outbound"
                  rules={group.IpPermissionsEgress}
                />
              </>
            )}
          </PanelFields>
        ))}
      </div>
    </PanelSection>
  )
}

function NetworkInterfaces({ instance }: { instance: Instance }) {
  const interfaces = instance.NetworkInterfaces ?? []

  if (!interfaces.length) {
    return null
  }

  return (
    <PanelSection defaultOpen={false} title="Network interfaces">
      <div className="flex flex-col gap-3">
        {interfaces.map((eni) => (
          <PanelFields key={eni.NetworkInterfaceId}>
            <PanelField copy={eni.NetworkInterfaceId} label="Interface" mono>
              {eni.NetworkInterfaceId}
            </PanelField>
            <PanelField label="Device index" mono>
              {eni.Attachment?.DeviceIndex?.toString()}
            </PanelField>
            <PanelField label="Private IP" mono>
              {eni.PrivateIpAddress}
            </PanelField>
            <PanelField label="Public IP" mono>
              {eni.Association?.PublicIp}
            </PanelField>
            <PanelField label="MAC address" mono>
              {eni.MacAddress}
            </PanelField>
            <PanelField label="Subnet" mono>
              {eni.SubnetId && (
                <Link
                  className={LINK}
                  params={{ id: eni.SubnetId }}
                  to="/ec2/describe-subnets/$id"
                >
                  {eni.SubnetId}
                </Link>
              )}
            </PanelField>
            <PanelField label="Status">{eni.Status}</PanelField>
          </PanelFields>
        ))}
      </div>
    </PanelSection>
  )
}

export function InstanceOverview({ instance }: { instance: Instance }) {
  const { isAdmin } = useAdmin()
  const { data: imageData } = useQuery(ec2ImageQueryOptions(instance.ImageId))
  const { data: addressData } = useQuery(ec2AddressesQueryOptions)

  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    enabled: isAdmin,
  })

  const image = imageData?.Images?.[0]

  const elasticIp = addressData?.Addresses?.find(
    (address) => address.InstanceId === instance.InstanceId,
  )

  const gpus = vmsData?.vms?.find(
    (vm) => vm.instance_id === instance.InstanceId,
  )?.gpus

  return (
    <>
      <PanelSection title="Instance information">
        <PanelFields>
          <PanelField copy={instance.InstanceId} label="Instance ID" mono>
            {instance.InstanceId}
          </PanelField>
          <PanelField label="Type" mono>
            {instance.InstanceType}
          </PanelField>
          <PanelField label="Zone">
            {instance.Placement?.AvailabilityZone}
          </PanelField>
          <PanelField label="Placement group">
            {instance.Placement?.GroupName && (
              <Link className={LINK} to="/ec2/describe-placement-groups">
                {instance.Placement.GroupName}
              </Link>
            )}
          </PanelField>
          <PanelField
            info={
              image && (
                <InfoList
                  items={[
                    ["AMI", image.ImageId],
                    ["Name", image.Name],
                    ["Description", image.Description],
                    ["Architecture", image.Architecture],
                    ["Boot mode", image.BootMode],
                    ["Platform", image.PlatformDetails],
                    [
                      "Created",
                      image.CreationDate && formatDateTime(image.CreationDate),
                    ],
                  ]}
                />
              )
            }
            label="Image"
            mono
          >
            {instance.ImageId && (
              <Link
                className={LINK}
                params={{ id: instance.ImageId }}
                title={image?.Name}
                to="/ec2/describe-images/$id"
              >
                {image?.Name ?? instance.ImageId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Key pair">{instance.KeyName}</PanelField>
          <PanelField label="Launched">
            {instance.LaunchTime && formatDateTime(instance.LaunchTime)}
          </PanelField>
          {instance.State?.Name === "running" && (
            <SSHAccess image={image} instance={instance} />
          )}
        </PanelFields>
      </PanelSection>
      <PanelSection title="Networking">
        <PanelFields>
          <PanelField copy={instance.PrivateIpAddress} label="Private IP" mono>
            {instance.PrivateIpAddress}
          </PanelField>
          <PanelField copy={instance.PublicIpAddress} label="Public IP" mono>
            {instance.PublicIpAddress}
          </PanelField>
          <PanelField
            info="A static public address the account owns. It stays with the account when the instance stops or is terminated."
            label="Elastic IP"
            mono
          >
            {elasticIp?.AllocationId && (
              <Link
                className={LINK}
                params={{ id: elasticIp.AllocationId }}
                title={elasticIp.AllocationId}
                to="/ec2/describe-addresses/$id"
              >
                {elasticIp.PublicIp}
              </Link>
            )}
          </PanelField>
          <PanelField copy={instance.PrivateDnsName} label="Private DNS" mono>
            {instance.PrivateDnsName}
          </PanelField>
          <PanelField copy={instance.PublicDnsName} label="Public DNS" mono>
            {instance.PublicDnsName}
          </PanelField>
          <PanelField label="VPC" mono>
            {instance.VpcId && (
              <Link
                className={LINK}
                params={{ id: instance.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {instance.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Subnet" mono>
            {instance.SubnetId && (
              <Link
                className={LINK}
                params={{ id: instance.SubnetId }}
                to="/ec2/describe-subnets/$id"
              >
                {instance.SubnetId}
              </Link>
            )}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <SecurityGroups instance={instance} />
      <NetworkInterfaces instance={instance} />
      {(instance.BlockDeviceMappings?.length ?? 0) > 0 && (
        <PanelSection title="Storage">
          <PanelFields>
            {instance.BlockDeviceMappings?.map((device) => (
              <PanelField
                key={device.DeviceName}
                label={device.DeviceName ?? "—"}
                mono
              >
                {device.Ebs?.VolumeId && (
                  <Link
                    className={LINK}
                    params={{ id: device.Ebs.VolumeId }}
                    to="/ec2/describe-volumes/$id"
                  >
                    {device.Ebs.VolumeId}
                  </Link>
                )}
              </PanelField>
            ))}
          </PanelFields>
        </PanelSection>
      )}
      {instance.InstanceId && <IamRolePanel instanceId={instance.InstanceId} />}
      {isAdmin && <GpuSection gpus={gpus} />}
    </>
  )
}
