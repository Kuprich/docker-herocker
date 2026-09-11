package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	ctypes "github.com/moby/moby/api/types/container"
	mclient "github.com/moby/moby/client"
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
	Labels  map[string]string
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
	RefCount   int64 // containers referencing this volume (daemon-computed); -1 when unknown
	Size       int64 // bytes used by the volume; -1 when the driver does not report it
}

type Network struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Containers int // containers attached to this network (including stopped); 0 means unused
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

// containerListOptions builds the request options for a container listing.
// statuses lists the docker container states ("running", "exited", …) the
// caller wants to keep; an empty list means no status restriction and all is
// honoured as before. The docker "status" filter is OR-combined by the API,
// so several states still form a plain union.
func containerListOptions(all bool, statuses []string) mclient.ContainerListOptions {
	opts := mclient.ContainerListOptions{All: all}
	if len(statuses) > 0 {
		opts.Filters = make(mclient.Filters).Add("status", statuses...)
	}
	return opts
}

// ListContainers lists the containers on the host. statuses narrows the
// result to the given docker lifecycle states; when it is empty the listing
// falls back to all (all=false shows only running/paused containers).
func (c *Client) ListContainers(all bool, statuses []string) ([]Container, error) {
	res, err := c.cli.ContainerList(context.Background(), containerListOptions(all, statuses))
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
			Labels:  s.Labels,
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

// ListVolumes returns every volume with its daemon-computed usage: how many
// containers reference it (including stopped ones, so a volume tied to a
// stopped container still counts as IN USE) and its on-disk size. The plain
// /volumes endpoint never includes usage details — only the disk-usage report
// does — and the detailed per-volume Items are only filled with Verbose, so
// this issues a verbose /system/df query restricted to volumes.
func (c *Client) ListVolumes() ([]Volume, error) {
	res, err := c.cli.DiskUsage(context.Background(), mclient.DiskUsageOptions{Volumes: true, Verbose: true})
	if err != nil {
		return nil, err
	}
	items := res.Volumes.Items
	out := make([]Volume, 0, len(items))
	for _, v := range items {
		refCount, size := int64(0), int64(0)
		if u := v.UsageData; u != nil {
			refCount, size = u.RefCount, u.Size
		}
		out = append(out, Volume{
			Name:       v.Name,
			Driver:     v.Driver,
			Mountpoint: v.Mountpoint,
			CreatedAt:  v.CreatedAt,
			Scope:      v.Scope,
			RefCount:   refCount,
			Size:       size,
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
	c.annotateNetworkUsage(out)
	return out, nil
}

// annotateNetworkUsage fills each network's Containers count, crossing the
// network list with the container list. The /networks endpoint never reports
// attachments, but every container's summary carries the IDs of the networks
// it is connected to, so usage (matching `docker network rm`'s notion of
// "active endpoints", stopped containers included) is one extra list query
// rather than an inspect per network.
func (c *Client) annotateNetworkUsage(networks []Network) {
	containerRes, err := c.cli.ContainerList(context.Background(), mclient.ContainerListOptions{All: true})
	if err != nil {
		return
	}
	used := make(map[string]int, len(containerRes.Items))
	for i := range containerRes.Items {
		s := &containerRes.Items[i]
		if s.NetworkSettings == nil {
			continue
		}
		for _, ep := range s.NetworkSettings.Networks {
			if ep != nil {
				used[ep.NetworkID]++
			}
		}
	}
	for i := range networks {
		networks[i].Containers = used[networks[i].ID]
	}
}

func stopTimeout(seconds int) *int {
	t := seconds
	return &t
}

func (c *Client) RemoveImage(id string, force bool) error {
	_, err := c.cli.ImageRemove(context.Background(), id, mclient.ImageRemoveOptions{Force: force, PruneChildren: true})
	return err
}

// RemoveVolume deletes a volume; force ignores an in-use volume (instantly
// disconnects it from the container referencing it), mirroring
// `docker volume rm [-f]`.
func (c *Client) RemoveVolume(name string, force bool) error {
	_, err := c.cli.VolumeRemove(context.Background(), name, mclient.VolumeRemoveOptions{Force: force})
	return err
}

// PruneVolumes removes every unused volume, named and anonymous alike — the
// daemon's notion of "unused" matches what this app marks UNUSED (no
// container references it), so All is required for docker-compatible `volume
// prune -a` semantics.
func (c *Client) PruneVolumes() error {
	_, err := c.cli.VolumePrune(context.Background(), mclient.VolumePruneOptions{All: true})
	return err
}

// RemoveNetwork deletes a network. There is no force variant for networks:
// the daemon refuses to remove one with active endpoints (attached
// containers, stopped ones included), and that error is surfaced as-is.
func (c *Client) RemoveNetwork(name string) error {
	_, err := c.cli.NetworkRemove(context.Background(), name, mclient.NetworkRemoveOptions{})
	return err
}

// PruneNetworks removes every unused network, mirroring `docker network
// prune`: the daemon only deletes networks no container is attached to.
func (c *Client) PruneNetworks() error {
	_, err := c.cli.NetworkPrune(context.Background(), mclient.NetworkPruneOptions{})
	return err
}

// PruneImages deletes all dangling images (no longer referenced by any tag).
func (c *Client) PruneImages() error {
	filters := mclient.Filters{}.Add("dangling", "true")
	_, err := c.cli.ImagePrune(context.Background(), mclient.ImagePruneOptions{Filters: filters})
	return err
}

// PruneContainers removes every stopped (exited/created) container, the
// daemon's notion of "unused": a stopped container holds its writable layer's
// data, so this is destructive and irreversible. Operating containers are
// never pruned by the daemon.
func (c *Client) PruneContainers() error {
	_, err := c.cli.ContainerPrune(context.Background(), mclient.ContainerPruneOptions{})
	return err
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

// ComposeService is one service of a compose project and whether at least one
// of its containers is currently running (replicas collapse into a single
// service row).
type ComposeService struct {
	Name    string
	Running bool
}

// ComposeProject is a docker compose project extracted from container labels.
type ComposeProject struct {
	Name        string
	Services    []ComposeService
	Running     int
	Total       int
	ConfigFiles string
}

// ListComposeProjects groups all containers by their com.docker.compose.project
// label and returns one ComposeProject per project name; statuses is forwarded
// to ListContainers so the tree only reflects the filtered container states.
// Containers without the label are ignored; empty results yield an empty slice
// (never nil).
func (c *Client) ListComposeProjects(all bool, statuses []string) ([]ComposeProject, error) {
	containers, err := c.ListContainers(all, statuses)
	if err != nil {
		return nil, err
	}
	return groupComposeProjects(containers), nil
}

// groupComposeProjects turns a flat container list into compose projects keyed
// on the com.docker.compose.project label. It is a pure function so the
// grouping/sorting logic is unit-tested without a daemon.
func groupComposeProjects(containers []Container) []ComposeProject {
	type project struct {
		services    map[string]bool
		running     int
		total       int
		configFiles string
	}
	projects := make(map[string]*project)
	for _, ct := range containers {
		name := ct.Labels["com.docker.compose.project"]
		if name == "" {
			continue
		}
		p := projects[name]
		if p == nil {
			p = &project{services: make(map[string]bool)}
			projects[name] = p
		}
		if svc := ct.Labels["com.docker.compose.service"]; svc != "" {
			// Always register the service so fully-down (exited) services
			// still appear in the list; Running collapses replicas (up if
			// any container of the service is running).
			if ct.State == "running" {
				p.services[svc] = true
			} else if _, seen := p.services[svc]; !seen {
				p.services[svc] = false
			}
		}
		p.total++
		if ct.State == "running" {
			p.running++
		}
		if p.configFiles == "" {
			p.configFiles = ct.Labels["com.docker.compose.project.config_files"]
		}
	}
	out := make([]ComposeProject, 0, len(projects))
	for name, p := range projects {
		svcs := make([]ComposeService, 0, len(p.services))
		for s, running := range p.services {
			svcs = append(svcs, ComposeService{Name: s, Running: running})
		}
		sort.Slice(svcs, func(i, j int) bool {
			return svcs[i].Name < svcs[j].Name
		})
		out = append(out, ComposeProject{
			Name:        name,
			Services:    svcs,
			Running:     p.running,
			Total:       p.total,
			ConfigFiles: p.configFiles,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
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
