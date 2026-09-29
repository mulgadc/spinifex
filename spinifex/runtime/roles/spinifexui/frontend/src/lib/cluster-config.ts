import { z } from "zod"

// The SPA is embedded in the spx binary, so anything baked in at build time
// would pin one binary to one region. The serving node reports its own region
// instead, which is the value awsgw verifies signatures against.

const clusterConfigSchema = z.object({
  region: z.string().min(1),
  // Absent on an older node's response, which safe-defaults to hidden rather
  // than exposing the console before it is meant to ship.
  ochreEnabled: z.boolean().default(false),
})

type ClusterConfig = z.infer<typeof clusterConfigSchema>

let config: ClusterConfig | null = null

// Fetched once before the app renders so every signing path can read the region
// synchronously.
export async function loadClusterConfig(): Promise<void> {
  const response = await fetch("/api/config", { cache: "no-store" })
  if (!response.ok) {
    throw new Error(`could not load cluster config: HTTP ${response.status}`)
  }
  const parsed = clusterConfigSchema.safeParse(await response.json())
  if (!parsed.success) {
    // An issue at the root means the body was not an object at all; anything
    // deeper means the region itself is missing or empty.
    const atRoot = parsed.error.issues.some((issue) => issue.path.length === 0)
    throw new TypeError(
      atRoot
        ? "cluster config was not an object"
        : "cluster config did not include a region",
    )
  }
  config = parsed.data
}

// Throws rather than falling back to a default: signing with the wrong region
// fails as an opaque credential error, which is far harder to diagnose.
export function getRegion(): string {
  if (!config) {
    throw new Error("cluster config has not been loaded")
  }
  return config.region
}

// Gates the Ochre console nav group and its routes until the flag ships on.
export function isOchreEnabled(): boolean {
  if (!config) {
    throw new Error("cluster config has not been loaded")
  }
  return config.ochreEnabled
}
