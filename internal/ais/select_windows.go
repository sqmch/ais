//go:build windows

package ais

import (
	"fmt"
	"os"
)

// interactiveSelectSupported is false on Windows: the arrow-key picker depends
// on stty, which does not exist there. --configure falls back to the numbered
// text menu in chooseOption instead.
const interactiveSelectSupported = false

// enterSelectMode is never called on Windows (guarded by interactiveSelectSupported),
// but it must exist so chooseOptionInteractive still compiles for this target.
func enterSelectMode(_ *os.File) (func(), error) {
	return func() {}, fmt.Errorf("interactive selection is not supported on windows")
}
