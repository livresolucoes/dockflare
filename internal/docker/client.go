package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/livresolucoes/dockflare/internal/logger"
)

// cgroupFile is a package-level variable to allow test overrides.
var cgroupFile = "/proc/self/cgroup"

type ContainerInfo struct {
	ID       string
	Name     string
	Networks []string
	// Ports lists the TCP ports the container declares as exposed (EXPOSE in
	// the image, or `expose:`/`ports:` in compose) — the ports reachable from
	// inside the Docker network, NOT the ports published on the host.
	// Empty means the container declares none, which is common; callers must
	// not treat that as "no port is reachable".
	Ports []int
}

type Client struct {
	cli *dockerclient.Client
	log *logger.Logger
}

func New(log *logger.Logger) (*Client, error) {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	return &Client{cli: cli, log: log}, nil
}

// InspectContainer returns ContainerInfo for the given name or ID.
// Returns (nil, nil) if the container does not exist.
func (c *Client) InspectContainer(ctx context.Context, nameOrID string) (*ContainerInfo, error) {
	resp, err := c.cli.ContainerInspect(ctx, nameOrID)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspecting container %q: %w", nameOrID, err)
	}
	nets := make([]string, 0, len(resp.NetworkSettings.Networks))
	for name := range resp.NetworkSettings.Networks {
		nets = append(nets, name)
	}
	// Docker returns these maps in random order; sort so logs and error
	// messages are stable across inspects.
	sort.Strings(nets)

	var ports []int
	if resp.Config != nil {
		for p := range resp.Config.ExposedPorts {
			if p.Proto() != "tcp" {
				continue
			}
			ports = append(ports, p.Int())
		}
		sort.Ints(ports)
	}

	name := strings.TrimPrefix(resp.Name, "/")
	return &ContainerInfo{
		ID:       resp.ID,
		Name:     name,
		Networks: nets,
		Ports:    ports,
	}, nil
}

// ListContainers returns every container on the host, running or not, so the
// web UI can offer a picker instead of asking the user to type a name exactly.
//
// Ports here come from the container list, which reports the container-internal
// port of each mapping — the same number a route needs, never the host-published
// one.
func (c *Client) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	out := make([]ContainerInfo, 0, len(list))
	for _, item := range list {
		// Slices start empty rather than nil: these values are serialised to
		// JSON for the web UI, and a nil slice becomes `null`, which breaks any
		// caller that expects a list. A container with no published port is
		// completely normal — with a tunnel it is the whole point — so this is
		// the common case, not an edge one.
		info := ContainerInfo{
			ID:       item.ID,
			Networks: []string{},
			Ports:    []int{},
		}
		if len(item.Names) > 0 {
			info.Name = strings.TrimPrefix(item.Names[0], "/")
		}
		for name := range item.NetworkSettings.Networks {
			info.Networks = append(info.Networks, name)
		}
		sort.Strings(info.Networks)

		seen := make(map[int]bool, len(item.Ports))
		for _, p := range item.Ports {
			if p.Type != "tcp" || p.PrivatePort == 0 || seen[int(p.PrivatePort)] {
				continue
			}
			seen[int(p.PrivatePort)] = true
			info.Ports = append(info.Ports, int(p.PrivatePort))
		}
		sort.Ints(info.Ports)

		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ConnectNetwork connects containerID to networkName. Idempotent.
func (c *Client) ConnectNetwork(ctx context.Context, networkName, containerID string) error {
	err := c.cli.NetworkConnect(ctx, networkName, containerID, &network.EndpointSettings{})
	if err != nil {
		// already connected — not an error
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("connecting to network %q: %w", networkName, err)
	}
	return nil
}

// DisconnectNetwork disconnects containerID from networkName. Idempotent.
func (c *Client) DisconnectNetwork(ctx context.Context, networkName, containerID string) error {
	err := c.cli.NetworkDisconnect(ctx, networkName, containerID, false)
	if err != nil {
		if strings.Contains(err.Error(), "is not connected") {
			return nil
		}
		return fmt.Errorf("disconnecting from network %q: %w", networkName, err)
	}
	return nil
}

// SelfContainerID returns the container ID of the running DockFlare process.
// Tier 1: parse /proc/self/cgroup for a Docker container path.
// Tier 2: use hostname (Docker sets it to the short container ID by default).
func SelfContainerID() (string, error) {
	data, err := os.ReadFile(cgroupFile)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) != 3 {
				continue
			}
			path := parts[2]
			// cgroup v1 path: /docker/<64-char-hex>
			after, ok := strings.CutPrefix(path, "/docker/")
			if !ok {
				continue
			}
			after = strings.TrimSpace(after)
			if len(after) == 64 && isHex(after) {
				return after[:12], nil
			}
		}
	}

	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("cannot determine self container ID: hostname unavailable: %w", err)
	}
	if len(hostname) == 12 && isHex(hostname) {
		return hostname, nil
	}
	return "", errors.New("cannot determine self container ID: not running inside a Docker container")
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
