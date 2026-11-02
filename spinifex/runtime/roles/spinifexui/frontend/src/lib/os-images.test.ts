import type { Image, InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import {
  buildImageCatalog,
  compareVersions,
  distroLabel,
  imageArchitecture,
  imageArchitectures,
  instanceTypeSupports,
  latestOfDistro,
  parseImageName,
  preferredImage,
} from "./os-images"

function image(id: string, name: string, architecture?: string): Image {
  return { ImageId: id, Name: name, Architecture: architecture as never }
}

describe("parseImageName", () => {
  it.each([
    [
      "ami-ubuntu-26.04-x86_64",
      { distro: "ubuntu", version: "26.04", arch: "x86_64" },
    ],
    ["oracle-10.1-arm64", { distro: "oracle", version: "10.1", arch: "arm64" }],
    ["rocky-10-x86_64", { distro: "rocky", version: "10", arch: "x86_64" }],
    [
      "alpine-3.24.3-aarch64",
      { distro: "alpine", version: "3.24.3", arch: "arm64" },
    ],
    ["debian-13-amd64", { distro: "debian", version: "13", arch: "x86_64" }],
    [
      "rocky-linux-9.5-x86_64",
      { distro: "rocky-linux", version: "9.5", arch: "x86_64" },
    ],
  ])("reads %s", (name, expected) => {
    expect(parseImageName(name)).toStrictEqual(expected)
  })

  it.each([
    "ubuntu-24.04",
    "my-golden-image",
    "spinifex-rds-postgres",
    "ubuntu-26.04-riscv64",
    "26.04-x86_64",
    "",
  ])("rejects %s", (name) => {
    expect(parseImageName(name)).toBeUndefined()
  })

  it("rejects a missing name", () => {
    expect(parseImageName(undefined)).toBeUndefined()
  })
})

describe("compareVersions", () => {
  it("compares each segment as a number", () => {
    expect(compareVersions("10.1", "9.8")).toBeGreaterThan(0)
    expect(compareVersions("24.10", "24.4")).toBeGreaterThan(0)
    expect(compareVersions("3.24.1", "3.24.3")).toBeLessThan(0)
  })

  it("treats a missing segment as zero", () => {
    expect(compareVersions("10", "10.0")).toBe(0)
    expect(compareVersions("10", "10.1")).toBeLessThan(0)
  })
})

describe("imageArchitecture", () => {
  it("prefers the image's own Architecture over its name", () => {
    expect(imageArchitecture(image("a", "ubuntu-26.04-x86_64", "arm64"))).toBe(
      "arm64",
    )
  })

  it("falls back to the name", () => {
    expect(imageArchitecture(image("a", "oracle-10.1-arm64"))).toBe("arm64")
  })

  it("is undefined when neither says", () => {
    expect(imageArchitecture(image("a", "golden"))).toBeUndefined()
  })
})

describe("buildImageCatalog", () => {
  const images = [
    image("ami-o98", "oracle-9.8-x86_64", "x86_64"),
    image("ami-o101", "oracle-10.1-x86_64", "x86_64"),
    image("ami-o101a", "oracle-10.1-arm64", "arm64"),
    image("ami-u", "ami-ubuntu-26.04-x86_64", "x86_64"),
    image("ami-gold", "golden", "x86_64"),
    image("ami-unknown", "untagged"),
  ]

  it("groups by distro, newest version first, for one architecture", () => {
    const catalog = buildImageCatalog(images, "x86_64")

    expect(catalog.distros.map((g) => g.distro)).toStrictEqual([
      "oracle",
      "ubuntu",
    ])
    expect(
      catalog.distros[0]?.versions.map((v) => v.image.ImageId),
    ).toStrictEqual(["ami-o101", "ami-o98"])
  })

  it("keeps unparsed images, and those of no known architecture, as other", () => {
    expect(
      buildImageCatalog(images, "arm64").other.map((i) => i.ImageId),
    ).toStrictEqual(["ami-unknown"])
    expect(
      buildImageCatalog(images, "x86_64").other.map((i) => i.ImageId),
    ).toStrictEqual(["ami-gold", "ami-unknown"])
  })

  it("prefers ubuntu's newest, then the first distro, then any other image", () => {
    expect(preferredImage(buildImageCatalog(images, "x86_64"))?.ImageId).toBe(
      "ami-u",
    )
    expect(preferredImage(buildImageCatalog(images, "arm64"))?.ImageId).toBe(
      "ami-o101a",
    )
    expect(
      preferredImage(buildImageCatalog([image("g", "golden")], undefined))
        ?.ImageId,
    ).toBe("g")
  })

  it("finds a distro's newest image in another architecture", () => {
    const arm = buildImageCatalog(images, "arm64")

    expect(latestOfDistro(arm, "oracle")?.ImageId).toBe("ami-o101a")
    expect(latestOfDistro(arm, "ubuntu")).toBeUndefined()
  })
})

describe("imageArchitectures", () => {
  it("lists x86_64 before arm64, once each", () => {
    expect(
      imageArchitectures([
        image("a", "oracle-10.1-arm64"),
        image("b", "x", "x86_64"),
        image("c", "y", "arm64"),
      ]),
    ).toStrictEqual(["x86_64", "arm64"])
  })
})

describe("instanceTypeSupports", () => {
  const arm: InstanceTypeInfo = {
    InstanceType: "t4g.nano",
    ProcessorInfo: { SupportedArchitectures: ["arm64"] },
  }

  it("matches the type's reported architecture", () => {
    expect(instanceTypeSupports(arm, "arm64")).toBeTruthy()
    expect(instanceTypeSupports(arm, "x86_64")).toBeFalsy()
  })

  it("offers a type that reports none for every architecture", () => {
    expect(
      instanceTypeSupports({ InstanceType: "t3.nano" }, "arm64"),
    ).toBeTruthy()
  })
})

describe("distroLabel", () => {
  it("names known distros and title-cases the rest", () => {
    expect(distroLabel("oracle")).toBe("Oracle Linux")
    expect(distroLabel("rocky-linux")).toBe("Rocky Linux")
    expect(distroLabel("nixos")).toBe("Nixos")
  })
})
