import { Check, Copy } from "lucide-react"

import { Button } from "@/components/ui/button"
import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"

function NameServerRow({ nameServer }: { nameServer: string }) {
  const { copied, copy } = useCopyToClipboard()

  return (
    <li className="flex items-center justify-between gap-2 px-3 py-2">
      <span className="font-mono text-sm">{nameServer}</span>
      <Button
        aria-label={`Copy ${nameServer}`}
        onClick={() => {
          void copy(nameServer)
        }}
        size="sm"
        type="button"
        variant="ghost"
      >
        {copied ? (
          <Check className="size-3.5" />
        ) : (
          <Copy className="size-3.5" />
        )}
      </Button>
    </li>
  )
}

export function NameServerList({
  nameServers,
}: {
  nameServers: readonly string[]
}) {
  if (nameServers.length === 0) {
    return <p className="text-sm text-muted-foreground">No name servers.</p>
  }

  return (
    <ul className="max-w-md divide-y rounded-md border bg-card">
      {nameServers.map((nameServer) => (
        <NameServerRow key={nameServer} nameServer={nameServer} />
      ))}
    </ul>
  )
}
