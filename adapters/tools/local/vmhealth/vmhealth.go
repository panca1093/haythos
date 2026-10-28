package vmhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/core/task"
)

const (
	maxLoadPerCore = 1.0
	maxMemUsedPct  = 90.0
	maxDiskUsedPct = 90.0
	maxGPUMemPct   = 95.0
	maxGPUTempC    = 85.0
	topN           = 5
)

type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

type Collector struct {
	Run      Runner
	ReadFile func(path string) ([]byte, error)
	NumCPU   int
}

func New() Collector {
	return Collector{
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed command names only
		},
		ReadFile: os.ReadFile,
		NumCPU:   runtime.NumCPU(),
	}
}

type Process struct {
	Command string  `json:"command"`
	CPU     float64 `json:"cpu_pct"`
	Mem     float64 `json:"mem_pct"`
}

type GPU struct {
	Name    string  `json:"name"`
	UtilPct float64 `json:"util_pct"`
	MemPct  float64 `json:"mem_pct"`
	TempC   float64 `json:"temp_c"`
}

type Report struct {
	LoadPerCore *float64  `json:"load_per_core,omitempty"`
	MemUsedPct  *float64  `json:"mem_used_pct,omitempty"`
	DiskUsedPct *float64  `json:"disk_used_pct,omitempty"`
	GPUs        []GPU     `json:"gpus,omitempty"`
	TopProcs    []Process `json:"top_processes"`
	Anomalies   []string  `json:"anomalies"`
	Gaps        []string  `json:"gaps"`
}

func (c Collector) Collect(ctx context.Context) Report {
	r := Report{Anomalies: []string{}, Gaps: []string{}}

	if load, err := c.load1(ctx); err != nil {
		r.Gaps = append(r.Gaps, "load: "+err.Error())
	} else {
		perCore := round(load / float64(max(c.NumCPU, 1)))
		r.LoadPerCore = &perCore
		if perCore > maxLoadPerCore {
			r.Anomalies = append(r.Anomalies, fmt.Sprintf("load per core %.2f > %.1f", perCore, maxLoadPerCore))
		}
	}
	if pct, err := c.memUsedPct(ctx); err != nil {
		r.Gaps = append(r.Gaps, "memory: "+err.Error())
	} else {
		mem := round(pct)
		r.MemUsedPct = &mem
		if pct > maxMemUsedPct {
			r.Anomalies = append(r.Anomalies, fmt.Sprintf("memory used %.0f%% > %.0f%%", pct, maxMemUsedPct))
		}
	}
	if pct, err := c.diskUsedPct(ctx); err != nil {
		r.Gaps = append(r.Gaps, "disk: "+err.Error())
	} else {
		r.DiskUsedPct = &pct
		if pct > maxDiskUsedPct {
			r.Anomalies = append(r.Anomalies, fmt.Sprintf("disk / used %.0f%% > %.0f%%", pct, maxDiskUsedPct))
		}
	}
	if procs, err := c.topProcesses(ctx); err != nil {
		r.Gaps = append(r.Gaps, "processes: "+err.Error())
	} else {
		r.TopProcs = procs
	}
	r.GPUs = c.gpus(ctx)
	for _, g := range r.GPUs {
		if g.MemPct > maxGPUMemPct {
			r.Anomalies = append(r.Anomalies, fmt.Sprintf("gpu %s memory %.0f%% > %.0f%%", g.Name, g.MemPct, maxGPUMemPct))
		}
		if g.TempC > maxGPUTempC {
			r.Anomalies = append(r.Anomalies, fmt.Sprintf("gpu %s temperature %.0fC > %.0fC", g.Name, g.TempC, maxGPUTempC))
		}
	}
	return r
}

func round(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

func (c Collector) load1(ctx context.Context) (float64, error) {
	if b, err := c.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			return strconv.ParseFloat(f[0], 64)
		}
	}
	out, err := c.Run(ctx, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return 0, fmt.Errorf("unavailable")
	}
	f := strings.Fields(strings.Trim(string(out), "{} \n"))
	if len(f) == 0 {
		return 0, fmt.Errorf("unparseable")
	}
	return strconv.ParseFloat(f[0], 64)
}

func (c Collector) memUsedPct(ctx context.Context) (float64, error) {
	b, err := c.ReadFile("/proc/meminfo")
	if err != nil {
		return c.macMemUsedPct(ctx)
	}
	var total, avail float64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("unparseable")
	}
	return (total - avail) / total * 100, nil
}

// TODO: uncertain — free+inactive+speculative reads ~20 points higher than memory_pressure (ignores purgeable/compressed), verify before prod
func (c Collector) macMemUsedPct(ctx context.Context) (float64, error) {
	memsize, err := c.Run(ctx, "sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("unavailable (no /proc/meminfo or vm_stat)")
	}
	stat, err := c.Run(ctx, "vm_stat")
	if err != nil {
		return 0, fmt.Errorf("unavailable (no /proc/meminfo or vm_stat)")
	}
	total, err := strconv.ParseFloat(strings.TrimSpace(string(memsize)), 64)
	if err != nil || total == 0 {
		return 0, fmt.Errorf("unparseable")
	}
	var pageSize, availPages float64
	for _, line := range strings.Split(string(stat), "\n") {
		if _, rest, ok := strings.Cut(line, "page size of "); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				pageSize, _ = strconv.ParseFloat(f[0], 64)
			}
			continue
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch name {
		case "Pages free", "Pages inactive", "Pages speculative":
			v, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(val), "."), 64)
			availPages += v
		}
	}
	if pageSize == 0 {
		return 0, fmt.Errorf("unparseable")
	}
	return (total - availPages*pageSize) / total * 100, nil
}

func (c Collector) diskUsedPct(ctx context.Context) (float64, error) {
	out, err := c.Run(ctx, "df", "-kP", "/")
	if err != nil {
		return 0, fmt.Errorf("df failed")
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unparseable")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 5 {
		return 0, fmt.Errorf("unparseable")
	}
	return strconv.ParseFloat(strings.TrimSuffix(f[4], "%"), 64)
}

func (c Collector) topProcesses(ctx context.Context) ([]Process, error) {
	out, err := c.Run(ctx, "ps", "-eo", "pcpu,pmem,comm")
	if err != nil {
		return nil, fmt.Errorf("ps failed")
	}
	var procs []Process
	for _, line := range strings.Split(string(out), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		cpu, err1 := strconv.ParseFloat(f[0], 64)
		mem, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		procs = append(procs, Process{Command: strings.Join(f[2:], " "), CPU: cpu, Mem: mem})
	}
	sort.SliceStable(procs, func(i, j int) bool { return procs[i].CPU > procs[j].CPU })
	if len(procs) > topN {
		procs = procs[:topN]
	}
	return procs, nil
}

// gpus returns nothing when nvidia-smi is absent: no GPU is not a gap.
func (c Collector) gpus(ctx context.Context) []GPU {
	out, err := c.Run(ctx, "nvidia-smi", "--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	var gpus []GPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 5 {
			continue
		}
		num := func(s string) float64 { v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return v }
		g := GPU{Name: strings.TrimSpace(f[0]), UtilPct: num(f[1]), TempC: num(f[4])}
		if total := num(f[3]); total > 0 {
			g.MemPct = round(num(f[2]) / total * 100)
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// Register adds the vmhealth.read tool; its result is flagged Normal only when nothing is anomalous and nothing is unmeasurable.
func Register(r *local.Registry, c Collector) {
	r.Register(task.ToolSpec{
		Name:        "vmhealth.read",
		Description: "Read CPU load, memory, disk, GPU and top processes of this machine, with anomalies already flagged.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, _ []byte) (task.ToolResult, error) {
		rep := c.Collect(ctx)
		b, err := json.Marshal(rep)
		if err != nil {
			return task.ToolResult{}, fmt.Errorf("encode vmhealth report: %w", err)
		}
		return task.ToolResult{Content: string(b), Normal: len(rep.Anomalies) == 0 && len(rep.Gaps) == 0}, nil
	})
}
