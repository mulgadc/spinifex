---
title: "Brand the Console"
seoTitle: "Brand the Spinifex Console With Your Name — Spinifex Docs"
description: "Turn on console branding, then set the product name, logos, favicon, primary colours and Docs link that every user of the cluster sees, from Settings."
category: "Admin"
tags:
  - console
  - branding
  - white-label
resources:
  - title: "Spinifex Admin CLI"
    url: "/docs/spinifex-admin-cli"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Brand the Console

## Overview

A cluster that hosts the console for its own customers can show its own brand in place of the built-in one.
The cluster administrator sets the brand once, on Settings → Branding, and every console on every node shows it from the login page on.
No rebuild and no restart is needed.

You can set:

| Value | Where it shows | Empty means |
|---|---|---|
| Product name, up to 40 characters | The sidebar, the login page and every page title | Spinifex in the sidebar, Mulga in titles |
| Logo | The sidebar header | The built-in mark |
| Dark-theme logo | The sidebar header in the dark theme | The logo |
| Favicon | The browser tab | The built-in favicon |
| Primary colour, light theme | Buttons, links and the focus ring | The built-in colour |
| Primary colour, dark theme | The same, in the dark theme | The built-in colour |
| Docs link | The sidebar's Docs entry | The built-in documentation; you can also hide the link |

Images are PNG, WebP or SVG, up to 128 KiB for a logo and 32 KiB for the favicon.
The gateway checks the file's own bytes, not only its name.
A Docs link must be an `https://` URL.

The brand is one record in the cluster's NATS KV store, in the `spinifex-console-branding` bucket.
Each gateway keeps a copy in memory and reads the store again every 10 seconds, so a page loaded on any node within a minute of a save shows it.
A page that is already open shows the change on its next load.
If a gateway cannot reach the store, it keeps serving the last brand it read.

## Prerequisites

- Shell access to every node, to edit `/etc/spinifex/spinifex.toml`.
- A sign-in to the account's `admin` user, or to an admin principal that holds `spinifex:GetConsoleBranding` and `spinifex:PutConsoleBranding`.

## Instructions

### Turn branding on

Add this to `/etc/spinifex/spinifex.toml` on every node, then restart the gateway on that node:

```toml
[console]
branding_enabled = true
```

```sh
sudo systemctl restart spinifex-awsgw
```

While the setting is off, every console shows the built-in brand and Settings → Branding is not offered.
A brand saved earlier is kept, and shows again when you turn the setting back on.

### Set the brand

1. Sign in to the console as the account's administrator.
2. Open **Settings → Branding** in the sidebar.
3. Set the values you want. Leave a value empty to keep its default.
4. Check the **Preview**, which shows the sidebar header, a login card, a button and a link in both themes.
5. Click **Save**.

Each colour field lists the five places the colour is used and the contrast it gets in each.
A colour is refused unless every one meets the minimum:

| Use | Minimum |
|---|---|
| Button text on the colour | 4.5:1 |
| Button text on the colour as the button is hovered | 4.5:1 |
| The colour as link text on the page | 4.5:1 |
| The colour as link text on a card | 4.5:1 |
| The colour as the focus ring | 3:1 |

The light theme puts white text on its buttons and the dark theme puts near-black text, so a colour that suits one theme seldom suits the other.
Set each theme's colour on its own; a theme you leave empty keeps its default.

### Reset the brand

Click **Reset to default** and confirm.
The name, colours, Docs link and every image are removed in one change, and every user sees the built-in brand from their next page load.

### Grant an admin principal

The account's `admin` user can always change the brand.
A principal made with `spx admin principal create` holds the admin methods that existed on the day it was made, so a principal made before this release cannot read or change the brand.
Run the command again with the same name to give it every current method, these two included:

```sh
spx admin principal create <name>
```

This replaces the principal's access key, so give the new key to whoever uses it.
To grant only branding, pass `--grant GetConsoleBranding,PutConsoleBranding`.

## Troubleshooting

**Settings → Branding is not in the sidebar.**
Branding is off on the node you signed in to, or you are not signed in to the administrator account.
Check that `branding_enabled = true` is under `[console]` on that node and that its gateway was restarted.

**Save fails with `UnsupportedOperation`.**
The gateway that answered has branding off.
Set `branding_enabled = true` on every node, not only one.

**The page says the principal may not read or change the branding.**
The principal lacks `spinifex:GetConsoleBranding` and `spinifex:PutConsoleBranding`.
See [Grant an admin principal](#grant-an-admin-principal).

**Save fails with `PreconditionFailedException`.**
Someone saved the brand after you opened the page.
Reload the page, which shows their change, and make yours again.

**Save fails with `InvalidParameterValue`.**
The message names the field and the reason, such as a colour that fails a contrast check or an image that is not a PNG, WebP or SVG file.

**A node still shows the old brand.**
Load the page again: an open page keeps the brand it loaded with.
If a new page still shows the old brand after a minute, that node's gateway cannot reach the NATS store, and it shows its last copy until it reconnects.

**The console shows the built-in colours for a moment.**
The console waits up to 2 seconds for the brand before it renders.
A slower gateway gives the built-in colours first, and the brand applies when it arrives.
