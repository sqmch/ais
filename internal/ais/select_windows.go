//go:build windows

package ais

import (
	"os"
	"syscall"
	"unsafe"
)

// interactiveSelectSupported reports whether the arrow-key picker can run. On
// Windows we drive the console directly through ReadConsoleInputW + raw console
// mode, so the picker works without stty.
const interactiveSelectSupported = true

const (
	enableProcessedInput = 0x0001 // keep Ctrl+C generating an interrupt
	enableLineInput      = 0x0002 // cooked, line-buffered input (we disable this)
	enableEchoInput      = 0x0004 // echo typed characters (we disable this)
)

// Virtual-key codes for the keys the picker cares about.
const (
	vkReturn = 0x0D
	vkEscape = 0x1B
	vkUp     = 0x26
	vkDown   = 0x28
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode    = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode    = kernel32.NewProc("SetConsoleMode")
	procReadConsoleInputW = kernel32.NewProc("ReadConsoleInputW")
)

// keyEventRecord mirrors the Win32 KEY_EVENT_RECORD struct.
type keyEventRecord struct {
	bKeyDown          int32
	wRepeatCount      uint16
	wVirtualKeyCode   uint16
	wVirtualScanCode  uint16
	unicodeChar       uint16
	dwControlKeyState uint32
}

// inputRecord mirrors the Win32 INPUT_RECORD struct. We only decode KEY_EVENTs,
// but the trailing bytes must be present so ReadConsoleInputW writes a full
// record (the union is larger than keyEventRecord for some event types).
type inputRecord struct {
	eventType uint16
	_         uint16
	keyEvent  keyEventRecord
	_         [8]byte
}

const keyEventType = 0x0001

// enterSelectMode switches the console to raw mode (no line buffering, no echo)
// and returns a function that restores the previous mode.
func enterSelectMode(inFile *os.File) (func(), error) {
	handle := syscall.Handle(inFile.Fd())
	var orig uint32
	ret, _, err := procGetConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&orig)))
	if ret == 0 {
		return nil, err
	}
	// Drop line + echo so each keypress is delivered immediately; keep processed
	// input so Ctrl+C still interrupts.
	raw := (orig &^ (enableLineInput | enableEchoInput)) | enableProcessedInput
	if ret, _, err := procSetConsoleMode.Call(uintptr(handle), uintptr(raw)); ret == 0 {
		return nil, err
	}
	restore := func() {
		_, _, _ = procSetConsoleMode.Call(uintptr(handle), uintptr(orig))
	}
	return restore, nil
}

// readSelectKey blocks until a relevant key-down event arrives and maps it to a
// platform-neutral selectKey. Key-up events and unrelated console events (focus,
// window resize, mouse) are skipped.
func readSelectKey(inFile *os.File) (selectKey, error) {
	handle := syscall.Handle(inFile.Fd())
	var rec inputRecord
	var read uint32
	for {
		ret, _, err := procReadConsoleInputW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&rec)),
			1,
			uintptr(unsafe.Pointer(&read)),
		)
		if ret == 0 {
			return selectKey{}, err
		}
		if read == 0 || rec.eventType != keyEventType || rec.keyEvent.bKeyDown == 0 {
			continue
		}
		switch rec.keyEvent.wVirtualKeyCode {
		case vkUp:
			return selectKey{kind: selectKeyUp}, nil
		case vkDown:
			return selectKey{kind: selectKeyDown}, nil
		case vkReturn:
			return selectKey{kind: selectKeyEnter}, nil
		case vkEscape:
			return selectKey{kind: selectKeyCancel}, nil
		}
		switch ch := rec.keyEvent.unicodeChar; {
		case ch == 'k' || ch == 'K':
			return selectKey{kind: selectKeyUp}, nil
		case ch == 'j' || ch == 'J':
			return selectKey{kind: selectKeyDown}, nil
		case ch >= '1' && ch <= '9':
			return selectKey{kind: selectKeyDigit, digit: int(ch - '0')}, nil
		}
	}
}
