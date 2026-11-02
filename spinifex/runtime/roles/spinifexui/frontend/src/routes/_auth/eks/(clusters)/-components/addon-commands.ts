import {
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"

export interface AddonCommandValues {
  addonVersion: string
  serviceAccountRoleArn: string
  configurationValues: string
}

// create-addon and update-addon take the same flags, and the console sends an
// unset field as absent rather than empty, so the command omits it too.
export function buildAddonCommands(
  subcommand: "create-addon" | "update-addon",
  label: string,
  clusterName: string,
  addonName: string,
  values: AddonCommandValues,
): CliCommand[] {
  const config = values.configurationValues.trim()

  return [
    {
      label,
      parts: [
        { type: "bin", value: `AWS_PROFILE=spinifex aws eks ${subcommand}` },
        ...commandFlag("\\\n  --cluster-name", clusterName),
        ...commandFlag(
          "\\\n  --addon-name",
          cliPlaceholder(addonName, "addon-name"),
        ),
        ...optionalFlag("\\\n  --addon-version", values.addonVersion),
        ...optionalFlag(
          "\\\n  --service-account-role-arn",
          values.serviceAccountRoleArn.trim(),
        ),
        ...optionalFlag(
          "\\\n  --configuration-values",
          config && shellSingleQuote(config),
        ),
      ],
    },
  ]
}
