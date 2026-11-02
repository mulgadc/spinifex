import { describe, expect, it } from "vitest"

import { partsToText } from "@/components/cli-command-panel"

import { buildRunInstancesCommands } from "./run-instances-cli"

const BASE = {
  imageId: "ami-1",
  instanceType: "t3.nano",
  keyName: "default",
  securityGroupMode: "create" as const,
  newSgName: "launch-wizard-1",
}

function text(values: Parameters<typeof buildRunInstancesCommands>[0]) {
  return partsToText(buildRunInstancesCommands(values, "vpc-1"))
}

describe("buildRunInstancesCommands", () => {
  it("tags the instance with its name, quoting a name with a quote in it", () => {
    const out = text({ ...BASE, name: " it's web " })

    expect(out).toContain("--tag-specifications")
    expect(out).toContain(String.raw`"Value":"it'\''s web"`)
  })

  it("leaves out tag specifications when the name is blank", () => {
    expect(text({ ...BASE, name: "  " })).not.toContain("--tag-specifications")
  })

  it("authorizes ping as ICMP echo request ahead of SSH", () => {
    const out = text({ ...BASE, allowPing: true, allowSsh: true })

    expect(out).toContain(
      '{"IpProtocol":"icmp","FromPort":8,"ToPort":-1,"IpRanges":[{"CidrIp":"0.0.0.0/0"}]},{"IpProtocol":"tcp","FromPort":22',
    )
  })

  it("names the instance on a launch-template launch as well", () => {
    const out = text({ launchTemplateId: "lt-1", name: "web-1" })

    expect(out).toContain("--launch-template")
    expect(out).toContain('"Value":"web-1"')
  })
})
