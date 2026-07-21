//go:build windows

package runtime

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// superviseChild assigns the child to a Job Object with kill-on-job-close.
// The job handle is deliberately leaked: the OS closes it when this host
// process exits — for any reason, including Task Scheduler force-ending the
// task — which then terminates the child. Without this, Task Scheduler only
// kills the action process and the child is orphaned.
func superviseChild(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	return nil
}
