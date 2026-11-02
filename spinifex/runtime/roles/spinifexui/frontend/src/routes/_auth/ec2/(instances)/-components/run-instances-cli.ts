import type { DeepPartialSkipArrayKey } from "react-hook-form"

import {
  type CliCommand,
  captureId,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CommandPart,
} from "@/components/cli-command-panel"
import type { CreateInstanceFormData } from "@/types/ec2"

import { DEFAULT_ROOT_DEVICE_NAME } from "./run-instances-helpers"

type RunInstancesValues = DeepPartialSkipArrayKey<CreateInstanceFormData>

function runInstancesBin(): CommandPart {
  return {
    type: "bin",
    value: "AWS_PROFILE=spinifex aws ec2 run-instances",
  }
}

function countParts(values: RunInstancesValues): CommandPart[] {
  return commandFlag("\\\n  --count", (values.count ?? 0) || 1)
}

function launchTemplateCommand(values: RunInstancesValues): CliCommand {
  const templateVersion = values.launchTemplateVersion || "$Default"

  return {
    label: "Run Instances",
    parts: [
      runInstancesBin(),
      ...commandFlag(
        "\\\n  --launch-template",
        `LaunchTemplateId=${values.launchTemplateId},Version=${templateVersion}`,
      ),
      ...countParts(values),
    ],
  }
}

function directLaunchParts(values: RunInstancesValues): CommandPart[] {
  return [
    runInstancesBin(),
    ...commandFlag(
      "\\\n  --image-id",
      cliPlaceholder(values.imageId ?? "", "ImageId"),
    ),
    ...commandFlag(
      "\\\n  --instance-type",
      cliPlaceholder(values.instanceType ?? "", "InstanceType"),
    ),
    ...commandFlag(
      "\\\n  --key-name",
      cliPlaceholder(values.keyName ?? "", "KeyName"),
    ),
    ...optionalFlag("\\\n  --subnet-id", values.subnetId),
    ...optionalFlag(
      "\\\n  --placement",
      values.placementGroupName && `GroupName=${values.placementGroupName}`,
    ),
    ...countParts(values),
  ]
}

function createSecurityGroupCommand(
  values: RunInstancesValues,
  vpcId: string | undefined,
): CliCommand {
  return {
    label: "Create security group",
    parts: captureId(
      "SG_ID",
      "AWS_PROFILE=spinifex aws ec2 create-security-group",
      [
        ...commandFlag(
          "\\\n  --group-name",
          cliPlaceholder(values.newSgName ?? "", "name"),
        ),
        ...commandFlag(
          "\\\n  --description",
          `'${values.newSgDescription || "Created by launch wizard"}'`,
        ),
        ...commandFlag("\\\n  --vpc-id", vpcId ?? "<VpcId>"),
      ],
      "GroupId",
    ),
  }
}

function authorizeIngressCommand(
  values: RunInstancesValues,
): CliCommand | undefined {
  const cidr =
    values.ruleSource === "custom" && values.customCidr
      ? values.customCidr
      : "0.0.0.0/0"

  const ports = [
    ...(values.allowSsh === true ? [22] : []),
    ...(values.allowHttp === true ? [80] : []),
    ...(values.allowHttps === true ? [443] : []),
  ]

  if (ports.length === 0) {
    return undefined
  }

  const ipPerms = JSON.stringify(
    ports.map((port) => ({
      IpProtocol: "tcp",
      FromPort: port,
      ToPort: port,
      IpRanges: [{ CidrIp: cidr }],
    })),
  )

  return {
    label: "Authorize inbound rules",
    parts: [
      {
        type: "bin",
        value: "AWS_PROFILE=spinifex aws ec2 authorize-security-group-ingress",
      },
      ...commandFlag("\\\n  --group-id", "$SG_ID"),
      ...commandFlag("\\\n  --ip-permissions", `'${ipPerms}'`),
    ],
  }
}

interface SecurityGroupCli {
  commands: CliCommand[]
  parts: CommandPart[]
}

// Create-new emits create + authorize commands and feeds $SG_ID into
// run-instances; select-existing passes the chosen IDs.
function securityGroupCommands(
  values: RunInstancesValues,
  vpcId: string | undefined,
): SecurityGroupCli {
  if ((values.securityGroupMode || "create") !== "create") {
    return {
      commands: [],
      parts: optionalFlag(
        "\\\n  --security-group-ids",
        values.securityGroupIds?.join(" "),
      ),
    }
  }

  const authorize = authorizeIngressCommand(values)

  return {
    commands: [
      createSecurityGroupCommand(values, vpcId),
      ...(authorize ? [authorize] : []),
    ],
    parts: commandFlag("\\\n  --security-group-ids", "$SG_ID"),
  }
}

function blockDeviceMappingParts(values: RunInstancesValues): CommandPart[] {
  const { rootDeleteOnTermination, rootVolumeSize, rootVolumeType } = values

  const hasStorageOverride =
    rootVolumeSize !== undefined ||
    rootVolumeType !== undefined ||
    rootDeleteOnTermination !== undefined

  if (!hasStorageOverride) {
    return []
  }

  // A field the form left unset stays undefined, which JSON.stringify omits,
  // so the CLI preview shows only the overrides the user actually set.
  const ebs = {
    VolumeSize: rootVolumeSize,
    VolumeType: rootVolumeType || undefined,
    DeleteOnTermination: rootDeleteOnTermination,
  }

  const bdm = JSON.stringify([
    {
      DeviceName: values.rootDeviceName || DEFAULT_ROOT_DEVICE_NAME,
      Ebs: ebs,
    },
  ])

  return commandFlag("\\\n  --block-device-mappings", `'${bdm}'`)
}

export function buildRunInstancesCommands(
  values: RunInstancesValues,
  vpcId: string | undefined,
): CliCommand[] {
  if (values.launchTemplateId) {
    return [launchTemplateCommand(values)]
  }

  const sg = securityGroupCommands(values, vpcId)

  const parts: CommandPart[] = [
    ...directLaunchParts(values),
    ...sg.parts,
    ...blockDeviceMappingParts(values),
  ]

  return [...sg.commands, { label: "Run Instances", parts }]
}
