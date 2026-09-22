//go:build darwin

package controlplane

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

func inspectProcess(pid int) (processIdentity, error) {
	if pid <= 1 || int64(pid) > 2147483647 {
		return processIdentity{}, fmt.Errorf("invalid process identity")
	}
	if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
		return processIdentity{Dead: true}, nil
	} else if err != nil {
		return processIdentity{}, fmt.Errorf("process identity cannot be inspected")
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info == nil || info.Proc.P_starttime.Sec == 0 {
		return processIdentity{}, fmt.Errorf("process creation identity is unavailable")
	}
	return processIdentity{Start: "darwin:" + strconv.FormatInt(info.Proc.P_starttime.Sec, 10) + ":" + strconv.FormatInt(int64(info.Proc.P_starttime.Usec), 10)}, nil
}
