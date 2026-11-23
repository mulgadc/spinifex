# Docs diagrams

These D2 files are the sources for the architecture diagrams on docs.mulgadc.com.
Each `<name>.d2` file renders to `../<name>.svg`.
Do not edit an SVG by hand.

## Render

You need d2 v0.7.1.
Other d2 releases give different bytes.

```sh
make diagrams                              # render all sources
scripts/render-diagrams.sh tf-bastion      # render one source
make diagrams-check                        # fail if a committed SVG is stale
```

If d2 is not on your `PATH`, set `D2_BIN` to the binary.

The render step runs `scripts/diagram-postprocess.py` after d2.
This script moves each group label to the right of its group icon, which d2 cannot do.

## Style

The diagrams use the AWS architecture diagram style: a white background, AWS group boxes and AWS icons.

- Start each source with `...@aws`.
  Use the classes in `aws.d2` and do not set colours inline.
- Use `region`, `vpc`, `az`, `public-subnet` and `private-subnet` only for Spinifex regions and customer VPCs.
- Use `datacenter` or `group` for a provider network (Exoscale, OCI, Latitude, Vultr), because it is not an AWS region.
- Use `server` for a host and `res` with an icon for a resource.
- Use `box` for a thing that has no AWS icon, for example an OVS bridge, a NIC or NATS.
- Put a fact that the page text does not give in a `note` with `near: bottom-center`.

## Icons

The repository does not store icons.
Each rendered SVG embeds the icons it uses.

The render script downloads the AWS Architecture Icons package, release 2026-07-31 (<https://aws.amazon.com/architecture/icons/>), and checks its sha256.
It keeps the package in `~/.cache/spinifex-diagrams/` and copies each icon that a source names into `icons/`, which git ignores.
AWS lets customers and partners use these icons in architecture diagrams.

Refer to an icon by its AWS file name, for example `icons/Res_Amazon-EC2_Instance_48.svg`.
For a group icon, use `icons/group/<name>`.
The script draws a group icon at 26 px in the top-left corner of a 64 px transparent canvas, because d2 sets the size of a container icon.
