package ui

import (
	"net"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

var tunKeywords = []string{"mihomo", "meta", "clash", "sing-box", "wintun"}

func (tm *TrayManager) WatchTunState() {
	fd, err := windows.Socket(windows.AF_INET, windows.SOCK_DGRAM, windows.IPPROTO_UDP)
	if err != nil {
		tm.fallbackWatchTunState()
		return
	}

	go func() {
		for {
			if tm.cm.IsReallyExiting() {
				_ = windows.Close(fd)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	checkTun := func() {
		if !tm.cm.IsKernelActive() || !tm.cm.GetTunState() {
			tm.cm.SetTunAlive(false)
			return
		}
		currentHasTun := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				if tm.IsTunInterfaceMatch(i.Name) {
					currentHasTun = true
					break
				}
			}
		}
		tm.cm.SetTunAlive(currentHasTun)
	}

	checkTun()

	for {
		var bytesReturned uint32
		const SIO_ADDRESS_LIST_CHANGE = 0x28000017

		err := windows.WSAIoctl(
			fd, SIO_ADDRESS_LIST_CHANGE,
			nil, 0, nil, 0, &bytesReturned, nil, 0,
		)

		if err != nil {
			if tm.cm.IsReallyExiting() {
				return
			}
			tm.fallbackWatchTunState()
			return
		}

		time.Sleep(200 * time.Millisecond)
		checkTun()
	}
}

func (tm *TrayManager) fallbackWatchTunState() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if tm.cm.IsReallyExiting() {
				return
			}
			if !tm.cm.IsKernelActive() || !tm.cm.GetTunState() {
				tm.cm.SetTunAlive(false)
				continue
			}
			currentHasTun := false
			ifaces, err := net.Interfaces()
			if err == nil {
				for _, i := range ifaces {
					if tm.IsTunInterfaceMatch(i.Name) {
						currentHasTun = true
						break
					}
				}
			}
			tm.cm.SetTunAlive(currentHasTun)
		}
	}
}

func (tm *TrayManager) IsTunInterfaceMatch(ifaceName string) bool {
	name := strings.ToLower(ifaceName)
	target := strings.ToLower(tm.cm.GetJsonConfig("tun_device"))
	if target != "" && strings.Contains(name, target) {
		return true
	}
	for _, kw := range tunKeywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}
