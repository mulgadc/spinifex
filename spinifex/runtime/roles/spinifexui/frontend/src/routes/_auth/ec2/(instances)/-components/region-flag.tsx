import { Globe } from "lucide-react"
import type { ReactElement } from "react"

import type { CountryCode } from "@/lib/aws-regions"

// oxlint-disable shadcn/no-raw-colors -- a flag's colours are the country's, not theme tokens

// Simplified flags drawn on a 30 by 20 grid. They are recognisable marks at
// 24 pixels wide, not faithful reproductions.

function star(cx: number, cy: number, r: number, points = 5): string {
  const inner = r * 0.45
  const coords: string[] = []

  for (let i = 0; i < points * 2; i++) {
    const radius = i % 2 === 0 ? r : inner
    const angle = (Math.PI / points) * i - Math.PI / 2
    coords.push(
      `${(cx + radius * Math.cos(angle)).toFixed(2)},${(cy + radius * Math.sin(angle)).toFixed(2)}`,
    )
  }

  return coords.join(" ")
}

function horizontal(...colours: string[]): ReactElement[] {
  const h = 20 / colours.length

  // Stripes are positional and never reorder, so the index is their identity.
  return colours.map((fill, i) => (
    // oxlint-disable-next-line react/no-array-index-key
    <rect fill={fill} height={h} key={`${fill}-${i}`} width={30} y={i * h} />
  ))
}

function vertical(...colours: string[]): ReactElement[] {
  const w = 30 / colours.length

  // Stripes are positional and never reorder, so the index is their identity.
  return colours.map((fill, i) => (
    // oxlint-disable-next-line react/no-array-index-key
    <rect fill={fill} height={20} key={`${fill}-${i}`} width={w} x={i * w} />
  ))
}

function union(width: number, height: number): ReactElement {
  return (
    <g>
      <rect fill="#012169" height={height} width={width} />
      <path
        d={`M0,0 L${width},${height} M${width},0 L0,${height}`}
        stroke="#fff"
        strokeWidth={height / 5}
      />
      <path
        d={`M0,0 L${width},${height} M${width},0 L0,${height}`}
        stroke="#C8102E"
        strokeWidth={height / 15}
      />
      <path
        d={`M${width / 2},0 V${height} M0,${height / 2} H${width}`}
        stroke="#fff"
        strokeWidth={height / 3}
      />
      <path
        d={`M${width / 2},0 V${height} M0,${height / 2} H${width}`}
        stroke="#C8102E"
        strokeWidth={height / 5}
      />
    </g>
  )
}

const FLAGS = {
  AE: () => (
    <>
      {horizontal("#00732F", "#fff", "#000")}
      <rect fill="#FF0000" height={20} width={8} />
    </>
  ),
  AU: () => (
    <>
      <rect fill="#012169" height={20} width={30} />
      {union(15, 10)}
      <polygon fill="#fff" points={star(7.5, 15, 2.6, 7)} />
      <polygon fill="#fff" points={star(22.5, 4, 1.2, 7)} />
      <polygon fill="#fff" points={star(19, 9, 1.2, 7)} />
      <polygon fill="#fff" points={star(26, 8, 1.2, 7)} />
      <polygon fill="#fff" points={star(22.5, 16, 1.4, 7)} />
    </>
  ),
  BH: () => (
    <>
      <rect fill="#CE1126" height={20} width={30} />
      <polygon
        fill="#fff"
        points="0,0 8,0 11,2 8,4 11,6 8,8 11,10 8,12 11,14 8,16 11,18 8,20 0,20"
      />
    </>
  ),
  BR: () => (
    <>
      <rect fill="#009C3B" height={20} width={30} />
      <polygon fill="#FFDF00" points="15,2.5 27,10 15,17.5 3,10" />
      <circle cx={15} cy={10} fill="#002776" r={4.5} />
    </>
  ),
  CA: () => (
    <>
      {vertical("#D80621", "#fff", "#fff", "#D80621")}
      <polygon fill="#D80621" points={star(15, 10, 5, 5)} />
    </>
  ),
  CH: () => (
    <>
      <rect fill="#DA291C" height={20} width={30} />
      <path d="M15,4 V16 M9,10 H21" stroke="#fff" strokeWidth={3.5} />
    </>
  ),
  CN: () => (
    <>
      <rect fill="#EE1C25" height={20} width={30} />
      <polygon fill="#FFFF00" points={star(6, 6, 3.5)} />
      <polygon fill="#FFFF00" points={star(11, 2.5, 1)} />
      <polygon fill="#FFFF00" points={star(13, 5, 1)} />
      <polygon fill="#FFFF00" points={star(13, 8.5, 1)} />
      <polygon fill="#FFFF00" points={star(11, 11, 1)} />
    </>
  ),
  DE: () => <>{horizontal("#000", "#DD0000", "#FFCE00")}</>,
  ES: () => <>{horizontal("#AA151B", "#F1BF00", "#F1BF00", "#AA151B")}</>,
  FR: () => <>{vertical("#002395", "#fff", "#ED2939")}</>,
  GB: () => <>{union(30, 20)}</>,
  HK: () => (
    <>
      <rect fill="#DE2910" height={20} width={30} />
      <polygon fill="#fff" points={star(15, 10, 6, 5)} />
    </>
  ),
  ID: () => <>{horizontal("#CE1126", "#fff")}</>,
  IE: () => <>{vertical("#169B62", "#fff", "#FF883E")}</>,
  IL: () => (
    <>
      <rect fill="#fff" height={20} width={30} />
      <rect fill="#0038B8" height={2.5} width={30} y={2} />
      <rect fill="#0038B8" height={2.5} width={30} y={15.5} />
      <polygon
        fill="none"
        points="15,5.5 19,12.5 11,12.5"
        stroke="#0038B8"
        strokeWidth={1}
      />
      <polygon
        fill="none"
        points="15,14.5 19,7.5 11,7.5"
        stroke="#0038B8"
        strokeWidth={1}
      />
    </>
  ),
  IN: () => (
    <>
      {horizontal("#FF9933", "#fff", "#138808")}
      <circle
        cx={15}
        cy={10}
        fill="none"
        r={2.6}
        stroke="#000080"
        strokeWidth={0.8}
      />
    </>
  ),
  IT: () => <>{vertical("#009246", "#fff", "#CE2B37")}</>,
  JP: () => (
    <>
      <rect fill="#fff" height={20} width={30} />
      <circle cx={15} cy={10} fill="#BC002D" r={6} />
    </>
  ),
  KR: () => (
    <>
      <rect fill="#fff" height={20} width={30} />
      <path d="M10,10 A5,5 0 0 1 20,10 Z" fill="#CD2E3A" />
      <path d="M10,10 A5,5 0 0 0 20,10 Z" fill="#0047A0" />
    </>
  ),
  MX: () => (
    <>
      {vertical("#006847", "#fff", "#CE1126")}
      <circle cx={15} cy={10} fill="#8C5A2B" r={2.6} />
    </>
  ),
  MY: () => (
    <>
      {horizontal(
        "#CC0001",
        "#fff",
        "#CC0001",
        "#fff",
        "#CC0001",
        "#fff",
        "#CC0001",
      )}
      <rect fill="#010066" height={11.4} width={15} />
      <circle cx={6} cy={5.7} fill="#FFCC00" r={3.6} />
      <circle cx={7.4} cy={5.7} fill="#010066" r={3} />
      <polygon fill="#FFCC00" points={star(11.5, 5.7, 2.4, 7)} />
    </>
  ),
  NZ: () => (
    <>
      <rect fill="#012169" height={20} width={30} />
      {union(15, 10)}
      <polygon fill="#C8102E" points={star(22.5, 4, 1.4)} />
      <polygon fill="#C8102E" points={star(19.5, 9.5, 1.4)} />
      <polygon fill="#C8102E" points={star(25.5, 8.5, 1.4)} />
      <polygon fill="#C8102E" points={star(22.5, 16, 1.6)} />
    </>
  ),
  SE: () => (
    <>
      <rect fill="#006AA7" height={20} width={30} />
      <path d="M11,0 V20 M0,10 H30" stroke="#FECC00" strokeWidth={4} />
    </>
  ),
  SG: () => (
    <>
      {horizontal("#EF3340", "#fff")}
      <circle cx={6.5} cy={5} fill="#fff" r={3.4} />
      <circle cx={8} cy={5} fill="#EF3340" r={3.2} />
    </>
  ),
  TH: () => (
    <>
      {horizontal(
        "#A51931",
        "#F4F5F8",
        "#2D2A4A",
        "#2D2A4A",
        "#F4F5F8",
        "#A51931",
      )}
    </>
  ),
  TW: () => (
    <>
      <rect fill="#FE0000" height={20} width={30} />
      <rect fill="#000095" height={10} width={15} />
      <circle cx={7.5} cy={5} fill="#fff" r={2.8} />
    </>
  ),
  US: () => (
    <>
      {horizontal(
        "#B22234",
        "#fff",
        "#B22234",
        "#fff",
        "#B22234",
        "#fff",
        "#B22234",
      )}
      <rect fill="#3C3B6E" height={11.4} width={13} />
    </>
  ),
  ZA: () => (
    <>
      {horizontal("#E03C31", "#001489")}
      <polygon fill="#fff" points="0,0 4,0 14,8 30,8 30,12 14,12 4,20 0,20" />
      <polygon
        fill="#007749"
        points="0,1.5 2,0 4,0 13,7 30,7 30,13 13,13 4,20 2,20 0,18.5"
      />
      <polygon fill="#FFB81C" points="0,3.5 9,10 0,16.5" />
      <polygon fill="#000" points="0,5 7,10 0,15" />
    </>
  ),
} satisfies Record<CountryCode, () => ReactElement>

interface RegionFlagProps {
  country: CountryCode | undefined
}

// A globe stands in for a region with no country, such as a custom name.
export function RegionFlag({ country }: RegionFlagProps) {
  if (!country) {
    return <Globe aria-hidden className="size-5 text-muted-foreground" />
  }

  return (
    <svg
      aria-hidden
      className="h-4 w-6 shrink-0 overflow-hidden rounded-xs ring-1 ring-border"
      data-country={country}
      viewBox="0 0 30 20"
    >
      {FLAGS[country]()}
    </svg>
  )
}
