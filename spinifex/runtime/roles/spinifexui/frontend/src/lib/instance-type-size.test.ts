import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import { describeClassSize } from "./instance-type-size"

const TYPES: InstanceTypeInfo[] = [
  {
    InstanceType: "m5.large",
    VCpuInfo: { DefaultVCpus: 2 },
    MemoryInfo: { SizeInMiB: 8192 },
  },
]

describe("describeClassSize", () => {
  it("sizes a db. class by the EC2 type it runs on", () => {
    expect(describeClassSize("db.m5.large", TYPES)).toBe(
      "2 vCPU · 8 GiB memory",
    )
  })

  it("says nothing for a class the cluster does not offer", () => {
    expect(describeClassSize("db.r6g.large", TYPES)).toBeUndefined()
  })
})
