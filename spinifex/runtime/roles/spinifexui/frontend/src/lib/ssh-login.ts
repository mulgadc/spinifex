import type { Image, Instance } from "@aws-sdk/client-ec2"

// The SSH login user belongs to the image, not to Spinifex. Nothing in the
// launch path creates an account or renames one: stock cloud-init reads the
// EC2 datasource and applies the distro's own default_user, so every name
// below is whatever the upstream image ships with.
// Ordered longest name first, so an image naming two of them resolves to the
// more specific: "amazonlinux" must not be read as "amzn".
const DEFAULT_USERS = [
  ["amazonlinux", "ec2-user"],
  ["almalinux", "almalinux"],
  ["oracle", "cloud-user"],
  ["ubuntu", "ubuntu"],
  ["debian", "debian"],
  ["fedora", "fedora"],
  ["centos", "cloud-user"],
  ["alpine", "alpine"],
  ["rocky", "rocky"],
  ["amzn", "ec2-user"],
  ["rhel", "cloud-user"],
] as const

export type KnownDistro = (typeof DEFAULT_USERS)[number][0]

export interface SSHLogin {
  user: string
  distro?: KnownDistro
}

// resolveSSHLogin reads the distro out of an AMI's own name and description.
// Our catalog images register as "ami-<distro>-<version>-<arch>", and an AMI a
// customer captured with CreateImage inherits that name, so the token survives
// a re-capture. An unrecognised image yields no user rather than a guess: a
// wrong username in a copy-paste command is worse than an honest placeholder.
export function resolveSSHLogin(image: Image | undefined): SSHLogin {
  const tokens = new Set(
    `${image?.Name ?? ""} ${image?.Description ?? ""}`
      .toLowerCase()
      .split(/[^a-z0-9]+/)
      .filter(Boolean),
  )
  const match = DEFAULT_USERS.find(([name]) => tokens.has(name))

  return match ? { user: match[1], distro: match[0] } : { user: "" }
}

export type SSHReachability = "public" | "private" | "none"

export interface SSHTarget {
  address: string
  reachability: SSHReachability
}

// A public address is reachable from anywhere the security group allows. A
// private one is only reachable from inside the VPC, which is a material
// difference to whoever is about to paste the command, so it is returned as a
// distinct state rather than as a plain address.
export function resolveSSHTarget(instance: Instance | undefined): SSHTarget {
  if (instance?.PublicIpAddress) {
    return { address: instance.PublicIpAddress, reachability: "public" }
  }
  if (instance?.PrivateIpAddress) {
    return { address: instance.PrivateIpAddress, reachability: "private" }
  }
  return { address: "", reachability: "none" }
}

export const SSH_KEY_PLACEHOLDER = "path/to/key.pem"
export const SSH_USER_PLACEHOLDER = "<user>"

// buildSSHCommand assembles the line to paste. The key path stays a
// placeholder even when the key pair's name is known, because the private half
// never leaves the machine that generated it — Spinifex stores only the public
// key and cannot know where the operator put the other one.
export function buildSSHCommand(options: {
  keyName?: string
  user: string
  address: string
}): string {
  const keyPath = options.keyName
    ? `path/to/${options.keyName}.pem`
    : SSH_KEY_PLACEHOLDER
  const user = options.user || SSH_USER_PLACEHOLDER
  return `ssh -i ${keyPath} ${user}@${options.address}`
}
