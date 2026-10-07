package docker

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"time"
)

// Stats is the panel-friendly view of a one-shot docker stats sample.
type Stats struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemUsedMB  float64 `json:"mem_used_mb"`
	MemLimitMB float64 `json:"mem_limit_mb"`
	MemPercent float64 `json:"mem_percent"`
	NetRxMB    float64 `json:"net_rx_mb"`
	NetTxMB    float64 `json:"net_tx_mb"`
	Pids       uint64  `json:"pids"`
}

type statsRaw struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

func (c *Client) fetchStats(ctx context.Context, id string) (*statsRaw, error) {
	var s statsRaw
	err := c.do(ctx, http.MethodGet,
		"/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Stats returns a one-shot sample. Engine behavior for precpu differs across
// versions (one-shot may return a single sample where precpu == cpu); when the
// built-in delta is unusable we take a second sample ~500ms later and compute
// the delta ourselves — same formula the docker CLI uses.
func (c *Client) Stats(ctx context.Context, id string) (*Stats, error) {
	s1, err := c.fetchStats(ctx, id)
	if err != nil {
		return nil, err
	}
	st := computeStats(s1, nil)
	if st.CPUPercent <= 0 {
		select {
		case <-ctx.Done():
			return st, nil
		case <-time.After(500 * time.Millisecond):
		}
		s2, err := c.fetchStats(ctx, id)
		if err != nil {
			// first sample is still valid; CPU stays 0
			return st, nil
		}
		st = computeStats(s2, s1)
	}
	return st, nil
}

func computeStats(cur, prev *statsRaw) *Stats {
	out := &Stats{}

	cpuDelta := float64(cur.CPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(cur.CPUStats.SystemUsage)
	if prev != nil && cur.CPUStats.CPUUsage.TotalUsage >= prev.CPUStats.CPUUsage.TotalUsage &&
		cur.CPUStats.SystemUsage >= prev.CPUStats.SystemUsage {
		cpuDelta = float64(cur.CPUStats.CPUUsage.TotalUsage - prev.CPUStats.CPUUsage.TotalUsage)
		sysDelta = float64(cur.CPUStats.SystemUsage - prev.CPUStats.SystemUsage)
	} else if cur.PreCPUStats.SystemUsage > 0 && cur.PreCPUStats.CPUUsage.TotalUsage > 0 &&
		cur.CPUStats.SystemUsage >= cur.PreCPUStats.SystemUsage &&
		cur.CPUStats.CPUUsage.TotalUsage >= cur.PreCPUStats.CPUUsage.TotalUsage {
		cpuDelta = float64(cur.CPUStats.CPUUsage.TotalUsage - cur.PreCPUStats.CPUUsage.TotalUsage)
		sysDelta = float64(cur.CPUStats.SystemUsage - cur.PreCPUStats.SystemUsage)
	}
	online := cur.CPUStats.OnlineCPUs
	if online == 0 {
		online = uint32(len(cur.CPUStats.CPUUsage.PercpuUsage))
	}
	if online == 0 {
		online = 1
	}
	if sysDelta > 0 && cpuDelta > 0 {
		out.CPUPercent = round1(cpuDelta / sysDelta * float64(online) * 100)
	}

	used := cur.MemoryStats.Usage
	// cgroup v2: subtract page cache (inactive_file) like the docker CLI does
	if f, ok := cur.MemoryStats.Stats["total_inactive_file"]; ok && f < used {
		used -= f
	}
	out.MemUsedMB = round1(float64(used) / (1024 * 1024))
	out.MemLimitMB = round1(float64(cur.MemoryStats.Limit) / (1024 * 1024))
	if cur.MemoryStats.Limit > 0 {
		out.MemPercent = round1(float64(used) / float64(cur.MemoryStats.Limit) * 100)
	}

	var rx, tx uint64
	for _, n := range cur.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}
	out.NetRxMB = round1(float64(rx) / (1024 * 1024))
	out.NetTxMB = round1(float64(tx) / (1024 * 1024))
	out.Pids = cur.PidsStats.Current
	return out
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
