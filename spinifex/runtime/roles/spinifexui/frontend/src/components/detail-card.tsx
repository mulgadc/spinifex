import type { ReactNode } from "react"

function DetailCardRoot({ children }: { children: ReactNode }) {
  return <div className="rounded-lg border bg-card">{children}</div>
}

function DetailCardHeader({ children }: { children: ReactNode }) {
  return (
    <div className="border-b p-4">
      <h2 className="font-semibold">{children}</h2>
    </div>
  )
}

function DetailCardContent({ children }: { children: ReactNode }) {
  return <div className="grid gap-4 p-4 sm:grid-cols-2">{children}</div>
}

export const DetailCard = Object.assign(DetailCardRoot, {
  Header: DetailCardHeader,
  Content: DetailCardContent,
})
