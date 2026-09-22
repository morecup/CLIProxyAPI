//go:build !windows && !darwin

package controlplane

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func inspectProcess(pid int) (processIdentity, error) {
	if pid <= 1 || int64(pid) > 2147483647 {
		return processIdentity{}, fmt.Errorf("invalid process identity")
	}
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return processIdentity{Dead: true}, nil
	}
	if err != nil || runtime.GOOS != "linux" {
		return processIdentity{}, fmt.Errorf("process creation identity is unavailable")
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return processIdentity{}, fmt.Errorf("process stat is unavailable")
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return processIdentity{}, fmt.Errorf("process stat is invalid")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 {
		return processIdentity{}, fmt.Errorf("process start identity is missing")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return processIdentity{}, fmt.Errorf("process boot identity is unavailable")
	}
	return processIdentity{Start: "linux:" + strings.TrimSpace(string(boot)) + ":" + fields[19]}, nil
}
