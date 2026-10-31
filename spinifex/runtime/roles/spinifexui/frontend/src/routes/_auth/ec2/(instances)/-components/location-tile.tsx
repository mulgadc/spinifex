import { regionLocation } from "@/lib/aws-regions"

import { ChoiceTile, ChoiceTiles } from "./choice-tile"
import { RegionFlag } from "./region-flag"

interface LocationTileProps {
  region: string
  availabilityZone: string | undefined
}

// The cluster serves one region and one zone today, so the tile is shown
// selected and read-only; it becomes a choice when there is more than one.
export function LocationTile({ region, availabilityZone }: LocationTileProps) {
  const location = regionLocation(region)

  return (
    <ChoiceTiles columns={2} label="Location">
      <ChoiceTile
        checked
        description={[location?.city, availabilityZone ?? "zone set by subnet"]
          .filter(Boolean)
          .join(" · ")}
        icon={<RegionFlag country={location?.country} />}
        label={region}
        layout="inline"
      />
    </ChoiceTiles>
  )
}
