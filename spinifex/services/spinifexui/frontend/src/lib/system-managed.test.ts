import type { Image } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import {
  LB_MANAGED_BY_VALUE,
  SYSTEM_MANAGED_TAG_KEY,
  hasLbImage,
  isLbImage,
  isSystemManagedImage,
} from "./system-managed"

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

describe("isLbImage", () => {
  it("returns true when tag value matches elbv2", () => {
    const image: Image = {
      ImageId: "ami-lb",
      Tags: [{ Key: SYSTEM_MANAGED_TAG_KEY, Value: LB_MANAGED_BY_VALUE }],
    }
    expect(isLbImage(image)).toBeTruthy()
  })

  it("returns false for system-managed images owned by other components", () => {
    const image: Image = {
      ImageId: "ami-other",
      Tags: [{ Key: SYSTEM_MANAGED_TAG_KEY, Value: "other-service" }],
    }
    expect(isLbImage(image)).toBeFalsy()
  })

  it("returns false for customer images", () => {
    expect(isLbImage({ ImageId: "ami-cust" })).toBeFalsy()
  })
})

describe("hasLbImage", () => {
  it("returns true when any image in the list is the LB AMI", () => {
    const images: Image[] = [
      { ImageId: "ami-cust", Tags: [{ Key: "Name", Value: "ubuntu" }] },
      {
        ImageId: "ami-lb",
        Tags: [{ Key: SYSTEM_MANAGED_TAG_KEY, Value: LB_MANAGED_BY_VALUE }],
      },
    ]
    expect(hasLbImage(images)).toBeTruthy()
  })

  it("returns false for an empty list", () => {
    expect(hasLbImage([])).toBeFalsy()
  })

  it("returns false when no image carries the elbv2 tag", () => {
    const images: Image[] = [
      { ImageId: "ami-cust", Tags: [{ Key: "Name", Value: "ubuntu" }] },
    ]
    expect(hasLbImage(images)).toBeFalsy()
  })
})
