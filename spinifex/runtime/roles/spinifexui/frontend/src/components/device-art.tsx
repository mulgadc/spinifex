import type { DeviceKind } from "@/lib/node-topology"
import type { FormFactor } from "@/queries/nodes"

interface DeviceArtProps {
  kind: DeviceKind
  formFactor?: FormFactor
  className?: string
}

// Line drawings of the hardware, in currentColor so they follow the theme.
// Each is a 64 x 48 viewBox, drawn front-on as the thing looks in a rack.
export function DeviceArt({ kind, formFactor, className }: DeviceArtProps) {
  return (
    <svg
      aria-hidden
      className={className}
      fill="none"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth={1.5}
      viewBox="0 0 64 48"
    >
      {art(kind, formFactor)}
    </svg>
  )
}

function art(kind: DeviceKind, formFactor: FormFactor | undefined) {
  if (kind === "internet") {
    return (
      <>
        <path d="M18 34h28a9 9 0 0 0 0-18 13 13 0 0 0-25-3 10 10 0 0 0-3 21Z" />
        <path d="M26 26h12M32 20v12" opacity={0.5} />
      </>
    )
  }

  if (kind === "switch" || kind === "segment") {
    return (
      <>
        <rect height={14} rx={2} width={58} x={3} y={17} />
        {[9, 15, 21, 27, 33, 39].map((x) => (
          <rect height={4} key={x} rx={0.5} width={4} x={x} y={22} />
        ))}
        <rect height={4} rx={0.5} width={6} x={47} y={22} />
        <circle cx={56} cy={24} fill="currentColor" r={1} />
        {kind === "segment" && <path d="M3 37h58" strokeDasharray="3 3" />}
      </>
    )
  }

  return nodeArt(formFactor)
}

function nodeArt(formFactor: FormFactor | undefined) {
  if (formFactor === "rack-server") {
    return (
      <>
        <rect height={10} rx={1.5} width={58} x={3} y={13} />
        <rect height={10} rx={1.5} width={58} x={3} y={25} />
        {[9, 15, 21, 27].map((x) => (
          <path d={`M${x} 16v4M${x} 28v4`} key={x} />
        ))}
        <circle cx={54} cy={18} fill="currentColor" r={1} />
        <circle cx={54} cy={30} fill="currentColor" r={1} />
      </>
    )
  }

  if (formFactor === "tower") {
    return (
      <>
        <rect height={40} rx={2} width={24} x={20} y={4} />
        <path d="M25 10h14M25 14h14" />
        <circle cx={32} cy={36} r={2.5} />
      </>
    )
  }

  if (formFactor === "sbc") {
    return (
      <>
        <rect height={26} rx={2} width={44} x={10} y={11} />
        <rect height={10} rx={1} width={10} x={18} y={19} />
        <path d="M34 15h4M40 15h4M46 15h4M34 33h16" />
        <circle cx={14} cy={15} r={1.2} />
        <circle cx={50} cy={33} r={1.2} />
      </>
    )
  }

  if (formFactor === "jetson") {
    return (
      <>
        <rect height={24} rx={2} width={40} x={12} y={14} />
        <path d="M16 18h32M16 22h32M16 26h32M16 30h32" opacity={0.6} />
        <rect height={6} rx={1} width={14} x={25} y={38} />
      </>
    )
  }

  if (formFactor === "virtual-machine") {
    return (
      <>
        <rect height={30} rx={2} width={48} x={8} y={9} strokeDasharray="4 3" />
        <rect height={14} rx={1.5} width={32} x={16} y={17} />
        <path d="M22 24h8" />
      </>
    )
  }

  return (
    <>
      <rect height={30} rx={2} width={48} x={8} y={9} />
      <path d="M28 20a4 4 0 1 1 5 4v3M32 32v0.5" />
    </>
  )
}
