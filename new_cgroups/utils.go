package NewCGroups

import (
	"errors"
	com "github.com/ArisAachen/deepin-network-proxy/com"
	"os/exec"
	"strings"
)

// attach pid to cgroups path
func attach(pid string, path string) error {
	if !com.IsPid(pid) {
		return errors.New("pid is not num")
	}
	args := []string{"echo", pid, ">", path}
	// echo 12345 > /sys/fs/cgroup/unified/App.slice/cgroup.procs
	cmd := exec.Command("/bin/sh", "-c", strings.Join(args, " "))
	logger.Debugf("echo pid %s run command %v", pid, cmd)
	buf, err := cmd.CombinedOutput()
	if err != nil {
		logger.Warningf("echo pid %s to cgroups %s failed, out: %s,err: %v", pid, path, string(buf), err)
		return err
	}
	logger.Debugf("echo pid %s to cgroups %s success", pid, path)
	return nil
}