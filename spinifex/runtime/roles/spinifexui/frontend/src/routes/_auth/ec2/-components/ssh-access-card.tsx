import type { Image, Instance } from "@aws-sdk/client-ec2"
import { Check, Copy, Info, Network, TriangleAlert } from "lucide-react"

import { DetailCard } from "@/components/detail-card"
import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"
import {
  buildSSHCommand,
  resolveSSHLogin,
  resolveSSHTarget,
} from "@/lib/ssh-login"

interface SSHAccessCardProps {
  instance: Instance
  image: Image | undefined
}

// SSHAccessCard shows the one command needed to reach a running instance.
// The login user is the part nobody can guess — it comes from the AMI, not
// from Spinifex — so the card names it explicitly and says where it came from.
export function SSHAccessCard({ instance, image }: SSHAccessCardProps) {
  const { copied, copy } = useCopyToClipboard()

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
    <DetailCard>
      <DetailCard.Header>Connect over SSH</DetailCard.Header>
      <div className="space-y-4 p-4">
        <div className="flex items-center gap-2 rounded border border-border bg-background px-3 py-2 font-mono text-xs">
          <span className="text-muted-foreground select-none">$</span>
          <span className="flex-1 overflow-x-auto whitespace-nowrap text-foreground">
            {command}
          </span>
          <button
            aria-label="Copy SSH command"
            className="text-muted-foreground transition-colors hover:text-foreground"
            onClick={() => {
              void copy(command)
            }}
            type="button"
          >
            {copied ? (
              <Check className="size-3.5 text-tactical-green" />
            ) : (
              <Copy className="size-3.5" />
            )}
          </button>
        </div>

        <dl className="grid gap-x-6 gap-y-2 text-xs sm:grid-cols-3">
          <div>
            <dt className="text-muted-foreground">Login user</dt>
            <dd className="font-mono text-foreground">
              {user || "unknown — check the AMI"}
            </dd>
          </div>
          <div>
            <dt className="text-muted-foreground">Address</dt>
            <dd className="font-mono text-foreground">{address}</dd>
          </div>
          <div>
            <dt className="text-muted-foreground">Key pair</dt>
            <dd className="font-mono text-foreground">
              {instance.KeyName ?? "none assigned"}
            </dd>
          </div>
        </dl>

        {reachability === "private" && (
          <Callout tone="amber" icon={Network}>
            This instance has no public IP address, so{" "}
            <span className="font-mono">{address}</span> is reachable only from
            inside the VPC. Run the command from another instance in the same
            VPC whose security group allows it, or associate an Elastic IP with
            this one.
          </Callout>
        )}

        {user ? (
          <Callout tone="muted" icon={Info}>
            <span className="font-mono">{user}</span> is the default login user
            for {distro} images. It comes from the AMI itself — Spinifex does
            not create or rename the account — so an image built with a
            different default will want a different name here.
          </Callout>
        ) : (
          <Callout tone="amber" icon={TriangleAlert}>
            The login user could not be determined from this AMI. Every
            distribution ships its own default —{" "}
            <span className="font-mono">ubuntu</span>,{" "}
            <span className="font-mono">debian</span>,{" "}
            <span className="font-mono">cloud-user</span> on Oracle Linux and
            RHEL — so check the image&apos;s documentation before connecting.
          </Callout>
        )}

        <p className="text-xs text-muted-foreground">
          Spinifex stores only the public half of a key pair, so the path above
          is where <em>you</em> saved the private key when you created it. It
          must be readable only by you (
          <span className="font-mono">chmod 600</span>
          ), and the instance&apos;s security group must allow TCP 22 from
          wherever you are connecting.
        </p>
      </div>
    </DetailCard>
  )
}

const CALLOUT_TONES = {
  amber: "border-tactical-amber/40 bg-tactical-amber/5 text-tactical-amber",
  muted: "border-border bg-muted/40 text-muted-foreground",
} as const

function Callout({
  tone,
  icon: Icon,
  children,
}: {
  tone: keyof typeof CALLOUT_TONES
  icon: typeof Info
  children: React.ReactNode
}) {
  return (
    <div
      className={`flex items-start gap-2 rounded-md border p-3 ${CALLOUT_TONES[tone]}`}
    >
      <Icon className="mt-0.5 size-3.5 shrink-0" />
      <p className="text-xs leading-relaxed text-muted-foreground">
        {children}
      </p>
    </div>
  )
}
