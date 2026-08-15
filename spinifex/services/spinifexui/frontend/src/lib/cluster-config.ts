// The SPA is embedded in the spx binary, so anything baked in at build time
// would pin one binary to one region. The serving node reports its own region
// instead, which is the value awsgw verifies signatures against.

interface ClusterConfig {
  region: string
}

let config: ClusterConfig | null = null

function readRegion(body: unknown): string {
  if (typeof body !== "object" || body === null) {
    throw new TypeError("cluster config was not an object")
  }
  if (!("region" in body)) {
    throw new TypeError("cluster config did not include a region")
  }
  const { region } = body
  if (typeof region !== "string" || region === "") {
    throw new TypeError("cluster config did not include a region")
  }
  return region
}

// Fetched once before the app renders so every signing path can read the region
// synchronously.
export async function loadClusterConfig(): Promise<void> {
  const response = await fetch("/api/config", { cache: "no-store" })
  if (!response.ok) {
    throw new Error(`could not load cluster config: HTTP ${response.status}`)
  }
  config = { region: readRegion(await response.json()) }
}

// Throws rather than falling back to a default: signing with the wrong region
// fails as an opaque credential error, which is far harder to diagnose.
export function getRegion(): string {
  if (!config) {
    throw new Error("cluster config has not been loaded")
  }
  return config.region
}
