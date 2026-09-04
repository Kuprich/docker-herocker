package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	mclient "github.com/moby/moby/client"
	ctypes "github.com/moby/moby/api/types/container"
)

type Container struct {
	ID      string
	Names   []string
	Image   string
	ImageID string
	Command string
	Created int64
	State   string
	Status  string
	Ports   []Port
}

// Stats is a single resource-usage snapshot of a container. CPUPercent is
// filled in by the caller: reliable percentages need a delta over a known
// interval, computed from two consecutive samples (CPUNano/SystemNano)
// scaled by OnlineCPUs.
type Stats struct {
	ID         string
	CPUPercent float64
	CPUNano    uint64
	SystemNano uint64
	OnlineCPUs uint32
	MemUsage   uint64
	MemLimit   uint64
	MemPercent float64
}

type Port struct {
	IP          string
	PrivatePort int
	PublicPort  int
	Type        string
}

type Image struct {
	ID         string
	RepoTags   []string
	Created    int64
	Size       int64
	Containers int64 // number of containers using this image; -1 when unknown
}

type Volume struct {
	Name       string
	Driver     string
	Mountpoint string
	CreatedAt  string
	Scope      string
}

type Network struct {
	ID     string
	Name   string
	Driver string
	Scope  string
}

type HealthInfo struct {
	Status string
}

type ContainerDetails struct {
	State struct {
		Status   string
		ExitCode int
		Health   *HealthInfo
	}
	Config struct {
		Labels map[string]string
	}
	NetworkSettings struct {
		Networks map[string]NetworkEndpoint
	}
	Mounts []MountPoint
}

type NetworkEndpoint struct {
	IPAddress string
}

type MountPoint struct {
	Type        string
	Name        string
	Source      string
	Destination string
	RW          bool
}

// Client wraps the official Docker SDK client behind the same interface the
// TUI has always used.
type Client struct {
	cli *mclient.Client
}

func NewClient() (*Client, error) {
	c, err := mclient.NewClientWithOpts(
		mclient.FromEnv,
		mclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	return &Client{cli: c}, nil
}

func (c *Client) ListContainers(all bool) ([]Container, error) {
	res, err := c.cli.ContainerList(context.Background(), mclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		ports := make([]Port, 0, len(s.Ports))
		seen := make(map[Port]bool, len(s.Ports))
		for _, p := range s.Ports {
			port := Port{
				IP:          p.IP.String(),
				PrivatePort: int(p.PrivatePort),
				PublicPort:  int(p.PublicPort),
				Type:        p.Type,
			}
			// The API reports one entry per bound IP (IPv4 + IPv6); we do
			// not display the address, so collapse those duplicates.
			key := Port{PrivatePort: port.PrivatePort, PublicPort: port.PublicPort, Type: port.Type}
			if seen[key] {
				continue
			}
			seen[key] = true
			ports = append(ports, port)
		}
		out = append(out, Container{
			ID:      s.ID,
			Names:   s.Names,
			Image:   s.Image,
			ImageID: s.ImageID,
			Command: s.Command,
			Created: s.Created,
			State:   string(s.State),
			Status:  s.Status,
			Ports:   ports,
		})
	}
	return out, nil
}

// ContainerStats fetches a single one-shot resource-usage snapshot of the
// container (the same data docker stats reports). Memory percent and the raw
// CPU/clock counters are filled in; the caller computes the CPU percent from
// the counters of two consecutive samples.
func (c *Client) ContainerStats(id string) (*Stats, error) {
	res, err := c.cli.ContainerStats(context.Background(), id, mclient.ContainerStatsOptions{
		Stream: false,
	})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var raw ctypes.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding stats for %s: %w", id, err)
	}

	s := &Stats{ID: id}
	s.CPUNano = raw.CPUStats.CPUUsage.TotalUsage
	s.SystemNano = raw.CPUStats.SystemUsage
	s.OnlineCPUs = raw.CPUStats.OnlineCPUs
	s.MemUsage = raw.MemoryStats.Usage
	s.MemLimit = raw.MemoryStats.Limit
	if s.MemLimit > 0 {
		s.MemPercent = 100 * float64(s.MemUsage) / float64(s.MemLimit)
	}
	return s, nil
}

func (c *Client) ListImages(all bool) ([]Image, error) {
	res, err := c.cli.ImageList(context.Background(), mclient.ImageListOptions{All: all})
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, Image{
			ID:         s.ID,
			RepoTags:   s.RepoTags,
			Created:    s.Created,
			Size:       s.Size,
			Containers: s.Containers,
		})
	}
	return out, nil
}

func (c *Client) ListVolumes() ([]Volume, error) {
	res, err := c.cli.VolumeList(context.Background(), mclient.VolumeListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Volume, 0, len(res.Items))
	for _, v := range res.Items {
		out = append(out, Volume{
			Name:       v.Name,
			Driver:     v.Driver,
			Mountpoint: v.Mountpoint,
			CreatedAt:  v.CreatedAt,
			Scope:      v.Scope,
		})
	}
	return out, nil
}

func (c *Client) ListNetworks() ([]Network, error) {
	res, err := c.cli.NetworkList(context.Background(), mclient.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, Network{
			ID:     n.ID,
			Name:   n.Name,
			Driver: n.Driver,
			Scope:  n.Scope,
		})
	}
	return out, nil
}

func stopTimeout(seconds int) *int {
	t := seconds
	return &t
}

func (c *Client) StartContainer(id string) error {
	_, err := c.cli.ContainerStart(context.Background(), id, mclient.ContainerStartOptions{})
	return err
}

func (c *Client) StopContainer(id string) error {
	_, err := c.cli.ContainerStop(context.Background(), id, mclient.ContainerStopOptions{Timeout: stopTimeout(10)})
	return err
}

func (c *Client) RestartContainer(id string) error {
	_, err := c.cli.ContainerRestart(context.Background(), id, mclient.ContainerRestartOptions{Timeout: stopTimeout(10)})
	return err
}

func (c *Client) PauseContainer(id string) error {
	_, err := c.cli.ContainerPause(context.Background(), id, mclient.ContainerPauseOptions{})
	return err
}

func (c *Client) UnpauseContainer(id string) error {
	_, err := c.cli.ContainerUnpause(context.Background(), id, mclient.ContainerUnpauseOptions{})
	return err
}

func (c *Client) RemoveContainer(id string) error {
	_, err := c.cli.ContainerRemove(context.Background(), id, mclient.ContainerRemoveOptions{Force: true})
	return err
}

// RemoveContainerVolumes removes the container together with its data: like
// `docker rm -v`, it also drops the anonymous volumes mounted by the
// container.
func (c *Client) RemoveContainerVolumes(id string) error {
	_, err := c.cli.ContainerRemove(context.Background(), id, mclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

func (c *Client) ContainerLogs(id, tail string, follow bool) (io.ReadCloser, error) {
	res, err := c.cli.ContainerLogs(context.Background(), id, mclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Tail:       tail,
		Follow:     follow,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ContainerLogsSince fetches log lines written strictly after since. The
// daemon treats its since filter as inclusive and truncates to microsecond
// precision, so a 1ms offset is added to the cursor; otherwise the line whose
// timestamp equals the cursor would be re-read on every tick and duplicated.
func (c *Client) ContainerLogsSince(id string, since time.Time) (io.ReadCloser, error) {
	s := since.UTC().Add(time.Millisecond)
	res, err := c.cli.ContainerLogs(context.Background(), id, mclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Since:      s.Format(time.RFC3339Nano),
		Follow:     false,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Client) InspectContainer(id string) (*ContainerDetails, error) {
	res, err := c.cli.ContainerInspect(context.Background(), id, mclient.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	ctr := res.Container

	d := &ContainerDetails{
		Mounts: make([]MountPoint, 0, len(ctr.Mounts)),
	}
	if ctr.State != nil {
		d.State.Status = string(ctr.State.Status)
		d.State.ExitCode = ctr.State.ExitCode
		if ctr.State.Health != nil {
			d.State.Health = &HealthInfo{Status: string(ctr.State.Health.Status)}
		}
	}
	if ctr.Config != nil {
		d.Config.Labels = ctr.Config.Labels
	}
	if ctr.NetworkSettings != nil && len(ctr.NetworkSettings.Networks) > 0 {
		d.NetworkSettings.Networks = make(map[string]NetworkEndpoint, len(ctr.NetworkSettings.Networks))
		for name, ep := range ctr.NetworkSettings.Networks {
			ip := ""
			if ep != nil && ep.IPAddress.IsValid() {
				ip = ep.IPAddress.String()
			}
			d.NetworkSettings.Networks[name] = NetworkEndpoint{IPAddress: ip}
		}
	}
	for _, mt := range ctr.Mounts {
		d.Mounts = append(d.Mounts, MountPoint{
			Type:        string(mt.Type),
			Name:        mt.Name,
			Source:      mt.Source,
			Destination: mt.Destination,
			RW:          mt.RW,
		})
	}
	return d, nil
}

func (c *Client) Close() error {
	return c.cli.Close()
}

// StripDockerStreamHeaders removes the 8-byte Docker stream header from log frames.
func StripDockerStreamHeaders(data []byte) string {
	var result strings.Builder
	for len(data) > 0 {
		if len(data) < 8 {
			result.Write(data)
			break
		}
		payloadLen := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		if payloadLen > len(data)-8 {
			payloadLen = len(data) - 8
		}
		if payloadLen == 0 {
			data = data[8:]
			continue
		}
		result.Write(data[8 : 8+payloadLen])
		data = data[8+payloadLen:]
	}
	return result.String()
}
