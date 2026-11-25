# Docs diagrams

These D2 files are the sources for the architecture diagrams on docs.mulgadc.com.
Each `<name>.d2` file renders to `../<name>.svg`.
Do not edit an SVG by hand.

## Render

You need d2 v0.7.1.
Other d2 releases give different bytes.

```sh
make diagrams                              # lint, then render all sources
scripts/render-diagrams.sh tf-bastion      # lint, then render one source
scripts/render-diagrams.sh --lint          # lint only
make diagrams-check                        # lint, then fail if a committed SVG is stale
```

If d2 is not on your `PATH`, set `D2_BIN` to the binary.

The render step runs `scripts/diagram-postprocess.py` after d2.
This script moves each group label to the right of its group icon, which d2 cannot do.

## Style

The diagrams use the layout of an AWS architecture diagram: a white background, group boxes in the AWS category colours, and a resource drawn as an icon with its label below.
The icons are Lucide outlines, not AWS icons.

- Start each source with `...@style`.
- Give every node and edge a class from `style.d2`.
  A source sets no `icon` and no `style`; the lint rejects both.
  `width`, `height`, `near` and `shape: sequence_diagram` are allowed for layout.
- Use `region`, `vpc`, `az`, `public-subnet` and `private-subnet` only for Spinifex regions and customer VPCs.
- Use `datacenter` or `group` for a provider network (Exoscale, OCI, Latitude, Vultr), because it is not an AWS region.
- Use `server` for a host and a resource class for a resource.
- Use `box` for a thing that has no resource class, for example an OVS bridge, a NIC or NATS.
- Put a fact that the page text does not give in a `note` with `near: bottom-center`.

## Resource labels

A resource node always has a label, and the first line is the service name as plain text:

```d2
web: "EC2 instance\nweb-1" {class: instance}
s3: "S3 API\nPredastore" {class: bucket}
```

Write the service name before our product name, never joined to it: "S3 API" and "Predastore" on separate lines, not "Predastore S3".
A resource with no AWS service starts with a plain noun:

| Class | First line |
| --- | --- |
| `node` | "Spinifex node", or the Spinifex service it runs |
| `disk` | "Disk" |
| `client` | "Client" |
| `person` | "Operator" or "User" |
| `internet` | "Internet" |

The lint checks that each resource has a label.
It does not check the words, so a reviewer reads each label.

## Classes and icons

`icons.tsv` gives each class that draws an icon its Lucide icon, its colour and its kind (`resource` or `group`).
The icon is the one the console uses for the same resource, so one resource type has one icon everywhere.
The colour is the AWS category colour: `#ED7100` compute and containers, `#7AA116` storage, `#C925D1` databases, `#8C4FFF` networking, `#DD344C` security, `#232F3E` general.

To add a resource class:

1. Pick the Lucide icon that the console uses for the resource, or the closest one at <https://lucide.dev/icons>.
2. Add a row to `icons.tsv`.
3. Add a line to the nodes section of `style.d2` with `icon: icons/<class>.svg`.

The lint fails when `icons.tsv` and `style.d2` disagree.

## Icons

The repository does not store icons.
Each rendered SVG embeds the icons it uses.

The render script downloads `lucide-static` 1.78.0 from the npm registry and checks its sha256.
It keeps the package in `~/.cache/spinifex-diagrams/` and writes one icon per class into `icons/`, which git ignores.
Each icon is drawn in its class colour.
A group icon is drawn at 22 px in the top-left corner of a 64 px transparent canvas, because d2 sets the size of a container icon.

Lucide is under the ISC licence.
Each embedded icon keeps the `@license lucide-static` comment from the package.
