// S3 lists the class only by its API name, so the console names it the way
// the storage-class docs do.
const STORAGE_CLASSES = new Map([
  ["STANDARD", "Standard"],
  ["REDUCED_REDUNDANCY", "Reduced redundancy"],
  ["STANDARD_IA", "Standard-IA"],
  ["ONEZONE_IA", "One Zone-IA"],
  ["INTELLIGENT_TIERING", "Intelligent-Tiering"],
  ["GLACIER", "Glacier Flexible Retrieval"],
  ["DEEP_ARCHIVE", "Glacier Deep Archive"],
  ["GLACIER_IR", "Glacier Instant Retrieval"],
])

export function storageClassLabel(storageClass: string): string {
  return STORAGE_CLASSES.get(storageClass) ?? storageClass
}

// The lowercased extension, or "" for a name with none or a dotfile.
export function extension(name: string): string {
  const dot = name.lastIndexOf(".")

  return dot > 0 ? name.slice(dot + 1).toLowerCase() : ""
}
