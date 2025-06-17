package system

import (
	"context"
	"math"
	"runtime"

	"github.com/acobaugh/osrelease"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
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
	Architecture       string   `json:"architecture"`
	CPUThreads         int      `json:"cpu_threads"`
	CPUUsage           *float64 `json:"cpu_usage"`
	MemoryBytes        int64    `json:"memory_bytes"`
	MemoryBytesUsage   *uint64  `json:"memory_bytes_usage"`
	MemoryPercentUsage *float64 `json:"memory_percent_usage"`
	DiskBytes          *uint64  `json:"disk_bytes"`
	DiskBytesUsage     *uint64  `json:"disk_bytes_usage"`
	DiskPercentUsage   *float64 `json:"disk_percent_usage"`
	KernelVersion      string   `json:"kernel_version"`
	OS                 string   `json:"os"`
	OSType             string   `json:"os_type"`
}

func GetSystemInformation(rootDirectoryPath string) (*Information, error) {
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

	cpuUsage, err := cpu.Percent(0, false)
	var cpuUsagePercent *float64
	if err == nil && len(cpuUsage) > 0 {
		v := math.Round(cpuUsage[0]*100) / 100
		cpuUsagePercent = &v
	}

	memStats, err := mem.VirtualMemory()
	var memPercentUsage *float64
	var memBytesUsage *uint64
	if err == nil {
		v := math.Round(memStats.UsedPercent*100) / 100
		memPercentUsage, memBytesUsage = &v, &memStats.Used
	}

	diskStats, err := disk.Usage(rootDirectoryPath)
	var diskBytes, diskBytesUsage *uint64
	var diskPercentUsage *float64
	if err == nil {
		diskBytes, diskBytesUsage = &diskStats.Total, &diskStats.Used
		v := math.Round(diskStats.UsedPercent*100) / 100
		diskPercentUsage = &v
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
			Architecture:       runtime.GOARCH,
			CPUThreads:         runtime.NumCPU(),
			CPUUsage:           cpuUsagePercent,
			MemoryBytes:        info.MemTotal,
			MemoryBytesUsage:   memBytesUsage,
			MemoryPercentUsage: memPercentUsage,
			DiskBytes:          diskBytes,
			DiskBytesUsage:     diskBytesUsage,
			DiskPercentUsage:   diskPercentUsage,
			KernelVersion:      k.String(),
			OS:                 os,
			OSType:             runtime.GOOS,
		},
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
