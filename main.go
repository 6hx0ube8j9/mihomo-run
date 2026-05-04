package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func main() {
	exePath, _ := os.Executable()
	workDir := filepath.Dir(exePath)
	os.Chdir(workDir)

	mutexName, _ := windows.UTF16PtrFromString("Local\\MihomoRun_GlobalMutex")
	_, err := windows.CreateMutex(nil, false, mutexName)
	if err != nil && err == windows.ERROR_ALREADY_EXISTS {
		os.Exit(0)
	}

	exec.Command("tasklist").Run() 
	exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T").Run()
	time.Sleep(500 * time.Millisecond)

	job, _ := windows.CreateJobObject(nil, nil)
	if job != 0 {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
			BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
				LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			},
		}
		_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(job),
			uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(windows.Pointer(&info)),
			uintptr(windows.SizeofJobObjectExtendedLimitInformation),
		)
	}

	targetExe := filepath.Join(workDir, "mihomo.exe")
	if _, err := os.Stat(targetExe); os.IsNotExist(err) {
		fatalLog("Kernel not found")
	}

	cmd := exec.Command(targetExe, "-d", workDir)
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
	}

	if err := cmd.Start(); err != nil {
		fatalLog(err.Error())
	}

	if job != 0 {
		hProcess, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		_ = windows.AssignProcessToJobObject(job, hProcess)
		windows.CloseHandle(hProcess)
	}

	if _, err := os.Stat(filepath.Join(workDir, "tun_on.lock")); err == nil {
		go func() {
			api := "http://127.0.0.1:9090/configs"
			client := &http.Client{Timeout: 3 * time.Second}
			payload := []byte(`{"tun":{"enable":true}}`)
			for i := 0; i < 100; i++ {
				resp, err := client.Get(api)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						time.Sleep(1 * time.Second)
						req, _ := http.NewRequest("PATCH", api, bytes.NewBuffer(payload))
						req.Header.Set("Content-Type", "application/json")
						if pr, perr := client.Do(req); perr == nil {
							pr.Body.Close()
							return
						}
					}
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()
	}

	_ = cmd.Wait()
}

func fatalLog(msg string) {
	f, _ := os.OpenFile("run_error.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	_, _ = f.WriteString(fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05"), msg))
	os.Exit(1)
}
