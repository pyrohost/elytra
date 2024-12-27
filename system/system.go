package system

import (
	"context"
	"runtime"

	"github.com/acobaugh/osrelease"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

type Information struct {
	Version string            `json:"version"`
	Docker  DockerInformation `json:"docker"`
	System  System            `json:"system"`
}

type DockerInformation struct {
	Version    string           `json:"version"`
	Cgroups    DockerCgroups    `json:"cgroups"`
	Containers DockerContainers `json:"containers"`
	Storage    DockerStorage    `json:"storage"`
	Runc       DockerRunc       `json:"runc"`
}

type DockerCgroups struct {
	Driver  string `json:"driver"`
	Version string `json:"version"`
}

type DockerContainers struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Paused  int `json:"paused"`
	Stopped int `json:"stopped"`
}

type DockerStorage struct {
	Driver     string `json:"driver"`
	Filesystem string `json:"filesystem"`
}

type DockerRunc struct {
	Version string `json:"version"`
}

type System struct {
	Architecture  string `json:"architecture"`
	CPUThreads    int    `json:"cpu_threads"`
	MemoryBytes   int64  `json:"memory_bytes"`
	KernelVersion string `json:"kernel_version"`
	OS            string `json:"os"`
	OSType        string `json:"os_type"`
}

type Utilization struct {
	CpuPercent  float64 `json:"cpu"`
	MemoryTotal uint64  `json:"memory_total"`
	MemoryUsed  uint64  `json:"memory_used"`
	SwapTotal   uint64  `json:"swap_total"`
	SwapUsed    uint64  `json:"swap_used"`
	DiskTotal   uint64  `json:"disk_total"`
	DiskUsed    uint64  `json:"disk_used"`
}

func GetSystemInformation() (*Information, error) {
	k, err := kernel.GetKernelVersion()
	if err != nil {
		return nil, err
	}

	version, info, err := GetDockerInfo(context.Background())
	if err != nil {
		return nil, err
	}

	release, err := osrelease.Read()
	if err != nil {
		return nil, err
	}

	var os string
	if release["PRETTY_NAME"] != "" {
		os = release["PRETTY_NAME"]
	} else if release["NAME"] != "" {
		os = release["NAME"]
	} else {
		os = info.OperatingSystem
	}

	var filesystem string
	for _, v := range info.DriverStatus {
		if v[0] != "Backing Filesystem" {
			continue
		}
		filesystem = v[1]
		break
	}

	return &Information{
		Version: Version,
		Docker: DockerInformation{
			Version: version.Version,
			Cgroups: DockerCgroups{
				Driver:  info.CgroupDriver,
				Version: info.CgroupVersion,
			},
			Containers: DockerContainers{
				Total:   info.Containers,
				Running: info.ContainersRunning,
				Paused:  info.ContainersPaused,
				Stopped: info.ContainersStopped,
			},
			Storage: DockerStorage{
				Driver:     info.Driver,
				Filesystem: filesystem,
			},
			Runc: DockerRunc{
				Version: info.RuncCommit.ID,
			},
		},
		System: System{
			Architecture:  runtime.GOARCH,
			CPUThreads:    runtime.NumCPU(),
			MemoryBytes:   info.MemTotal,
			KernelVersion: k.String(),
			OS:            os,
			OSType:        runtime.GOOS,
		},
	}, nil
}

func GetSystemUtilization() (*Utilization, error) {
	c, err := cpu.Percent(0, false)
	if err != nil {
		return nil, err
	}
	m, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}
	s, err := mem.SwapMemory()
	if err != nil {
		return nil, err
	}
	d, err := disk.Usage("/")
	if err != nil {
		return nil, err
	}

	return &Utilization{
		MemoryTotal: m.Total,
		MemoryUsed:  m.Used,
		SwapTotal:   s.Total,
		SwapUsed:    s.Used,
		CpuPercent:  c[0],
		DiskTotal:   d.Total,
		DiskUsed:    d.Used,
	}, nil
}

func GetDockerInfo(ctx context.Context) (types.Version, system.Info, error) {
	// TODO: find a way to re-use the client from the docker environment.
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return types.Version{}, system.Info{}, err
	}
	defer c.Close()

	dockerVersion, err := c.ServerVersion(ctx)
	if err != nil {
		return types.Version{}, system.Info{}, err
	}

	dockerInfo, err := c.Info(ctx)
	if err != nil {
		return types.Version{}, system.Info{}, err
	}

	return dockerVersion, dockerInfo, nil
}
