import { Link } from "@tanstack/react-router"
import {
  FlaskConical,
  FolderOpen,
  HardDrive,
  Key,
  Network,
  Ship,
} from "lucide-react"
import { useState } from "react"

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogMedia,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"

// Sandbox-ness is a property of how the console was reached, not of the node
// serving it: the same nodes answer on a LAN address and on the public name.
const SANDBOX_HOST = "console.spx3.com"
const DISMISSED_KEY = "spinifex:v1:sandbox-welcome"

const STARTING_POINTS = [
  {
    to: "/ec2/run-instances",
    icon: <HardDrive className="size-3.5" />,
    label: "Launch an instance",
    hint: "Boot a VM on real hardware",
  },
  {
    to: "/s3/ls",
    icon: <FolderOpen className="size-3.5" />,
    label: "Create a bucket",
    hint: "S3-compatible object storage",
  },
  {
    to: "/ec2/describe-vpcs",
    icon: <Network className="size-3.5" />,
    label: "Networking",
    hint: "VPCs, subnets and security groups",
  },
  {
    to: "/eks/list-clusters",
    icon: <Ship className="size-3.5" />,
    label: "Kubernetes",
    hint: "Managed EKS-compatible clusters",
  },
  {
    to: "/iam/list-users",
    icon: <Key className="size-3.5" />,
    label: "Access keys",
    hint: "Credentials for the AWS CLI and SDKs",
  },
]

function wasDismissed(): boolean {
  try {
    return localStorage.getItem(DISMISSED_KEY) !== null
  } catch {
    // localStorage might be disabled
  }
  return false
}

// Storage failing means the welcome shows again next visit, which is the right
// direction to fail in for a message that is only ever informational.
function markDismissed(): void {
  try {
    localStorage.setItem(DISMISSED_KEY, "1")
  } catch {
    // localStorage might be disabled
  }
}

export function SandboxWelcomeDialog() {
  const [open, setOpen] = useState(
    () => window.location.hostname === SANDBOX_HOST && !wasDismissed(),
  )

  const handleOpenChange = (next: boolean) => {
    if (!next) {
      markDismissed()
    }
    setOpen(next)
  }

  return (
    <AlertDialog onOpenChange={handleOpenChange} open={open}>
      <AlertDialogContent className="max-w-lg">
        <AlertDialogHeader>
          <AlertDialogMedia>
            <FlaskConical className="text-primary" />
          </AlertDialogMedia>
          <AlertDialogTitle>Welcome to the Mulga sandbox</AlertDialogTitle>
          <AlertDialogDescription>
            You have a real account on a shared evaluation cluster. The APIs are
            the AWS ones, so the CLI and the SDKs work against it unchanged.
            Resources here may be reclaimed, so please don&apos;t keep anything
            you need in it.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <div className="space-y-0.5">
          {STARTING_POINTS.map((point) => (
            <Link
              className="flex items-center gap-2 rounded-md px-2 py-1.5 text-xs hover:bg-muted"
              key={point.to}
              onClick={() => handleOpenChange(false)}
              to={point.to}
            >
              {point.icon}
              <span className="font-medium">{point.label}</span>
              <span className="text-muted-foreground">{point.hint}</span>
            </Link>
          ))}
        </div>

        <AlertDialogFooter>
          <a
            className="text-xs text-muted-foreground underline underline-offset-3 hover:text-foreground sm:mr-auto sm:self-center"
            href="https://docs.mulgadc.com"
            rel="noreferrer"
            target="_blank"
          >
            Read the docs
          </a>
          <AlertDialogAction onClick={() => handleOpenChange(false)}>
            Get started
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
