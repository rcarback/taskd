// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import "syscall"

func maxRSSBytes(ru *syscall.Rusage) int64 { return int64(ru.Maxrss) }
