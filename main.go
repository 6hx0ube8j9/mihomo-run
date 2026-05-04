package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func isProcessRunning(name string) bool {
	h, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if h == 0 { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	currPid := uint32(os.Getpid())
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		pname := windows.UTF16ToString(pe.ExeFile[:])
		if strings.EqualFold(pname, name) && pe.ProcessID != currPid {
			return true
		}
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

func main() {
	p, _ := os.Executable()
	d := filepath.Dir(p)
	os.Chdir(d)

	if isProcessRunning(filepath.Base(p)) {
		os.Exit(0)
	}

	exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T").Run()
	time.Sleep(500 * time.Millisecond)

	job, _ := windows.CreateJobObject(nil, nil)
	if job != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(job),
			uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)),
			uintptr(uint32(unsafe.Sizeof(info))),
		)
	}

	target := filepath.Join(d, "mihomo.exe")
	if _, err := os.Stat(target); os.IsNotExist(err) {
		os.Exit(1)
	}

	cmd := exec.Command(target, "-d", d)
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
	}

	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}

	if job != 0 {
		h, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}

	if _, err := os.Stat(filepath.Join(d, "tun_on.lock")); err == nil {
		go func() {
			api := "http://127.0.0.1:9090/configs"
			c := &http.Client{Timeout: 3 * time.Second}
			b := []byte(`{"tun":{"enable":true}}`)
			for i := 0; i < 100; i++ {
				r, err := c.Get(api)
				if err == nil {
					r.Body.Close()
					if r.StatusCode == 200 {
						time.Sleep(time.Second)
						req, _ := http.NewRequest("PATCH", api, bytes.NewBuffer(b))
						req.Header.Set("Content-Type", "application/json")
						if pr, perr := c.Do(req); perr == nil {
							pr.Body.Close()
							return
						}
					}
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()
	}

	cmd.Wait()
}
