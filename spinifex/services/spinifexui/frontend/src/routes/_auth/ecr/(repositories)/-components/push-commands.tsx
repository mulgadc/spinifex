import { useState } from "react"

import { Button } from "@/components/ui/button"

const AWS_REGION = "ap-southeast-2"

// registryHostFrom strips the trailing "/{repo}" path from a repositoryUri to
// recover the registry host docker logs in to. Returns "" when the URI is unset.
function registryHostFrom(repositoryUri: string | undefined): string {
  if (!repositoryUri) {
    return ""
  }
  const slash = repositoryUri.indexOf("/")
  return slash === -1 ? repositoryUri : repositoryUri.slice(0, slash)
}

interface PushCommandsProps {
  repositoryName: string
  repositoryUri: string | undefined
}

export function PushCommands({
  repositoryName,
  repositoryUri,
}: PushCommandsProps) {
  const [copied, setCopied] = useState(false)
  const host = registryHostFrom(repositoryUri)
  const uri = repositoryUri ?? `${host}/${repositoryName}`

  const commands = [
    `aws ecr get-login-password --region ${AWS_REGION} | docker login --username AWS --password-stdin ${host}`,
    `docker build -t ${repositoryName} .`,
    `docker tag ${repositoryName}:latest ${uri}:latest`,
    `docker push ${uri}:latest`,
  ]

  async function handleCopy() {
    await navigator.clipboard.writeText(commands.join("\n"))
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium">Push commands</h2>
        <Button onClick={handleCopy} size="sm" variant="outline">
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre className="overflow-x-auto rounded bg-muted p-3 font-mono text-xs">
        {commands.join("\n")}
      </pre>
    </div>
  )
}
