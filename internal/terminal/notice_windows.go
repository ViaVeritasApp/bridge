package terminal

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procWTSSendMessage = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSSendMessageW")

// showNotice shows a message box on the console session's desktop
// (WTSSendMessage), which closes by itself after a minute and never waits.
func showNotice(title, text string) error {
	session := windows.WTSGetActiveConsoleSessionId()
	if session == 0xFFFFFFFF {
		return errors.New("nobody is signed in")
	}
	t, err := windows.UTF16FromString(title)
	if err != nil {
		return err
	}
	m, err := windows.UTF16FromString(text)
	if err != nil {
		return err
	}
	const mbIconInformation, mbSetForeground = 0x40, 0x10000
	var response uint32
	ok, _, callErr := procWTSSendMessage.Call(
		0, // WTS_CURRENT_SERVER_HANDLE
		uintptr(session),
		uintptr(unsafe.Pointer(&t[0])), uintptr(len(t)*2),
		uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)*2),
		mbIconInformation|mbSetForeground,
		60, // seconds before it closes by itself
		uintptr(unsafe.Pointer(&response)),
		0, // don't wait
	)
	if ok == 0 {
		return callErr
	}
	return nil
}
