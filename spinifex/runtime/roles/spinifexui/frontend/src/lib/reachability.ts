// Whether a map's Internet card is reached: unknown when what decides it was
// not read, so a failed read is never drawn as a broken network.
export type Reachability = "reachable" | "unreachable" | "unknown"
