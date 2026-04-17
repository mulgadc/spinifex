/*
Copyright © 2026 Mulga Defense Corporation

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/mulgadc/spinifex/cmd/installer/autoinstall"
	"github.com/mulgadc/spinifex/cmd/installer/install"
	"github.com/mulgadc/spinifex/cmd/installer/ui"
)

func main() {
	// Check for a headless autoinstall config on the boot media before
	// launching the interactive TUI. If found and enabled, run without
	// any user input then eject the USB and reboot.
	if cfg, err := autoinstall.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "autoinstall: %v\n", err)
		os.Exit(1)
	} else if cfg != nil {
		if err := install.Run(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
			os.Exit(1)
		}
		autoinstall.EjectAndReboot()
		return
	}

	// Normal interactive path.
	ttyPath := detectTTY()

	cfg, err := ui.Run(ttyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "installer: %v\n", err)
		os.Exit(1)
	}

	if err := install.Run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
		os.Exit(1)
	}
}

// detectTTY returns the TTY device path the installer should attach to.
// It first checks the SPINIFEX_CONSOLE environment variable (set by
// spinifex-init.sh after console fallback logic), then falls back to parsing
// /proc/cmdline. spinifex-init.sh may override the GRUB-set value (e.g. when
// GRUB says tty1 but no framebuffer exists, it falls back to ttyS0 and
// exports the corrected value).
func detectTTY() string {
	// Prefer the env var — spinifex-init.sh sets this after resolving the
	// actual console (including headless/framebuffer fallback).
	if val := os.Getenv("SPINIFEX_CONSOLE"); val != "" && val != "auto" {
		return "/dev/" + val
	}

	// Fall back to kernel cmdline for direct invocation without spinifex-init.
	data, err := os.ReadFile("/proc/cmdline")
	if err == nil {
		for param := range strings.FieldsSeq(string(data)) {
			if after, ok := strings.CutPrefix(param, "SPINIFEX_CONSOLE="); ok {
				val := after
				if val != "" && val != "auto" {
					return "/dev/" + val
				}
			}
		}
	}
	return "/dev/tty1"
}
