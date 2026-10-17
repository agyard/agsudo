//go:build linux

// Package proc identifies a process without mistaking a recycled PID for it.
package proc

import (
	"os"
	"strconv"
	"strings"

	"github.com/agyard/agsudo/internal/api"
)

func Identity(pid int) api.Process {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return api.Process{}
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return api.Process{}
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return api.Process{}
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return api.Process{}
	}
	return api.Process{PID: pid, Start: fields[19], Boot: strings.TrimSpace(string(boot))}
}
func Alive(p api.Process) bool { return p.PID > 0 && p.Start != "" && Identity(p.PID) == p }
