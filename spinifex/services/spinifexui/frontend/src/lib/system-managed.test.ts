import type { Image } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import { SYSTEM_MANAGED_TAG_KEY, isSystemManagedImage } from "./system-managed"

describe("isSystemManagedImage", () => {
  it("returns true when image carries the managed-by tag", () => {
    const image: Image = {
      ImageId: "ami-1",
      Tags: [{ Key: SYSTEM_MANAGED_TAG_KEY, Value: "elbv2" }],
    }
    expect(isSystemManagedImage(image)).toBeTruthy()
  })

  it("returns false for customer images with unrelated tags", () => {
    const image: Image = {
      ImageId: "ami-2",
      Tags: [{ Key: "Name", Value: "my-ami" }],
    }
    expect(isSystemManagedImage(image)).toBeFalsy()
  })

  it("returns false for images with no tags", () => {
    expect(isSystemManagedImage({ ImageId: "ami-3" })).toBeFalsy()
  })
})
