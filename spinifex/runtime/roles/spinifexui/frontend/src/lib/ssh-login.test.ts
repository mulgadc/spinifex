import { describe, expect, it } from "vitest"

import {
  buildSSHCommand,
  resolveSSHLogin,
  resolveSSHTarget,
  SSH_KEY_PLACEHOLDER,
  SSH_USER_PLACEHOLDER,
} from "./ssh-login"

describe("resolveSSHLogin", () => {
  // Every name here is the distro's own default_user, verified against the
  // upstream cloud image rather than inferred from the distro name: Oracle
  // Linux is cloud-user, not oracle, and Debian is debian, not admin.
  it.each([
    ["ami-ubuntu-26.04-x86_64", "ubuntu", "ubuntu"],
    ["ami-debian-13-x86_64", "debian", "debian"],
    ["ami-oracle-10.1-x86_64", "cloud-user", "oracle"],
    ["ami-oracle-9.8-arm64", "cloud-user", "oracle"],
    ["ami-rocky-10-x86_64", "rocky", "rocky"],
    ["ami-alpine-3.24.1-x86_64", "alpine", "alpine"],
  ])("maps %s to %s", (name, user, distro) => {
    expect(resolveSSHLogin({ Name: name })).toStrictEqual({ user, distro })
  })

  it("reads the description when the name carries no distro", () => {
    expect(
      resolveSSHLogin({
        Name: "ami-0abc123",
        Description: "Oracle Linux 10.1 cloud image prepared for Spinifex",
      }),
    ).toStrictEqual({ user: "cloud-user", distro: "oracle" })
  })

  it("returns no user for an unrecognised image rather than guessing", () => {
    expect(resolveSSHLogin({ Name: "my-hardened-appliance" })).toStrictEqual({
      user: "",
    })
    expect(resolveSSHLogin(undefined)).toStrictEqual({ user: "" })
  })

  it("does not match a distro name buried inside a longer word", () => {
    expect(resolveSSHLogin({ Name: "ami-ubuntuish-1" })).toStrictEqual({
      user: "",
    })
  })

  it("prefers the more specific of two overlapping tokens", () => {
    expect(resolveSSHLogin({ Name: "amazonlinux amzn" }).user).toBe("ec2-user")
  })
})

describe("resolveSSHTarget", () => {
  it("prefers the public address", () => {
    expect(
      resolveSSHTarget({
        PublicIpAddress: "149.118.74.90",
        PrivateIpAddress: "172.31.0.4",
      }),
    ).toStrictEqual({ address: "149.118.74.90", reachability: "public" })
  })

  it("flags a private-only instance", () => {
    expect(resolveSSHTarget({ PrivateIpAddress: "172.31.0.4" })).toStrictEqual({
      address: "172.31.0.4",
      reachability: "private",
    })
  })

  it("reports none when the instance has no address at all", () => {
    expect(resolveSSHTarget({})).toStrictEqual({
      address: "",
      reachability: "none",
    })
    expect(resolveSSHTarget(undefined).reachability).toBe("none")
  })
})

describe("buildSSHCommand", () => {
  it("names the key pair's file", () => {
    expect(
      buildSSHCommand({
        keyName: "oracle-test",
        user: "cloud-user",
        address: "149.118.74.90",
      }),
    ).toBe("ssh -i path/to/oracle-test.pem cloud-user@149.118.74.90")
  })

  it("falls back to placeholders rather than emitting a broken command", () => {
    expect(buildSSHCommand({ user: "", address: "172.31.0.4" })).toBe(
      `ssh -i ${SSH_KEY_PLACEHOLDER} ${SSH_USER_PLACEHOLDER}@172.31.0.4`,
    )
  })
})
