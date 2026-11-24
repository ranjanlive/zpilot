//go:build windows

package platform

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var user = syscall.NewLazyDLL("user32.dll")
var shell = syscall.NewLazyDLL("shell32.dll")
var trayWindow atomic.Uintptr
var notices = make(chan [2]string, 32)

type point struct{ X, Y int32 }
type msg struct {
	HWnd           uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Pt             point
	Private        uint32
}
type wndclass struct {
	Style                              uint32
	Proc                               uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
}
type notifyIcon struct {
	Size                uint32
	Window              uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Version             uint32
	Title               [64]uint16
	InfoFlags           uint32
	GUID                [16]byte
	BalloonIcon         uintptr
}

func utf(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
func copyUTF(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}
func Balloon(title, body string) {
	select {
	case notices <- [2]string{title, body}:
	default:
	}
	if w := trayWindow.Load(); w != 0 {
		user.NewProc("PostMessageW").Call(w, 0x8002, 0, 0)
	}
}
func Tray(items func() []MenuItem, action func(string), done <-chan struct{}) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	instance, _, _ := kernel.NewProc("GetModuleHandleW").Call(0)
	icon := makeIcon(instance)
	if icon == 0 {
		icon, _, _ = user.NewProc("LoadIconW").Call(0, 32512)
	}
	defer user.NewProc("DestroyIcon").Call(icon)
	var ni notifyIcon
	ni.Size = uint32(unsafe.Sizeof(ni))
	ni.ID = 1
	ni.Flags = 1 | 2 | 4
	ni.Callback = 0x8001
	ni.Icon = icon
	copyUTF(ni.Tip[:], "ZPilot")
	taskbar, _, _ := user.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(utf("TaskbarCreated"))))
	var wnd uintptr
	proc := syscall.NewCallback(func(h uintptr, m uint32, w, l uintptr) uintptr {
		if uintptr(m) == taskbar {
			shell.NewProc("Shell_NotifyIconW").Call(0, uintptr(unsafe.Pointer(&ni)))
			return 0
		}
		switch m {
		case 0x8001:
			if uint32(l) == 0x203 || uint32(l) == 0x405 { // double-click, or a click on a notification
				go action("open")
				return 0
			}
			if uint32(l) == 0x205 {
				menu, _, _ := user.NewProc("CreatePopupMenu").Call()
				defer user.NewProc("DestroyMenu").Call(menu)
				choices := items()
				commands := map[uintptr]string{}
				for i, it := range choices {
					flags := uintptr(0)
					if it.Separator {
						flags = 0x800
					}
					if it.Disabled {
						flags |= 3
					}
					if it.Checked {
						flags |= 8
					}
					cid := uintptr(i + 1)
					user.NewProc("AppendMenuW").Call(menu, flags, cid, uintptr(unsafe.Pointer(utf(it.Label))))
					commands[cid] = it.Action
				}
				var pt point
				user.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&pt)))
				user.NewProc("SetForegroundWindow").Call(h)
				choice, _, _ := user.NewProc("TrackPopupMenu").Call(menu, 0x100|0x2, uintptr(pt.X), uintptr(pt.Y), 0, h, 0)
				user.NewProc("PostMessageW").Call(h, 0, 0, 0)
				if op := commands[choice]; op != "" {
					go action(op)
				}
			}
			return 0
		case 0x8002:
			select {
			case n := <-notices:
				copyUTF(ni.Title[:], n[0])
				copyUTF(ni.Info[:], n[1])
				ni.Flags = 0x10
				ni.InfoFlags = 1
				shell.NewProc("Shell_NotifyIconW").Call(1, uintptr(unsafe.Pointer(&ni)))
				ni.Flags = 7
			default:
			}
			return 0
		case 0x10:
			user.NewProc("DestroyWindow").Call(h)
			return 0
		case 0x2:
			shell.NewProc("Shell_NotifyIconW").Call(2, uintptr(unsafe.Pointer(&ni)))
			trayWindow.Store(0)
			user.NewProc("PostQuitMessage").Call(0)
			return 0
		}
		v, _, _ := user.NewProc("DefWindowProcW").Call(h, uintptr(m), w, l)
		return v
	})
	name := utf("ZPilotTray")
	wc := wndclass{Proc: proc, Instance: instance, ClassName: name}
	v, _, e := user.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&wc)))
	if v == 0 {
		return fmt.Errorf("register tray window: %w", e)
	}
	wnd, _, e = user.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(utf("zpilot"))), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if wnd == 0 {
		return fmt.Errorf("create tray window: %w", e)
	}
	ni.Window = wnd
	trayWindow.Store(wnd)
	v, _, e = shell.NewProc("Shell_NotifyIconW").Call(0, uintptr(unsafe.Pointer(&ni)))
	if v == 0 {
		user.NewProc("DestroyWindow").Call(wnd)
		return fmt.Errorf("add tray icon: %w", e)
	}
	go func() { <-done; user.NewProc("PostMessageW").Call(wnd, 0x10, 0, 0) }()
	var message msg
	for {
		v, _, _ := user.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(v) <= 0 {
			break
		}
		user.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&message)))
		user.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&message)))
	}
	return nil
}
func makeIcon(instance uintptr) uintptr {
	// 32px ZPilot mark: an indigo-to-violet rounded square with a white
	// "Z", drawn in code so no image decoder or asset is needed.
	const size = 32
	mask := make([]byte, size*size/8)
	pixels := make([]byte, size*size*4) // BGRA
	inRounded := func(x, y int) bool {
		const r = 7
		cx, cy := x, y
		switch {
		case x < r && y < r:
			cx, cy = r, r
		case x > size-1-r && y < r:
			cx, cy = size-1-r, r
		case x < r && y > size-1-r:
			cx, cy = r, size-1-r
		case x > size-1-r && y > size-1-r:
			cx, cy = size-1-r, size-1-r
		default:
			return true
		}
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if !inRounded(x, y) {
				continue
			}
			i := (y*size + x) * 4
			t := float64(x+y) / float64(2*size)
			pixels[i] = byte(241 + t*(246-241))  // B
			pixels[i+1] = byte(102 + t*(92-102)) // G
			pixels[i+2] = byte(99 + t*(139-99))  // R
			pixels[i+3] = 255
			white := (y >= 8 && y <= 11 && x >= 8 && x <= 23) || (y >= 20 && y <= 23 && x >= 8 && x <= 23)
			// diagonal from (22,11) down to (9,20), about 3px thick
			if y > 11 && y < 20 {
				cx := 22 - (y-11)*13/9
				white = white || (x >= cx-2 && x <= cx+1)
			}
			if white {
				pixels[i], pixels[i+1], pixels[i+2] = 255, 255, 255
			}
		}
	}
	icon, _, _ := user.NewProc("CreateIcon").Call(instance, size, size, 1, 32, uintptr(unsafe.Pointer(&mask[0])), uintptr(unsafe.Pointer(&pixels[0])))
	return icon
}
