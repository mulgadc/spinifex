import type { SubnetCidr } from "@/lib/subnet-calculator"

interface VpcPreviewProps {
  mode: "vpc-only" | "vpc-and-more"
  vpcCidr: string
  publicSubnets: SubnetCidr[]
  privateSubnets: SubnetCidr[]
  hasIgw: boolean
}

function SubnetCard({
  label,
  cidr,
  variant,
}: {
  label: string
  cidr: string
  variant: "public" | "private"
}) {
  return (
    <div
      className={`rounded-md border px-3 py-2 text-xs ${
        variant === "public"
          ? "border-emerald-500/30 bg-emerald-500/10"
          : "border-blue-500/30 bg-blue-500/10"
      }`}
    >
      <div className="font-medium">{label}</div>
      <div className="text-muted-foreground">{cidr}</div>
    </div>
  )
}

export function VpcPreview({
  mode,
  vpcCidr,
  publicSubnets,
  privateSubnets,
  hasIgw,
}: VpcPreviewProps) {
  return (
    <div className="rounded-lg border bg-muted/30 p-4">
      <h3 className="mb-3 text-xs font-semibold tracking-wider text-muted-foreground uppercase">
        Preview
      </h3>

      {/* VPC box */}
      <div className="rounded-lg border-2 border-dashed border-foreground/20 p-4">
        <div className="mb-3 text-xs font-medium">
          VPC <span className="text-muted-foreground">({vpcCidr})</span>
        </div>

        {mode === "vpc-and-more" && (
          <div className="grid gap-4 md:grid-cols-3">
            {/* Subnets column */}
            <div className="space-y-2">
              <div className="text-xs font-medium text-muted-foreground">
                Subnets
              </div>
              {publicSubnets.map((s) => (
                <SubnetCard
                  cidr={s.cidr}
                  key={s.cidr}
                  label={s.label}
                  variant="public"
                />
              ))}
              {privateSubnets.map((s) => (
                <SubnetCard
                  cidr={s.cidr}
                  key={s.cidr}
                  label={s.label}
                  variant="private"
                />
              ))}
              {publicSubnets.length === 0 && privateSubnets.length === 0 && (
                <div className="text-xs text-muted-foreground">No subnets</div>
              )}
            </div>

            {/* Route Tables column */}
            <div className="space-y-2">
              <div className="text-xs font-medium text-muted-foreground">
                Route Tables
              </div>
              {publicSubnets.length > 0 && (
                <div className="rounded-md border border-foreground/10 bg-background px-3 py-2 text-xs">
                  <div className="font-medium">Public Route Table</div>
                  <div className="mt-1 text-muted-foreground">
                    0.0.0.0/0 → IGW
                  </div>
                  <div className="text-muted-foreground">local</div>
                </div>
              )}
              {privateSubnets.length > 0 && (
                <div className="rounded-md border border-foreground/10 bg-background px-3 py-2 text-xs">
                  <div className="font-medium">Private Route Table</div>
                  <div className="mt-1 text-muted-foreground">local</div>
                </div>
              )}
            </div>

            {/* Network Connections column */}
            <div className="space-y-2">
              <div className="text-xs font-medium text-muted-foreground">
                Network Connections
              </div>
              {hasIgw && (
                <div className="rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs">
                  <div className="font-medium">Internet Gateway</div>
                  <div className="text-muted-foreground">Attached to VPC</div>
                </div>
              )}
              {!hasIgw && (
                <div className="text-xs text-muted-foreground">
                  No internet gateway
                </div>
              )}
            </div>
          </div>
        )}

        {mode === "vpc-only" && (
          <div className="text-xs text-muted-foreground">
            VPC only — no additional resources will be created.
          </div>
        )}
      </div>
    </div>
  )
}
