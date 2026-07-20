//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"golang.org/x/sys/windows"
)

type nativePlatform struct{ base }

func newNative(configOverride string) *nativePlatform {
	return &nativePlatform{base: newBase(configOverride)}
}

func (p *nativePlatform) GOOS() string { return "windows" }

func (p *nativePlatform) nativeRoots() (config, state, cache, data string, err error) {
	home, err := p.home()
	if err != nil {
		return "", "", "", "", err
	}
	roaming := absoluteEnvRoot(p.getenv("APPDATA"))
	if roaming == "" {
		roaming = rootsFromHome(home, "AppData", "Roaming")
	}
	local := absoluteEnvRoot(p.getenv("LOCALAPPDATA"))
	if local == "" {
		local = rootsFromHome(home, "AppData", "Local")
	}
	return filepath.Join(roaming, "PMux"), filepath.Join(local, "PMux", "State"), filepath.Join(local, "PMux", "Cache"), filepath.Join(local, "PMux", "Data"), nil
}

func (p *nativePlatform) ConfigDir() (string, error) {
	config, _, _, _, err := p.nativeRoots()
	if err != nil {
		return "", err
	}
	return p.configDir(config), nil
}

func (p *nativePlatform) StateDir() (string, error) {
	_, state, _, _, err := p.nativeRoots()
	return state, err
}

func (p *nativePlatform) CacheDir() (string, error) {
	_, _, cache, _, err := p.nativeRoots()
	return cache, err
}

func (p *nativePlatform) DataDir() (string, error) {
	_, _, _, data, err := p.nativeRoots()
	return data, err
}

func defaultServiceBackend(context.Context) service.ServiceBackend {
	return service.BackendForeground
}

func (p *nativePlatform) OpenBrowser(ctx context.Context, url string) error {
	return p.runHelper(ctx, []string{"rundll32.exe", "rundll32"}, "url.dll,FileProtocolHandler", url)
}

func (p *nativePlatform) SetClipboard(text string) error {
	path, err := p.lookPath("clip.exe")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.CodeDependencyMissing, pmuxerr.Environment, "the Windows clipboard helper was not found")
	}
	cmd := p.command(context.Background(), path)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return helperError(err, "could not run the Windows clipboard helper")
	}
	return nil
}

func (p *nativePlatform) Shell() string {
	if shell := safeEnvironmentValue(p.getenv("COMSPEC")); shell != "" {
		return shell
	}
	return "cmd.exe"
}

func (p *nativePlatform) IsWSL() bool { return false }

func (p *nativePlatform) SecurePermissions(path string, isDir bool) error {
	if err := validateWindowsPathType(path, isDir); err != nil {
		return err
	}
	userSID, _, err := privateSIDs()
	if err != nil {
		return permissionError(err, path, "could not resolve private Windows security principals")
	}
	inheritance := ""
	if isDir {
		inheritance = "OICI"
	}
	// D:P builds a protected (inheritance-disabled) DACL granting Full Access
	// only to SYSTEM and the current user.
	sddl := "D:P(A;" + inheritance + ";FA;;;SY)(A;" + inheritance + ";FA;;;" + userSID.String() + ")"
	sd, dacl, err := daclFromSDDL(sddl)
	if err != nil {
		return permissionError(err, path, "could not create a private Windows access list")
	}
	securityInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, securityInfo, nil, nil, dacl, nil)
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))
	if err != nil {
		return permissionError(err, path, "could not apply a private Windows access list")
	}
	return p.VerifySecurePermissions(path, isDir)
}

var procConvertSDDL = windows.NewLazySystemDLL("advapi32.dll").NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")

func daclFromSDDL(sddl string) (*windows.SECURITY_DESCRIPTOR, *windows.ACL, error) {
	sddlPtr, err := windows.UTF16PtrFromString(sddl)
	if err != nil {
		return nil, nil, err
	}
	var sd *windows.SECURITY_DESCRIPTOR
	r1, _, callErr := syscall.SyscallN(procConvertSDDL.Addr(), uintptr(unsafe.Pointer(sddlPtr)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if r1 == 0 {
		if callErr != windows.ERROR_SUCCESS {
			return nil, nil, callErr
		}
		return nil, nil, errors.New("ConvertStringSecurityDescriptorToSecurityDescriptorW failed")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))
		return nil, nil, err
	}
	return sd, dacl, nil
}

func (p *nativePlatform) VerifySecurePermissions(path string, isDir bool) error {
	if err := validateWindowsPathType(path, isDir); err != nil {
		return err
	}
	userSID, systemSID, err := privateSIDs()
	if err != nil {
		return permissionError(err, path, "could not resolve private Windows security principals")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return permissionError(err, path, "could not read the Windows access list")
	}
	control, _, err := sd.Control()
	if err != nil {
		return permissionError(err, path, "could not inspect Windows access-list inheritance")
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return insecureWindowsPermissions(path, "access-list inheritance is enabled")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return permissionError(err, path, "could not read the protected Windows access list")
	}
	if dacl == nil || dacl.AceCount != 2 {
		return insecureWindowsPermissions(path, fmt.Sprintf("expected exactly 2 access entries, found %d", windowsACECount(dacl)))
	}
	seenUser, seenSystem := false, false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return permissionError(err, path, "could not inspect a Windows access entry")
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return insecureWindowsPermissions(path, "the access list contains a non-allow entry")
		}
		if ace.Mask != windows.GENERIC_ALL && ace.Mask != 0x001f01ff {
			return insecureWindowsPermissions(path, fmt.Sprintf("an approved principal does not have Full Control (mask=0x%08x)", ace.Mask))
		}
		flags := ace.Header.AceFlags &^ windows.INHERITED_ACE
		if isDir {
			if flags != 0 && flags != (windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) {
				return insecureWindowsPermissions(path, fmt.Sprintf("a directory access entry has unexpected flags 0x%02x", ace.Header.AceFlags))
			}
		} else if flags != 0 {
			return insecureWindowsPermissions(path, fmt.Sprintf("a file access entry has unexpected flags 0x%02x", ace.Header.AceFlags))
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case windows.EqualSid(sid, userSID):
			if seenUser {
				return insecureWindowsPermissions(path, "the current-user access entry is duplicated")
			}
			seenUser = true
		case windows.EqualSid(sid, systemSID):
			if seenSystem {
				return insecureWindowsPermissions(path, "the SYSTEM access entry is duplicated")
			}
			seenSystem = true
		default:
			return insecureWindowsPermissions(path, "the access list grants access to an unapproved principal")
		}
	}
	if !seenUser || !seenSystem {
		return insecureWindowsPermissions(path, "the access list does not grant Full Control to both the current user and SYSTEM")
	}
	return nil
}

func privateSIDs() (user, system *windows.SID, err error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, nil, err
	}
	defer token.Close()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	user, err = tokenUser.User.Sid.Copy()
	if err != nil {
		return nil, nil, err
	}
	system, err = windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, err
	}
	return user, system, nil
}

func validateWindowsPathType(path string, isDir bool) error {
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return permissionError(err, path, "could not inspect private Windows path")
	}
	if fileInfo.Mode()&os.ModeSymlink != 0 {
		return insecureWindowsPermissions(path, "refusing to change permissions through a symbolic link")
	}
	if isDir != fileInfo.IsDir() {
		return insecureWindowsPermissions(path, "path type does not match the requested private-path type")
	}
	if !isDir && !fileInfo.Mode().IsRegular() {
		return insecureWindowsPermissions(path, "private file path is not a regular file")
	}
	return nil
}

func windowsACECount(dacl *windows.ACL) uint16 {
	if dacl == nil {
		return 0
	}
	return dacl.AceCount
}

func insecureWindowsPermissions(path, explanation string) *pmuxerr.Error {
	return &pmuxerr.Error{
		Code:        pmuxerr.ConfigInsecurePermissions,
		Class:       pmuxerr.Environment,
		Message:     fmt.Sprintf("private path %q does not have the required protected Windows access list: %s", path, explanation),
		Explanation: explanation,
		Evidence:    []string{"path: " + path},
	}
}

func permissionError(err error, path, message string) *pmuxerr.Error {
	wrapped := pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, message)
	wrapped.Evidence = []string{"path: " + path}
	return wrapped
}
