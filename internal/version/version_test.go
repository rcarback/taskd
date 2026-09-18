// SPDX-License-Identifier: AGPL-3.0-or-later

package version

import "testing"

func TestStringIsNotEmpty(t *testing.T) {
	if got := String(); got == "" {
		t.Fatal("String() returned an empty string")
	}
}
