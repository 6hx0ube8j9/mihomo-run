package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	_, err := syscall.CreateMutex(nil, false, syscall.StringToUTF16Ptr("Global\\MihomoRunMutex-123456"))
	if err != nil {
		os.Exit(0)
	}

	exePath, _ := os.Executable()
	os.Chdir(filepath.Dir(exePath))

	const targetExe = "mihomo.exe"

	cmd := exec.Command(targetExe, "-d", ".")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000,
	}

	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}

	go func() {
		apiURL := "http://127.0.0.1:9090/configs"
		jsonData := []byte(`{"tun":{"enable":true}}`)

		for i := 0; i < 150; i++ {
			resp, err := http.Get(apiURL)
			if err == nil && resp.StatusCode == 200 {
				resp.Body.Close()
				time.Sleep(500 * time.Millisecond)

				req, _ := http.NewRequest("PATCH", apiURL, bytes.NewBuffer(jsonData))
				req.Header.Set("Content-Type", "application/json")
				client := &http.Client{Timeout: 5 * time.Second}
				if pr, perr := client.Do(req); perr == nil {
					pr.Body.Close()
					return
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()

	_ = cmd.Wait()
}
