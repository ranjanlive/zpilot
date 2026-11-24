//go:build windows

package platform

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var moveFile = kernel.NewProc("MoveFileExW")

type Lock struct{ h syscall.Handle }

func Acquire(path string) (*Lock, error) {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if e != nil {
		return nil, fmt.Errorf("ZPilot daemon is already running or lock cannot be opened: %w", e)
	}
	return &Lock{h}, nil
}
func (l *Lock) Close() { syscall.CloseHandle(l.h) }
func Replace(from, to string) error {
	a, _ := syscall.UTF16PtrFromString(from)
	b, _ := syscall.UTF16PtrFromString(to)
	v, _, e := moveFile.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), 0x1|0x8)
	if v == 0 {
		return e
	}
	return nil
}
func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00000200}
}

// Jobs are closed on daemon death, terminating workers and their descendants.
type jobBasic struct {
	PerProcess, PerJob   int64
	Flags                uint32
	Min, Max             uintptr
	Active               uint32
	Affinity             uintptr
	Priority, Scheduling uint32
}
type ioCounters struct{ Read, Write, Other, ReadBytes, WriteBytes, OtherBytes uint64 }
type jobExtended struct {
	Basic                                          jobBasic
	IO                                             ioCounters
	ProcessMemory, JobMemory, PeakProcess, PeakJob uintptr
}

func Track(cmd *exec.Cmd) (func(), error) { return TrackLimits(cmd, Limits{}) }

// TrackLimits puts the process in a Job Object that kills the whole tree when
// released and, optionally, caps the tree's total memory and process count.
func TrackLimits(cmd *exec.Cmd, l Limits) (func(), error) {
	h, _, e := kernel.NewProc("CreateJobObjectW").Call(0, 0)
	if h == 0 {
		return nil, e
	}
	closeJob := func() { syscall.CloseHandle(syscall.Handle(h)) }
	info := jobExtended{}
	info.Basic.Flags = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if l.MemoryMB > 0 {
		info.Basic.Flags |= 0x200 // JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemory = uintptr(l.MemoryMB) << 20
	}
	if l.MaxProcesses > 0 {
		info.Basic.Flags |= 0x8 // JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.Basic.Active = uint32(l.MaxProcesses)
	}
	v, _, e := kernel.NewProc("SetInformationJobObject").Call(h, 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if v == 0 {
		closeJob()
		return nil, e
	}
	ph, e := syscall.OpenProcess(0x0100|0x0001, false, uint32(cmd.Process.Pid))
	if e != nil {
		closeJob()
		return nil, e
	}
	defer syscall.CloseHandle(ph)
	v, _, e = kernel.NewProc("AssignProcessToJobObject").Call(h, uintptr(ph))
	if v == 0 {
		closeJob()
		return nil, e
	}
	return closeJob, nil
}
func Kill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid))
	Configure(k)
	if k.Run() != nil {
		cmd.Process.Kill()
	}
}
func OpenURL(url string) error {
	p, _ := syscall.UTF16PtrFromString(url)
	verb, _ := syscall.UTF16PtrFromString("open")
	v, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW").Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(p)), 0, 0, 1)
	if v <= 32 {
		return fmt.Errorf("cannot open browser (code %d)", v)
	}
	return nil
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// SetStartup registers (or removes) a per-user login item named name that
// runs command. No administrator rights are needed.
func SetStartup(name, command string, on bool) error {
	advapi := syscall.NewLazyDLL("advapi32.dll")
	var key syscall.Handle
	rc, _, _ := advapi.NewProc("RegCreateKeyExW").Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(utf(runKey))), 0, 0, 0, 0x0002|0x0001, 0, uintptr(unsafe.Pointer(&key)), 0)
	if rc != 0 {
		return syscall.Errno(rc)
	}
	defer syscall.RegCloseKey(key)
	if !on {
		rc, _, _ = advapi.NewProc("RegDeleteValueW").Call(uintptr(key), uintptr(unsafe.Pointer(utf(name))))
		if rc == 2 {
			return nil
		}
	} else {
		value := syscall.StringToUTF16(command)
		rc, _, _ = advapi.NewProc("RegSetValueExW").Call(uintptr(key), uintptr(unsafe.Pointer(utf(name))), 0, 1, uintptr(unsafe.Pointer(&value[0])), uintptr(len(value)*2))
	}
	if rc != 0 {
		return syscall.Errno(rc)
	}
	return nil
}

// StartupEnabled reports whether the login item exists.
func StartupEnabled(name string) bool {
	var key syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, utf(runKey), 0, syscall.KEY_READ, &key) != nil {
		return false
	}
	defer syscall.RegCloseKey(key)
	var typ, size uint32
	return syscall.RegQueryValueEx(key, utf(name), nil, &typ, nil, &size) == nil
}

// StartupName is a stable login-item name for a data directory.
func StartupName(data string) string {
	return "ZPilot-" + fmt.Sprintf("%x", sha256.Sum256([]byte(data)))[:12]
}

type blob struct {
	N uint32
	P *byte
}

func crypt(b []byte, enc bool) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	in := blob{uint32(len(b)), &b[0]}
	out := blob{}
	dll := syscall.NewLazyDLL("crypt32.dll")
	name := "CryptUnprotectData"
	if enc {
		name = "CryptProtectData"
	}
	v, _, e := dll.NewProc(name).Call(uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 1, uintptr(unsafe.Pointer(&out)))
	if v == 0 {
		return nil, e
	}
	defer kernel.NewProc("LocalFree").Call(uintptr(unsafe.Pointer(out.P)))
	return append([]byte(nil), unsafe.Slice(out.P, int(out.N))...), nil
}
func Protect(b []byte) ([]byte, error)   { return crypt(b, true) }
func Unprotect(b []byte) ([]byte, error) { return crypt(b, false) }

var _ = os.ErrNotExist

func OpenTerminal(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010}
	if e := cmd.Start(); e != nil {
		return e
	}
	go cmd.Wait()
	return nil
}
