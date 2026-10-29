package daemon

import (
	"context"
	"encoding/binary"
	"os/exec"

	"golang.org/x/sys/unix"
)

// readHostLoad reads the Mac's load from sysctl, and its CPU use from
// iostat (two one-second reports; the first is since boot).
func readHostLoad(ctx context.Context) hostLoad {
	var h hostLoad
	if n, err := unix.SysctlUint32("hw.logicalcpu"); err == nil {
		h.Cores = int(n)
	}
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		scale := float64(binary.LittleEndian.Uint64(b[16:24]))
		if scale > 0 {
			for i := range 3 {
				h.Load[i] = float64(binary.LittleEndian.Uint32(b[i*4:])) / scale
			}
		}
	}
	// The share of memory the kernel considers free, as memory pressure
	// sees it (Activity Monitor's graph is 100 minus this).
	if n, err := unix.SysctlUint32("kern.memorystatus_level"); err == nil && n <= 100 {
		h.MemPressure = float64(100-n) / 100
	}
	// struct xsw_usage { u_int64_t xsu_total, xsu_avail, xsu_used; ... }
	if b, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(b) >= 24 {
		h.SwapTotal = int64(binary.LittleEndian.Uint64(b[0:8]))
		h.SwapUsed = int64(binary.LittleEndian.Uint64(b[16:24]))
	}
	h.CPU = -1
	if out, err := exec.CommandContext(ctx, "iostat", "-n", "0", "-c", "2", "-w", "1").Output(); err == nil {
		h.CPU = parseIostatCPU(string(out))
	}
	return h
}
