package winapi

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const taskName = "MihomoTrayTask"

func ToggleAutoStart(exePath, baseDir string, enable bool) bool {
	if key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE); err == nil {
		_ = key.DeleteValue("MihomoTray")
		key.Close()
	}
	success := false
	if enable {
		safeExePath := strings.ReplaceAll(exePath, "'", "''")
		safeBaseDir := strings.ReplaceAll(baseDir, "'", "''")

		psScript := fmt.Sprintf(
			`$trigger = New-ScheduledTaskTrigger -AtLogOn; $trigger.Delay = 'PT6S'; `+
				`$action = New-ScheduledTaskAction -Execute '%s' -Argument '---autostart' -WorkingDirectory '%s'; `+
				`$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); `+
				`Register-ScheduledTask -TaskName '%s' -Trigger $trigger -Action $action -Settings $settings -RunLevel Highest -Force`,
			safeExePath, safeBaseDir, taskName,
		)
		uni := []rune(psScript)
		b := make([]byte, len(uni)*2)
		for i, v := range uni {
			b[i*2] = byte(v)
			b[i*2+1] = byte(v >> 8)
		}
		encodedScript := base64.StdEncoding.EncodeToString(b)
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedScript)
		cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil {
			success = true
		}
	} else {
		cmd := exec.Command("schtasks", "/Delete", "/TN", "\\"+taskName, "/F")
		cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil || !CheckAutoStartStatus() {
			success = true
		}
	}
	return success
}

func CheckAutoStartStatus() bool {
	cmd := exec.Command("schtasks", "/Query", "/TN", taskName)
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}
