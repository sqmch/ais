//go:build !windows

package ais

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// interactiveSelectSupported reports whether the arrow-key picker can run on
// this platform. It relies on stty, which is POSIX-only, so it is enabled on
// every non-Windows target.
const interactiveSelectSupported = true

func enterSelectMode(inFile *os.File) (func(), error) {
	stateCmd := exec.Command("stty", "-g")
	stateCmd.Stdin = inFile
	state, err := stateCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to read terminal state: %w", err)
	}
	orig := strings.TrimSpace(string(state))

	// Disable canonical mode so arrow keys and single-digit picks are read immediately,
	// but keep normal output processing so line rendering stays aligned in terminals.
	modeCmd := exec.Command("stty", "-icanon", "-echo", "min", "1", "time", "0")
	modeCmd.Stdin = inFile
	if err := modeCmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to enter selection mode: %w", err)
	}

	restore := func() {
		restoreCmd := exec.Command("stty", orig)
		restoreCmd.Stdin = inFile
		_ = restoreCmd.Run()
	}
	return restore, nil
}
