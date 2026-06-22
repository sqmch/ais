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

// readSelectKey reads one logical keypress in raw mode. Arrow keys arrive as the
// ANSI escape sequences ESC [ A / ESC [ B; j/k mirror them; Enter confirms.
func readSelectKey(inFile *os.File) (selectKey, error) {
	buf := make([]byte, 3)
	n, err := inFile.Read(buf[:1])
	if err != nil {
		return selectKey{}, err
	}
	if n == 0 {
		return selectKey{kind: selectKeyNone}, nil
	}
	switch buf[0] {
	case '\r', '\n':
		return selectKey{kind: selectKeyEnter}, nil
	case 'k':
		return selectKey{kind: selectKeyUp}, nil
	case 'j':
		return selectKey{kind: selectKeyDown}, nil
	case 27: // ESC: possibly the start of an arrow-key sequence.
		m, err := inFile.Read(buf[1:3])
		if err != nil {
			return selectKey{}, err
		}
		if m >= 2 && buf[1] == '[' {
			switch buf[2] {
			case 'A':
				return selectKey{kind: selectKeyUp}, nil
			case 'B':
				return selectKey{kind: selectKeyDown}, nil
			}
		}
		return selectKey{kind: selectKeyCancel}, nil
	default:
		if buf[0] >= '1' && buf[0] <= '9' {
			return selectKey{kind: selectKeyDigit, digit: int(buf[0] - '0')}, nil
		}
	}
	return selectKey{kind: selectKeyNone}, nil
}
