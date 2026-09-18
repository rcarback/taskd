// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version reports the build version of taskd.
package version

// Version is set at build time with -ldflags.
var Version = "dev"

// String returns the build version.
func String() string { return Version }
