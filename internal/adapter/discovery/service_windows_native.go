//go:build windows

package discovery

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	coinitApartmentThreaded = 0x2
	clsctxInprocServer       = 0x1
	dispatchMethod          = 0x1
	dispatchPropertyGet     = 0x2
	taskEnumHidden          = 0x1
	taskActionExec          = 0

	variantEmpty    = 0
	variantI4       = 3
	variantBSTR     = 8
	variantDispatch = 9
	variantInt      = 22
)

var (
	ole32                    = windows.NewLazySystemDLL("ole32.dll")
	oleaut32                 = windows.NewLazySystemDLL("oleaut32.dll")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
	procCoCreateInstance     = ole32.NewProc("CoCreateInstance")
	procSysAllocString       = oleaut32.NewProc("SysAllocString")
	procSysFreeString        = oleaut32.NewProc("SysFreeString")
	procSysStringLen         = oleaut32.NewProc("SysStringLen")
	procVariantClear         = oleaut32.NewProc("VariantClear")
	classTaskScheduler       = windows.GUID{Data1: 0x0F87369F, Data2: 0xA4E5, Data3: 0x4CFC, Data4: [8]byte{0xBD, 0x3E, 0x73, 0xE6, 0x15, 0x45, 0x72, 0xDD}}
	interfaceDispatch        = windows.GUID{Data1: 0x00020400, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

// NativeScheduledTaskSource uses Task Scheduler 2.0 COM Automation in read-only
// mode. It performs no registration or lifecycle method.
type NativeScheduledTaskSource struct{}

func (NativeScheduledTaskSource) Tasks(ctx context.Context, limit int) ([]ScheduledTask, error) {
	if limit <= 0 || limit > defaultScheduledTaskLimit {
		limit = defaultScheduledTaskLimit
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	initialized, err := initializeCOM()
	if err != nil {
		return nil, err
	}
	if initialized {
		defer procCoUninitialize.Call()
	}

	serviceObject, err := createDispatch(classTaskScheduler)
	if err != nil {
		return nil, fmt.Errorf("create Schedule.Service: %w", err)
	}
	defer serviceObject.release()
	if value, err := serviceObject.invoke("Connect", dispatchMethod); err != nil {
		return nil, fmt.Errorf("connect Task Scheduler service: %w", err)
	} else {
		value.clear()
	}

	root, err := serviceObject.dispatch("GetFolder", dispatchMethod, `\`)
	if err != nil {
		return nil, fmt.Errorf("open Task Scheduler root folder: %w", err)
	}
	defer root.release()
	result := make([]ScheduledTask, 0, limit)
	inspectedTasks := 0
	visitedFolders := 0
	if err := walkScheduledTaskFolder(ctx, root, limit, &inspectedTasks, &visitedFolders, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func walkScheduledTaskFolder(ctx context.Context, folder *automationDispatch, limit int, inspectedTasks, visitedFolders *int, result *[]ScheduledTask) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if *inspectedTasks >= limit || *visitedFolders >= defaultScheduledTaskLimit {
		return nil
	}
	*visitedFolders++

	if collection, err := folder.dispatch("GetTasks", dispatchMethod, int32(taskEnumHidden)); err == nil {
		count, countErr := collection.integer("Count", dispatchPropertyGet)
		if countErr == nil && count > 0 {
			for offset := range int(count) {
				if *inspectedTasks >= limit {
					break
				}
				if err := ctx.Err(); err != nil {
					collection.release()
					return err
				}
				*inspectedTasks++
				registered, itemErr := collection.dispatch("Item", dispatchPropertyGet, int32(offset+1))
				if itemErr != nil {
					continue
				}
				task, ok := readScheduledTask(registered)
				registered.release()
				if ok {
					*result = append(*result, task)
				}
			}
		}
		collection.release()
	}

	if *inspectedTasks >= limit || *visitedFolders >= defaultScheduledTaskLimit {
		return nil
	}
	folders, err := folder.dispatch("GetFolders", dispatchMethod, int32(0))
	if err != nil {
		return nil
	}
	defer folders.release()
	count, err := folders.integer("Count", dispatchPropertyGet)
	if err != nil || count <= 0 {
		return nil
	}
	for offset := range int(count) {
		if *inspectedTasks >= limit || *visitedFolders >= defaultScheduledTaskLimit {
			break
		}
		child, itemErr := folders.dispatch("Item", dispatchPropertyGet, int32(offset+1))
		if itemErr != nil {
			continue
		}
		walkErr := walkScheduledTaskFolder(ctx, child, limit, inspectedTasks, visitedFolders, result)
		child.release()
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

func readScheduledTask(registered *automationDispatch) (ScheduledTask, bool) {
	identity, err := registered.text("Name", dispatchPropertyGet)
	if err != nil || identity == "" {
		return ScheduledTask{}, false
	}
	definitionPath, _ := registered.text("Path", dispatchPropertyGet)
	stateValue, _ := registered.integer("State", dispatchPropertyGet)
	definition, err := registered.dispatch("Definition", dispatchPropertyGet)
	if err != nil {
		return ScheduledTask{}, false
	}
	defer definition.release()

	description := ""
	if registration, err := definition.dispatch("RegistrationInfo", dispatchPropertyGet); err == nil {
		description, _ = registration.text("Description", dispatchPropertyGet)
		registration.release()
	}
	actions, err := definition.dispatch("Actions", dispatchPropertyGet)
	if err != nil {
		return ScheduledTask{}, false
	}
	defer actions.release()
	actionCount, err := actions.integer("Count", dispatchPropertyGet)
	if err != nil {
		return ScheduledTask{}, false
	}
	if actionCount > maxScheduledActions {
		actionCount = maxScheduledActions
	}
	if actionCount < 0 {
		actionCount = 0
	}

	execActions := make([]ScheduledExecAction, 0, actionCount)
	for offset := range int(actionCount) {
		action, err := actions.dispatch("Item", dispatchPropertyGet, int32(offset+1))
		if err != nil {
			continue
		}
		actionType, typeErr := action.integer("Type", dispatchPropertyGet)
		if typeErr == nil && actionType == taskActionExec {
			executable, pathErr := action.text("Path", dispatchPropertyGet)
			if pathErr == nil && executable != "" {
				arguments, _ := action.text("Arguments", dispatchPropertyGet)
				workingDirectory, _ := action.text("WorkingDirectory", dispatchPropertyGet)
				execActions = append(execActions, ScheduledExecAction{
					Executable:       executable,
					Arguments:        arguments,
					WorkingDirectory: workingDirectory,
				})
			}
		}
		action.release()
	}
	if len(execActions) == 0 {
		return ScheduledTask{}, false
	}
	return ScheduledTask{
		Identity:    identity,
		Definition:  definitionPath,
		Description: description,
		State:       taskStateFromCOM(stateValue),
		Actions:     execActions,
	}, true
}

func taskStateFromCOM(value int64) TaskState {
	// TASK_STATE: Unknown=0, Disabled=1, Queued=2, Ready=3, Running=4.
	switch value {
	case 1:
		return TaskStateDisabled
	case 2:
		return TaskStateQueued
	case 3:
		return TaskStateReady
	case 4:
		return TaskStateRunning
	default:
		return TaskStateUnknown
	}
}

func initializeCOM() (bool, error) {
	hresult, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded)
	switch uint32(hresult) {
	case 0:
		return true, nil
	case 1: // S_FALSE: already initialized on this thread; balance the call.
		return true, nil
	case 0x80010106: // RPC_E_CHANGED_MODE: COM is already usable in another mode.
		return false, nil
	default:
		return false, hresultError("CoInitializeEx", hresult)
	}
}

func createDispatch(classID windows.GUID) (*automationDispatch, error) {
	var object *automationDispatch
	hresult, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&classID)),
		0,
		clsctxInprocServer,
		uintptr(unsafe.Pointer(&interfaceDispatch)),
		uintptr(unsafe.Pointer(&object)),
	)
	if hresultFailed(hresult) {
		return nil, hresultError("CoCreateInstance", hresult)
	}
	if object == nil {
		return nil, fmt.Errorf("CoCreateInstance returned a nil IDispatch")
	}
	return object, nil
}

type automationDispatch struct {
	vtable *automationDispatchVTable
}

type automationDispatchVTable struct {
	queryInterface   uintptr
	addRef           uintptr
	release          uintptr
	getTypeInfoCount uintptr
	getTypeInfo      uintptr
	getIDsOfNames    uintptr
	invoke           uintptr
}

type automationVariant struct {
	variantType uint16
	reserved1   uint16
	reserved2   uint16
	reserved3   uint16
	value       int64
}

type dispatchParameters struct {
	arguments     *automationVariant
	namedArguments *int32
	argumentCount uint32
	namedCount    uint32
}

type exceptionInfo struct {
	code             uint16
	reserved         uint16
	source           uintptr
	description      uintptr
	helpFile         uintptr
	helpContext      uint32
	reservedPointer  uintptr
	deferredFillIn   uintptr
	scode            int32
}

func (d *automationDispatch) release() {
	if d != nil && d.vtable != nil {
		syscall.SyscallN(d.vtable.release, uintptr(unsafe.Pointer(d)))
	}
}

func (d *automationDispatch) invoke(name string, flags uint16, input ...any) (automationVariant, error) {
	dispatchID, err := d.dispatchID(name)
	if err != nil {
		return automationVariant{}, err
	}
	arguments := make([]automationVariant, len(input))
	cleanups := make([]func(), 0, len(input))
	for index, value := range input {
		variant, cleanup, err := inputVariant(value)
		if err != nil {
			for _, release := range cleanups {
				release()
			}
			return automationVariant{}, err
		}
		arguments[len(input)-1-index] = variant
		if cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
	}
	defer func() {
		for _, release := range cleanups {
			release()
		}
	}()

	parameters := dispatchParameters{argumentCount: uint32(len(arguments))}
	if len(arguments) > 0 {
		parameters.arguments = &arguments[0]
	}
	var result automationVariant
	var exception exceptionInfo
	var argumentError uint32
	var iidNull windows.GUID
	hresult, _, _ := syscall.SyscallN(
		d.vtable.invoke,
		uintptr(unsafe.Pointer(d)),
		uintptr(dispatchID),
		uintptr(unsafe.Pointer(&iidNull)),
		0,
		uintptr(flags),
		uintptr(unsafe.Pointer(&parameters)),
		uintptr(unsafe.Pointer(&result)),
		uintptr(unsafe.Pointer(&exception)),
		uintptr(unsafe.Pointer(&argumentError)),
	)
	if hresultFailed(hresult) {
		result.clear()
		return automationVariant{}, hresultError(name, hresult)
	}
	return result, nil
}

func (d *automationDispatch) dispatchID(name string) (int32, error) {
	wideName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	namePointer := wideName
	var dispatchID int32
	var iidNull windows.GUID
	hresult, _, _ := syscall.SyscallN(
		d.vtable.getIDsOfNames,
		uintptr(unsafe.Pointer(d)),
		uintptr(unsafe.Pointer(&iidNull)),
		uintptr(unsafe.Pointer(&namePointer)),
		1,
		0,
		uintptr(unsafe.Pointer(&dispatchID)),
	)
	runtime.KeepAlive(wideName)
	if hresultFailed(hresult) {
		return 0, hresultError("GetIDsOfNames("+name+")", hresult)
	}
	return dispatchID, nil
}

func (d *automationDispatch) dispatch(name string, flags uint16, input ...any) (*automationDispatch, error) {
	value, err := d.invoke(name, flags, input...)
	if err != nil {
		return nil, err
	}
	if value.variantType != variantDispatch || value.value == 0 {
		value.clear()
		return nil, fmt.Errorf("%s returned VARIANT type %d, want VT_DISPATCH", name, value.variantType)
	}
	object := (*automationDispatch)(unsafe.Pointer(uintptr(value.value)))
	value.variantType = variantEmpty
	value.value = 0
	return object, nil
}

func (d *automationDispatch) text(name string, flags uint16, input ...any) (string, error) {
	value, err := d.invoke(name, flags, input...)
	if err != nil {
		return "", err
	}
	defer value.clear()
	if value.variantType != variantBSTR {
		return "", fmt.Errorf("%s returned VARIANT type %d, want VT_BSTR", name, value.variantType)
	}
	if value.value == 0 {
		return "", nil
	}
	length, _, _ := procSysStringLen.Call(uintptr(value.value))
	characters := unsafe.Slice((*uint16)(unsafe.Pointer(uintptr(value.value))), int(length))
	return windows.UTF16ToString(characters), nil
}

func (d *automationDispatch) integer(name string, flags uint16, input ...any) (int64, error) {
	value, err := d.invoke(name, flags, input...)
	if err != nil {
		return 0, err
	}
	defer value.clear()
	if value.variantType != variantI4 && value.variantType != variantInt {
		return 0, fmt.Errorf("%s returned VARIANT type %d, want VT_I4", name, value.variantType)
	}
	return int64(int32(value.value)), nil
}

func inputVariant(value any) (automationVariant, func(), error) {
	switch typed := value.(type) {
	case string:
		wide, err := windows.UTF16PtrFromString(typed)
		if err != nil {
			return automationVariant{}, nil, err
		}
		bstr, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(wide)))
		runtime.KeepAlive(wide)
		if bstr == 0 {
			return automationVariant{}, nil, fmt.Errorf("SysAllocString failed")
		}
		return automationVariant{variantType: variantBSTR, value: int64(bstr)}, func() { procSysFreeString.Call(bstr) }, nil
	case int:
		return automationVariant{variantType: variantI4, value: int64(int32(typed))}, nil, nil
	case int32:
		return automationVariant{variantType: variantI4, value: int64(typed)}, nil, nil
	default:
		return automationVariant{}, nil, fmt.Errorf("unsupported Automation argument type %T", value)
	}
}

func (v *automationVariant) clear() {
	if v.variantType != variantEmpty {
		procVariantClear.Call(uintptr(unsafe.Pointer(v)))
		v.variantType = variantEmpty
		v.value = 0
	}
}

func hresultFailed(value uintptr) bool { return int32(uint32(value)) < 0 }

func hresultError(operation string, value uintptr) error {
	return fmt.Errorf("%s failed with HRESULT 0x%08X", operation, uint32(value))
}
