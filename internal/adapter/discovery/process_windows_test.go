//go:build windows

package discovery

import (
	"reflect"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestArgvFromWindowsProcessBuffer(t *testing.T) {
	commandLine := `"C:\Program Files\CLIProxyAPI\cli-proxy-api.exe" -config "C:\Users\me\PMux Data\config.yaml"`
	encoded, err := windows.UTF16FromString(commandLine)
	if err != nil {
		t.Fatal(err)
	}
	headerSize := int(unsafe.Sizeof(windowsUnicodeString{}))
	buffer := make([]byte, headerSize+len(encoded)*2)
	text := unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[headerSize])), len(encoded))
	copy(text, encoded)
	header := (*windowsUnicodeString)(unsafe.Pointer(&buffer[0]))
	header.Length = uint16((len(encoded) - 1) * 2)
	header.MaximumLength = uint16(len(encoded) * 2)
	header.Buffer = &text[0]

	argv, err := argvFromWindowsProcessBuffer(buffer)
	if err != nil {
		t.Fatalf("argvFromWindowsProcessBuffer returned error: %v", err)
	}
	want := []string{`C:\Program Files\CLIProxyAPI\cli-proxy-api.exe`, "-config", `C:\Users\me\PMux Data\config.yaml`}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
}

func TestWindowsProcessNativeQueriesCurrentProcess(t *testing.T) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, windows.GetCurrentProcessId())
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)

	executable, err := windowsProcessImage(handle)
	if err != nil || executable == "" {
		t.Fatalf("windowsProcessImage = %q, %v", executable, err)
	}
	argv, err := windowsProcessArgv(handle)
	if err != nil || len(argv) == 0 {
		t.Fatalf("windowsProcessArgv = %#v, %v", argv, err)
	}
}
