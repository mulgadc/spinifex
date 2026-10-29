import { Plus, Trash2 } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Slider } from "@/components/ui/slider"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

import { CatalogueSection, Specimen } from "./catalogue-section"

const BUTTON_VARIANTS = [
  "default",
  "secondary",
  "outline",
  "ghost",
  "destructive",
  "link",
] as const

const BUTTON_SIZES = ["xs", "sm", "default", "lg"] as const

const BADGE_VARIANTS = [
  "default",
  "secondary",
  "outline",
  "destructive",
  "ghost",
] as const

const TIME_RANGES = ["1H", "1D", "1W", "1M"]

export function ControlsSection() {
  return (
    <CatalogueSection title="Controls">
      <Specimen label="Button variants">
        {BUTTON_VARIANTS.map((variant) => (
          <Button key={variant} variant={variant}>
            {variant}
          </Button>
        ))}
        <Button disabled>disabled</Button>
      </Specimen>
      <Specimen label="Button sizes and icons">
        {BUTTON_SIZES.map((size) => (
          <Button key={size} size={size}>
            <Plus data-icon="inline-start" />
            Create {size}
          </Button>
        ))}
        <Button aria-label="Delete" size="icon" variant="destructive">
          <Trash2 />
        </Button>
      </Specimen>
      <Specimen label="Badges">
        {BADGE_VARIANTS.map((variant) => (
          <Badge key={variant} variant={variant}>
            {variant}
          </Badge>
        ))}
      </Specimen>
      <Specimen label="Text inputs">
        <div className="grid w-full max-w-xl gap-3 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-name">Name</Label>
            <Input id="design-name" placeholder="my-vpc" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-cidr">CIDR block (invalid)</Label>
            <Input aria-invalid defaultValue="10.0.0.0/33" id="design-cidr" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-disabled">Disabled</Label>
            <Input disabled id="design-disabled" value="vpc-0a1b2c3d" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-type">Instance type</Label>
            <Select defaultValue="t3.micro">
              <SelectTrigger className="w-full" id="design-type">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="t3.micro">t3.micro</SelectItem>
                <SelectItem value="t3.small">t3.small</SelectItem>
                <SelectItem value="t3.xlarge">t3.xlarge</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col gap-1.5 sm:col-span-2">
            <Label htmlFor="design-description">Description</Label>
            <Textarea
              id="design-description"
              placeholder="What this resource is for"
            />
          </div>
        </div>
      </Specimen>
      <Specimen label="Choices">
        <div className="flex items-center gap-2">
          <Switch defaultChecked id="design-switch" />
          <Label htmlFor="design-switch">Enable DNS hostnames</Label>
        </div>
        <div className="flex items-center gap-2">
          <Switch id="design-switch-off" />
          <Label htmlFor="design-switch-off">Auto-assign public IP</Label>
        </div>
        <div className="flex items-center gap-2">
          <Switch disabled id="design-switch-disabled" />
          <Label htmlFor="design-switch-disabled">Disabled</Label>
        </div>
        {/* A checkbox picks items from a list; a setting is a switch. */}
        <div className="flex items-center gap-2">
          <Checkbox defaultChecked id="design-check" />
          <Label htmlFor="design-check">sg-0a1b2c3d (web)</Label>
        </div>
        <ToggleGroup defaultValue={["1D"]} variant="outline">
          {TIME_RANGES.map((range) => (
            <ToggleGroupItem key={range} value={range}>
              {range}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        <div className="w-64">
          <Slider defaultValue={[20, 80]} />
        </div>
      </Specimen>
    </CatalogueSection>
  )
}
