package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func ReadProcesses(procRoot string) ([]Process, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	inodePorts, err := listeningSocketPorts(procRoot)
	if err != nil {
		return nil, err
	}

	var processes []Process
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		procDir := filepath.Join(procRoot, entry.Name())
		exe, _ := os.Readlink(filepath.Join(procDir, "exe"))
		cwd, _ := os.Readlink(filepath.Join(procDir, "cwd"))
		exe = strings.TrimSuffix(exe, " (deleted)")
		if exe == "" && cwd == "" {
			continue
		}
		ports := processPorts(procDir, inodePorts)
		processes = append(processes, Process{PID: pid, Exe: exe, Cwd: cwd, Ports: ports})
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

func listeningSocketPorts(procRoot string) (map[string]int, error) {
	out := make(map[string]int)
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			colon := strings.LastIndex(fields[1], ":")
			if colon < 0 {
				continue
			}
			port, err := strconv.ParseUint(fields[1][colon+1:], 16, 16)
			if err != nil || port == 0 {
				continue
			}
			out[fields[9]] = int(port)
		}
	}
	return out, nil
}

func processPorts(procDir string, inodePorts map[string]int) []int {
	entries, err := os.ReadDir(filepath.Join(procDir, "fd"))
	if err != nil {
		return nil
	}
	set := make(map[int]bool)
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(procDir, "fd", entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") {
			continue
		}
		inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
		if port := inodePorts[inode]; port != 0 {
			set[port] = true
		}
	}
	ports := make([]int, 0, len(set))
	for port := range set {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}
