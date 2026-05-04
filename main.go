package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func main() {
	// 1. 路径修复：获取绝对路径，避免在服务模式下迷失
	exePath, err := os.Executable()
	if err != nil {
		fatalLog("Get Executable Path failed: " + err.Error())
	}
	workDir := filepath.Dir(exePath)
	os.Chdir(workDir)

	// 2. 互斥锁修复：去掉 Global\ 前缀以降低权限要求，正确判断 ERROR_ALREADY_EXISTS
	// 使用本地会话前缀 Local\ 或直接不用前缀
	mutexName, _ := windows.UTF16PtrFromString("Local\\MihomoRunMutex_Static")
	_, err = windows.CreateMutex(nil, false, mutexName)
	if err != nil {
		if err == windows.ERROR_ALREADY_EXISTS {
			os.Exit(0) // 正常退出：实例已存在
		}
		fatalLog("CreateMutex failed: " + err.Error())
	}

	const targetExe = "mihomo.exe"
	const lockFile = "tun_on.lock"

	// 3. 启动检查：确保内核存在
	if _, err := os.Stat(targetExe); os.IsNotExist(err) {
		fatalLog(fmt.Sprintf("Kernel not found: %s in %s", targetExe, workDir))
	}

	// 4. 进程启动：调试阶段建议注释掉 CreationFlags 以便观察
	cmd := exec.Command(targetExe, "-d", ".")
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW, // 0x08000000
	}

	if err := cmd.Start(); err != nil {
		fatalLog("Start mihomo failed: " + err.Error())
	}

	// 5. 逻辑同步：检查 lock 文件
	if _, err := os.Stat(lockFile); err == nil {
		go startConfigInjector("http://127.0.0.1:9090/configs")
	}

	_ = cmd.Wait()
}

func startConfigInjector(apiURL string) {
	jsonData := []byte(`{"tun":{"enable":true}}`)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 150; i++ {
		// 探测端口是否开放
		resp, err := client.Get(apiURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				time.Sleep(500 * time.Millisecond)
				// 发送 PATCH
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
