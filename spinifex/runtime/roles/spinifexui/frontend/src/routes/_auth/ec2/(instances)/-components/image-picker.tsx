import type { Image } from "@aws-sdk/client-ec2"

import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  type Architecture,
  buildImageCatalog,
  distroLabel,
  parseImageName,
} from "@/lib/os-images"

import { ChoiceTile, ChoiceTiles } from "./choice-tile"
import { OsMark } from "./os-mark"

const OTHER = "other"

interface ImagePickerProps {
  images: readonly Image[]
  arch: Architecture | undefined
  value: string
  onChange: (imageId: string) => void
  invalid?: boolean
}

// Logo tiles pick a distribution at its newest version for the architecture;
// the select beneath offers its other versions. Images whose names do not
// read as <distro>-<version>-<arch> sit behind "Other images".
export function ImagePicker({
  images,
  arch,
  value,
  onChange,
  invalid,
}: ImagePickerProps) {
  const catalog = buildImageCatalog(images, arch)
  const selected = images.find((image) => image.ImageId === value)
  const parsed = parseImageName(selected?.Name)

  let selectedDistro: string | undefined

  if (parsed && catalog.distros.some((g) => g.distro === parsed.distro)) {
    selectedDistro = parsed.distro
  } else if (selected && catalog.other.includes(selected)) {
    selectedDistro = OTHER
  }

  const group = catalog.distros.find((g) => g.distro === selectedDistro)

  if (catalog.distros.length === 0 && catalog.other.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No images are available{arch ? ` for ${arch}` : ""}.
      </p>
    )
  }

  return (
    <div className="flex flex-col gap-3">
      <ChoiceTiles label="Operating system">
        {catalog.distros.map((g) => (
          <ChoiceTile
            checked={g.distro === selectedDistro}
            icon={<OsMark distro={g.distro} />}
            key={g.distro}
            label={distroLabel(g.distro)}
            onSelect={() => {
              const latest = g.versions[0]?.image.ImageId

              if (latest) {
                onChange(latest)
              }
            }}
          />
        ))}
        {catalog.other.length > 0 && (
          <ChoiceTile
            checked={selectedDistro === OTHER}
            icon={<OsMark />}
            label="Other images"
            onSelect={() => {
              const first = catalog.other[0]?.ImageId

              if (first) {
                onChange(first)
              }
            }}
          />
        )}
      </ChoiceTiles>

      {group && (
        <Field>
          <FieldTitle>
            <label htmlFor="imageVersion">Version</label>
          </FieldTitle>
          <Select
            onValueChange={(next) => {
              onChange(next ?? "")
            }}
            value={value}
          >
            <SelectTrigger
              aria-invalid={invalid}
              className="w-full"
              id="imageVersion"
            >
              <SelectValue>
                {parsed && `${distroLabel(parsed.distro)} ${parsed.version}`}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              {group.versions.map(({ image, version }, index) => (
                <SelectItem key={image.ImageId} value={image.ImageId ?? ""}>
                  {distroLabel(group.distro)} {version}
                  {index === 0 ? " (latest)" : ""}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <ImageDescription image={selected} />
        </Field>
      )}

      {selectedDistro === OTHER && (
        <Field>
          <FieldTitle>
            <label htmlFor="imageId">Image</label>
          </FieldTitle>
          <Select
            onValueChange={(next) => {
              onChange(next ?? "")
            }}
            value={value}
          >
            <SelectTrigger
              aria-invalid={invalid}
              className="w-full"
              id="imageId"
            >
              <SelectValue>{selected?.Name ?? selected?.ImageId}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {catalog.other.map((image) => (
                <SelectItem key={image.ImageId} value={image.ImageId ?? ""}>
                  {image.Name ?? image.ImageId}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <ImageDescription image={selected} />
        </Field>
      )}
    </div>
  )
}

function ImageDescription({ image }: { image: Image | undefined }) {
  if (!image) {
    return null
  }

  return (
    <FieldDescription>
      <span className="font-mono">{image.ImageId}</span>
      {image.Architecture && ` · ${image.Architecture}`}
      {image.BootMode && ` · ${image.BootMode}`}
    </FieldDescription>
  )
}
