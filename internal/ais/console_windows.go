//go:build windows

package ais

import (
	"os"
	"syscall"
	"unsafe"
)

// enableVirtualTerminalProcessing is the Windows console flag that makes the
// terminal interpret ANSI escape sequences (colors, cursor moves) instead of
// printing them literally.
const enableVirtualTerminalProcessing = 0x0004

// init turns on ANSI escape processing for stdout and stderr so colored output
// renders correctly on legacy conhost. Windows Terminal already enables this,
// and any failure (e.g. output is redirected to a file) is silently ignored.
func init() {
	enableVirtualTerminal(os.Stdout)
	enableVirtualTerminal(os.Stderr)
}

func enableVirtualTerminal(f *os.File) {
	if f == nil {
		return
	}
	handle := syscall.Handle(f.Fd())
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	var mode uint32
	ret, _, _ := getConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode)))
	if ret == 0 {
		return // not a console (likely redirected) — nothing to enable.
	}
	_, _, _ = setConsoleMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing))
}
