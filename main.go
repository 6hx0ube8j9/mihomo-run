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
	exePath, err := os.Executable()
	if err != nil {
		fatalLog("Get Executable Path failed: " + err.Error())
	}
	workDir := filepath.Dir(exePath)
	os.Chdir(workDir)

	mutexName, _ := windows.UTF16PtrFromString("Local\\MihomoRunMutex_Static")
	_, err = windows.CreateMutex(nil, false, mutexName)
	if err != nil {
		if err == windows.ERROR_ALREADY_EXISTS {
			os.Exit(0) 
		}
		fatalLog("CreateMutex failed: " + err.Error())
	}

	const targetExe = "mihomo.exe"
	const lockFile = "tun_on.lock"

	if _, err := os.Stat(targetExe); os.IsNotExist(err) {
		fatalLog(fmt.Sprintf("Kernel not found: %s in %s", targetExe, workDir))
	}

	cmd := exec.Command(targetExe, "-d", ".")
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}

	if err := cmd.Start(); err != nil {
		fatalLog("Start mihomo failed: " + err.Error())
	}

	if _, err := os.Stat(lockFile); err == nil {
		go startConfigInjector("http://127.0.0.1:9090/configs")
	}

	_ = cmd.Wait()
}

func startConfigInjector(apiURL string) {
	jsonData := []byte(`{"tun":{"enable":true}}`)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 150; i++ {
		resp, err := client.Get(apiURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				time.Sleep(500 * time.Millisecond)
				req, _ := http.NewRequest("PATCH", apiURL, bytes.NewBuffer(jsonData))
				req.Header.Set("Content-Type", "application/json")
				if pr, perr := client.Do(req); perr == nil {
					pr.Body.Close()
					return
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func fatalLog(msg string) {
	f, _ := os.OpenFile("run_error.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, _ = f.WriteString(fmt.Sprintf("[%s] %s\n", timestamp, msg))
	os.Exit(1)
}
