// Where each AWS region is, so a region code can be shown with a flag and a
// city. A cluster may name its region anything, so an unknown code is normal.

export type CountryCode =
  | "AE"
  | "AU"
  | "BH"
  | "BR"
  | "CA"
  | "CH"
  | "CN"
  | "DE"
  | "ES"
  | "FR"
  | "GB"
  | "HK"
  | "ID"
  | "IE"
  | "IL"
  | "IN"
  | "IT"
  | "JP"
  | "KR"
  | "MX"
  | "MY"
  | "NZ"
  | "SE"
  | "SG"
  | "TH"
  | "TW"
  | "US"
  | "ZA"

export interface RegionLocation {
  country: CountryCode
  city: string
}

const AWS_REGIONS = new Map<string, RegionLocation>([
  ["af-south-1", { country: "ZA", city: "Cape Town" }],
  ["ap-east-1", { country: "HK", city: "Hong Kong" }],
  ["ap-east-2", { country: "TW", city: "Taipei" }],
  ["ap-northeast-1", { country: "JP", city: "Tokyo" }],
  ["ap-northeast-2", { country: "KR", city: "Seoul" }],
  ["ap-northeast-3", { country: "JP", city: "Osaka" }],
  ["ap-south-1", { country: "IN", city: "Mumbai" }],
  ["ap-south-2", { country: "IN", city: "Hyderabad" }],
  ["ap-southeast-1", { country: "SG", city: "Singapore" }],
  ["ap-southeast-2", { country: "AU", city: "Sydney" }],
  ["ap-southeast-3", { country: "ID", city: "Jakarta" }],
  ["ap-southeast-4", { country: "AU", city: "Melbourne" }],
  ["ap-southeast-5", { country: "MY", city: "Malaysia" }],
  ["ap-southeast-6", { country: "NZ", city: "New Zealand" }],
  ["ap-southeast-7", { country: "TH", city: "Thailand" }],
  ["ca-central-1", { country: "CA", city: "Central" }],
  ["ca-west-1", { country: "CA", city: "Calgary" }],
  ["cn-north-1", { country: "CN", city: "Beijing" }],
  ["cn-northwest-1", { country: "CN", city: "Ningxia" }],
  ["eu-central-1", { country: "DE", city: "Frankfurt" }],
  ["eu-central-2", { country: "CH", city: "Zurich" }],
  ["eu-north-1", { country: "SE", city: "Stockholm" }],
  ["eu-south-1", { country: "IT", city: "Milan" }],
  ["eu-south-2", { country: "ES", city: "Spain" }],
  ["eu-west-1", { country: "IE", city: "Ireland" }],
  ["eu-west-2", { country: "GB", city: "London" }],
  ["eu-west-3", { country: "FR", city: "Paris" }],
  ["il-central-1", { country: "IL", city: "Tel Aviv" }],
  ["me-central-1", { country: "AE", city: "UAE" }],
  ["me-south-1", { country: "BH", city: "Bahrain" }],
  ["mx-central-1", { country: "MX", city: "Mexico" }],
  ["sa-east-1", { country: "BR", city: "São Paulo" }],
  ["us-east-1", { country: "US", city: "N. Virginia" }],
  ["us-east-2", { country: "US", city: "Ohio" }],
  ["us-gov-east-1", { country: "US", city: "GovCloud (US-East)" }],
  ["us-gov-west-1", { country: "US", city: "GovCloud (US-West)" }],
  ["us-west-1", { country: "US", city: "N. California" }],
  ["us-west-2", { country: "US", city: "Oregon" }],
])

export function regionLocation(
  region: string | undefined,
): RegionLocation | undefined {
  return region ? AWS_REGIONS.get(region.toLowerCase()) : undefined
}
