//go:build linux

package production

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// systemMemoryUsage returns the percentage of system memory in use by reading
// /proc/meminfo. It computes used memory as MemTotal - MemAvailable, which
// accounts for buffers and cache (matching what tools like `free` report).
func systemMemoryUsage() (percent float64, totalBytes int64, usedBytes int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("opening /proc/meminfo: %w", err)
	}
	defer f.Close()

	var memTotalKB, memAvailableKB int64
	foundTotal, foundAvailable := false, false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			memTotalKB, err = parseProcMemLine(line)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("parsing MemTotal: %w", err)
			}
			foundTotal = true
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailableKB, err = parseProcMemLine(line)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("parsing MemAvailable: %w", err)
			}
			foundAvailable = true
		}
		if foundTotal && foundAvailable {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("reading /proc/meminfo: %w", err)
	}

	if !foundTotal || !foundAvailable {
		return 0, 0, 0, fmt.Errorf("missing MemTotal or MemAvailable in /proc/meminfo")
	}

	totalBytes = memTotalKB * 1024
	usedBytes = (memTotalKB - memAvailableKB) * 1024
	if totalBytes > 0 {
		percent = float64(usedBytes) / float64(totalBytes) * 100.0
	}
	return percent, totalBytes, usedBytes, nil
}

// parseProcMemLine parses a line like "MemTotal:       16384000 kB" and returns
// the value in kilobytes.
func parseProcMemLine(line string) (int64, error) {
	// Format: "FieldName:     12345 kB"
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, fmt.Errorf("unexpected format: %q", line)
	}
	return strconv.ParseInt(parts[1], 10, 64)
}

// systemCPUUsage returns the CPU usage percentage by reading /proc/stat twice
// with a short delay and computing the delta. The result reflects system-wide
// CPU usage across all cores.
func systemCPUUsage() (percent float64, err error) {
	idle1, total1, err := readProcStat()
	if err != nil {
		return 0, err
	}

	// Sleep briefly to measure a delta. 200ms gives a reasonable sample
	// without blocking the monitor loop for too long.
	time.Sleep(200 * time.Millisecond)

	idle2, total2, err := readProcStat()
	if err != nil {
		return 0, err
	}

	totalDelta := total2 - total1
	idleDelta := idle2 - idle1

	if totalDelta <= 0 {
		return 0, nil
	}

	percent = float64(totalDelta-idleDelta) / float64(totalDelta) * 100.0
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return percent, nil
}

// readProcStat reads the aggregate CPU line from /proc/stat and returns the
// idle and total jiffies. The first line has the format:
//
//	cpu  user nice system idle iowait irq softirq steal [guest] [guest_nice]
func readProcStat() (idle, total int64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, fmt.Errorf("opening /proc/stat: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, fmt.Errorf("unexpected /proc/stat cpu line: %q", line)
		}

		// fields[0] = "cpu", fields[1..] = user, nice, system, idle, iowait, irq, softirq, steal, ...
		var values []int64
		for _, field := range fields[1:] {
			v, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parsing /proc/stat field %q: %w", field, err)
			}
			values = append(values, v)
		}

		// idle is at index 3 (the 4th numeric field).
		// iowait (index 4) is also considered idle time.
		idle = values[3]
		if len(values) > 4 {
			idle += values[4] // iowait
		}

		for _, v := range values {
			total += v
		}

		return idle, total, nil
	}

	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("reading /proc/stat: %w", err)
	}

	return 0, 0, fmt.Errorf("no cpu line found in /proc/stat")
}

// systemCPUCount returns the number of logical CPUs available to the process.
func systemCPUCount() int {
	return runtime.NumCPU()
}
