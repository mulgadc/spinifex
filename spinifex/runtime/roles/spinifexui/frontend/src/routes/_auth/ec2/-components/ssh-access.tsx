import type { Image, Instance } from "@aws-sdk/client-ec2"
import type { ReactNode } from "react"

import { PanelField } from "@/components/detail-panel"
import { InfoList } from "@/components/info-tip"
import {
  buildSSHCommand,
  resolveSSHLogin,
  resolveSSHTarget,
} from "@/lib/ssh-login"

interface SSHAccessProps {
  instance: Instance
  image: Image | undefined
}

function SSHNote({ children }: { children: ReactNode }) {
  return (
    <div className="py-1.5">
      <dd className="text-xs text-warning-foreground">{children}</dd>
    </div>
  )
}

// The command to reach a running instance, as rows of its Details. The login
// user is the part nobody can guess: it comes from the AMI, not from Spinifex.
export function SSHAccess({ instance, image }: SSHAccessProps) {
  const { address, reachability } = resolveSSHTarget(instance)
  const { user, distro } = resolveSSHLogin(image)

  if (reachability === "none") {
    return null
  }

  const command = buildSSHCommand({
    keyName: instance.KeyName,
    user,
    address,
  })

  return (
    <>
      <PanelField copy={command} label="SSH" mono>
        <span title={command}>{command}</span>
      </PanelField>
      <PanelField
        info={
          <>
            <p>
              The account cloud-init creates in the guest. It comes from the
              image, not from Spinifex.
            </p>
            <InfoList
              items={[
                ["Image", image?.Name],
                ["Distribution", distro],
                ["User", user || "Unknown"],
              ]}
            />
          </>
        }
        label="Login user"
        mono
      >
        {user || undefined}
      </PanelField>
      {reachability === "private" && (
        <SSHNote>
          No public IP, so {address} is reachable only from inside the VPC.
        </SSHNote>
      )}
      {!user && (
        <SSHNote>
          The login user could not be determined from this AMI; check the
          image&apos;s documentation.
        </SSHNote>
      )}
    </>
  )
}
