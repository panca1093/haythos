package vmhealth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const meminfo = "MemTotal:       1000000 kB\nMemFree: 1 kB\nMemAvailable:    %d kB\n"

func collector(avail string, load string, disk string, gpu string) Collector {
	return Collector{
		NumCPU: 4,
		ReadFile: func(p string) ([]byte, error) {
			switch p {
			case "/proc/loadavg":
				return []byte(load + " 1.0 1.0 1/100 123\n"), nil
			case "/proc/meminfo":
				return []byte(strings.Replace(meminfo, "%d", avail, 1)), nil
			}
			return nil, errors.New("nope")
		},
		Run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch name {
			case "df":
				return []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 1000 500 500 " + disk + "% /\n"), nil
			case "ps":
				return []byte("%CPU %MEM COMMAND\n 1.0 2.0 sshd\n 90.5 10.0 python worker\n 5.0 1.0 nginx\n"), nil
			case "nvidia-smi":
				if gpu == "" {
					return nil, errors.New("not found")
				}
				return []byte(gpu), nil
			}
			return nil, errors.New("unexpected " + name)
		},
	}
}

func TestCollectNormal(t *testing.T) {
	r := collector("500000", "1.2", "40", "").Collect(context.Background())
	if len(r.Anomalies) != 0 || len(r.Gaps) != 0 {
		t.Fatalf("anomalies=%v gaps=%v", r.Anomalies, r.Gaps)
	}
	if r.LoadPerCore == nil || r.MemUsedPct == nil || r.DiskUsedPct == nil || *r.LoadPerCore != 0.3 || *r.MemUsedPct != 50 || *r.DiskUsedPct != 40 {
		t.Errorf("report = %+v", r)
	}
	if len(r.TopProcs) != 3 || r.TopProcs[0].Command != "python worker" {
		t.Errorf("procs = %+v", r.TopProcs)
	}
	if r.GPUs != nil {
		t.Errorf("gpus = %+v, want none", r.GPUs)
	}
}

func TestCollectFlagsAnomalies(t *testing.T) {
	r := collector("50000", "9.0", "95", "RTX, 10, 24000, 24000, 90\n").Collect(context.Background())
	if len(r.Anomalies) != 5 {
		t.Errorf("anomalies = %v, want load, memory, disk, gpu memory, gpu temp", r.Anomalies)
	}
}

func TestMissingSourcesBecomeGapsNotAnomalies(t *testing.T) {
	c := Collector{NumCPU: 2,
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("nope") },
		Run:      func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("nope") },
	}
	r := c.Collect(context.Background())
	if len(r.Anomalies) != 0 || len(r.Gaps) != 4 {
		t.Errorf("anomalies=%v gaps=%v", r.Anomalies, r.Gaps)
	}
}

func TestUnmeasuredMetricsAreOmittedFromJSON(t *testing.T) {
	tests := []struct {
		name    string
		c       Collector
		absent  []string
		present []string
	}{
		{
			name: "no proc and no commands",
			c: Collector{NumCPU: 2,
				ReadFile: func(string) ([]byte, error) { return nil, errors.New("nope") },
				Run:      func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("nope") },
			},
			absent:  []string{"load_per_core", "mem_used_pct", "disk_used_pct"},
			present: []string{"gaps"},
		},
		{
			name: "only memory unavailable",
			c: func() Collector {
				c := collector("500000", "1.2", "40", "")
				rf := c.ReadFile
				c.ReadFile = func(p string) ([]byte, error) {
					if p == "/proc/meminfo" {
						return nil, errors.New("nope")
					}
					return rf(p)
				}
				return c
			}(),
			absent:  []string{"mem_used_pct"},
			present: []string{"load_per_core", "disk_used_pct", "gaps"},
		},
		{
			name:    "measured zero stays present",
			c:       collector("1000000", "0.0", "0", ""),
			present: []string{"load_per_core", "mem_used_pct", "disk_used_pct"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.c.Collect(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			for _, k := range tt.absent {
				if _, ok := m[k]; ok {
					t.Errorf("%s present in %s", k, b)
				}
			}
			for _, k := range tt.present {
				if _, ok := m[k]; !ok {
					t.Errorf("%s missing in %s", k, b)
				}
			}
		})
	}
}
