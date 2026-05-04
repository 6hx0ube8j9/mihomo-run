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
	// 1. 获取绝对路径并切换工作目录
	exePath, err := os.Executable()
	if err != nil {
		fatalLog("Get Executable Path failed: " + err.Error())
	}
	workDir := filepath.Dir(exePath)
	os.Chdir(workDir)

	// 2. 互斥锁逻辑
	mutexName, _ := windows.UTF16PtrFromString("Local\\MihomoRunMutex_Static")
	_, err = windows.CreateMutex(nil, false, mutexName)
	if err != nil {
		if err == windows.ERROR_ALREADY_EXISTS {
			os.Exit(0) 
		}
		fatalLog("CreateMutex failed: " + err.Error())
	}

	// 3. 核心修复：使用绝对路径启动，规避 Go 的安全限制
	targetExe := filepath.Join(workDir, "mihomo.exe")
	const lockFile = "tun_on.lock"

	if _, err := os.Stat(targetExe); os.IsNotExist(err) {
		fatalLog(fmt.Sprintf("Kernel not found: %s", targetExe))
	}

	// 4. 启动进程
	cmd := exec.Command(targetExe, "-d", workDir) // 显式指定工作目录
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}

	if err := cmd.Start(); err != nil {
		fatalLog("Start mihomo failed: " + err.Error())
	}

	// 5. 检查 lock 文件并注入配置
	if _, err := os.Stat(filepath.Join(workDir, lockFile)); err == nil {
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
				time.Sleep(1000 * time.Millisecond) // 给内核一点启动时间
				req, _ := http.NewRequest("PATCH", apiURL, bytes.NewBuffer(jsonData))
				req.Header.Set("Content-Type", "application/json")
				if pr, perr := client.Do(req); perr == nil {
					pr.Body.Close()
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func fatalLog(msg string) {
	f, _ := os.OpenFile("run_error.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, _ = f.WriteString(fmt.Sprintf("[%s] %s\n", timestamp, msg))
	os.Exit(1)
}
