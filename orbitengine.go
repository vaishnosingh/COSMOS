package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// DockerClient returns an http.Client communicating directly with the Docker daemon unix socket.
func DockerClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", "/var/run/docker.sock")
			},
		},
		Timeout: 0, // Infinite timeout for long-lived operations (build, stream logs)
	}
}

// DockerPing verifies the Docker daemon is reachable.
func DockerPing(ctx context.Context) error {
	client := DockerClient()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1.45/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("docker ping failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker ping returned status %d", resp.StatusCode)
	}
	return nil
}

// DockerContainerSummary represents basic container metadata from Docker API.
type DockerContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
	Ports   []struct {
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
		IP          string `json:"IP"`
	} `json:"Ports"`
}

// ListOrbitContainers returns all containers managed by Orbit (labeled with orbit=true).
func ListOrbitContainers(ctx context.Context) ([]DockerContainerSummary, error) {
	client := DockerClient()
	u := "http://localhost/v1.45/containers/json?all=1"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var all []DockerContainerSummary
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, err
	}

	var orbitContainers []DockerContainerSummary
	for _, c := range all {
		if c.Labels != nil && c.Labels["cosmos.orbit"] == "true" {
			orbitContainers = append(orbitContainers, c)
		}
	}
	return orbitContainers, nil
}

// InspectDockerContainer returns detailed information about a container.
func InspectDockerContainer(ctx context.Context, containerID string) (map[string]interface{}, error) {
	client := DockerClient()
	u := fmt.Sprintf("http://localhost/v1.45/containers/%s/json", url.PathEscape(containerID))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("container not found")
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// StartDockerContainer starts a stopped container.
func StartDockerContainer(ctx context.Context, containerID string) error {
	client := DockerClient()
	u := fmt.Sprintf("http://localhost/v1.45/containers/%s/start", url.PathEscape(containerID))
	req, err := http.NewRequestWithContext(ctx, "POST", u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to start container: %s", string(body))
	}
	return nil
}

// StopDockerContainer gracefully stops a container (10s timeout).
func StopDockerContainer(ctx context.Context, containerID string) error {
	client := DockerClient()
	u := fmt.Sprintf("http://localhost/v1.45/containers/%s/stop?t=10", url.PathEscape(containerID))
	req, err := http.NewRequestWithContext(ctx, "POST", u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to stop container: %s", string(body))
	}
	return nil
}

// RestartDockerContainer restarts a container.
func RestartDockerContainer(ctx context.Context, containerID string) error {
	client := DockerClient()
	u := fmt.Sprintf("http://localhost/v1.45/containers/%s/restart?t=10", url.PathEscape(containerID))
	req, err := http.NewRequestWithContext(ctx, "POST", u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to restart container: %s", string(body))
	}
	return nil
}

// RemoveDockerContainer stops and removes a container.
func RemoveDockerContainer(ctx context.Context, containerID string) error {
	client := DockerClient()
	u := fmt.Sprintf("http://localhost/v1.45/containers/%s?force=true&v=true", url.PathEscape(containerID))
	req, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to remove container: %s", string(body))
	}
	return nil
}

// OrbitContainerSpec holds configuration for spawning a container.
type OrbitContainerSpec struct {
	Name          string
	Image         string
	Env           []string
	AppID         string
	InternalPort  int // e.g. 3000, 8080
	ExposedPort   int // e.g. 4100
	MemLimitMB    int // e.g. 512
	CPULimit      float64 // e.g. 1.0
}

// CreateAndStartOrbitContainer creates and starts an Orbit-managed container.
func CreateAndStartOrbitContainer(ctx context.Context, spec OrbitContainerSpec) (string, error) {
	client := DockerClient()

	intPortStr := fmt.Sprintf("%d/tcp", spec.InternalPort)
	hostPortStr := fmt.Sprintf("%d", spec.ExposedPort)

	memBytes := int64(spec.MemLimitMB) * 1024 * 1024
	if memBytes <= 0 {
		memBytes = 512 * 1024 * 1024 // 512MB default
	}

	nanoCPUs := int64(spec.CPULimit * 1e9)
	if nanoCPUs <= 0 {
		nanoCPUs = 1e9 // 1 CPU default
	}

	createPayload := map[string]interface{}{
		"Image": spec.Image,
		"Env":   spec.Env,
		"Labels": map[string]string{
			"cosmos.orbit":    "true",
			"cosmos.app_id":   spec.AppID,
			"cosmos.app_name": spec.Name,
		},
		"ExposedPorts": map[string]interface{}{
			intPortStr: map[string]interface{}{},
		},
		"HostConfig": map[string]interface{}{
			"PortBindings": map[string]interface{}{
				intPortStr: []map[string]string{
					{
						"HostIp":   "0.0.0.0",
						"HostPort": hostPortStr,
					},
				},
			},
			"RestartPolicy": map[string]string{
				"Name": "unless-stopped",
			},
			"Memory":   memBytes,
			"NanoCpus": nanoCPUs,
		},
	}

	payloadBytes, err := json.Marshal(createPayload)
	if err != nil {
		return "", err
	}

	createURL := fmt.Sprintf("http://localhost/v1.45/containers/create?name=%s", url.QueryEscape("orbit-"+spec.AppID))
	req, err := http.NewRequestWithContext(ctx, "POST", createURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("create container request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("docker create returned %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	// Start the container
	if err := StartDockerContainer(ctx, res.ID); err != nil {
		// Clean up on failure
		_ = RemoveDockerContainer(ctx, res.ID)
		return "", fmt.Errorf("failed to start container: %w", err)
	}

	return res.ID, nil
}

// BuildDockerImage builds a Docker image from a tar stream of the build context.
func BuildDockerImage(ctx context.Context, tarStream io.Reader, tag string, logWriter io.Writer) error {
	client := DockerClient()
	buildURL := fmt.Sprintf("http://localhost/v1.45/build?t=%s&rm=1&forcerm=1", url.QueryEscape(tag))

	req, err := http.NewRequestWithContext(ctx, "POST", buildURL, tarStream)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("build request failed: %w", err)
	}
	defer resp.Body.Close()

	// Stream build JSON messages: {"stream": "Step 1/5 : FROM alpine\n"}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
			Status string `json:"status"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if msg.Error != "" {
			if logWriter != nil {
				fmt.Fprintf(logWriter, "ERROR: %s\n", msg.Error)
			}
			return fmt.Errorf("docker build error: %s", msg.Error)
		}
		if msg.Stream != "" && logWriter != nil {
			io.WriteString(logWriter, msg.Stream)
		} else if msg.Status != "" && logWriter != nil {
			fmt.Fprintf(logWriter, "%s\n", msg.Status)
		}
	}

	return nil
}

// ContainerLiveStats holds live CPU, RAM, and network metrics.
type ContainerLiveStats struct {
	CPUPct     float64 `json:"cpu_percent"`
	MemUsageMB float64 `json:"mem_usage_mb"`
	MemLimitMB float64 `json:"mem_limit_mb"`
	MemPct     float64 `json:"mem_percent"`
	NetRxMB    float64 `json:"net_rx_mb"`
	NetTxMB    float64 `json:"net_tx_mb"`
}

// GetContainerStats queries live resource utilization for a container.
func GetContainerStats(ctx context.Context, containerID string) (*ContainerLiveStats, error) {
	client := DockerClient()
	u := fmt.Sprintf("http://localhost/v1.45/containers/%s/stats?stream=false", url.PathEscape(containerID))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker stats returned status %d", resp.StatusCode)
	}

	var s struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage  uint64   `json:"total_usage"`
				PercpuUsage []uint64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs     int    `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Limit uint64 `json:"limit"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		} `json:"networks"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}

	// Calculate CPU %
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemCPUUsage) - float64(s.PreCPUStats.SystemCPUUsage)
	cpuPct := 0.0
	numCPUs := s.CPUStats.OnlineCPUs
	if numCPUs <= 0 {
		numCPUs = len(s.CPUStats.CPUUsage.PercpuUsage)
	}
	if numCPUs <= 0 {
		numCPUs = 1
	}
	if systemDelta > 0 && cpuDelta > 0 {
		cpuPct = (cpuDelta / systemDelta) * float64(numCPUs) * 100.0
	}

	// Memory
	memMB := float64(s.MemoryStats.Usage) / (1024 * 1024)
	limitMB := float64(s.MemoryStats.Limit) / (1024 * 1024)
	memPct := 0.0
	if limitMB > 0 {
		memPct = (memMB / limitMB) * 100.0
	}

	// Network
	var rxTotal, txTotal uint64
	for _, n := range s.Networks {
		rxTotal += n.RxBytes
		txTotal += n.TxBytes
	}

	return &ContainerLiveStats{
		CPUPct:     cpuPct,
		MemUsageMB: memMB,
		MemLimitMB: limitMB,
		MemPct:     memPct,
		NetRxMB:    float64(rxTotal) / (1024 * 1024),
		NetTxMB:    float64(txTotal) / (1024 * 1024),
	}, nil
}

// StreamDockerLogs streams container stdout/stderr. Docker demultiplexes with 8-byte frames.
func StreamDockerLogs(ctx context.Context, containerID string, tail string, follow bool, w io.Writer) error {
	client := DockerClient()
	followVal := "0"
	if follow {
		followVal = "1"
	}
	if tail == "" {
		tail = "200"
	}

	u := fmt.Sprintf("http://localhost/v1.45/containers/%s/logs?stdout=1&stderr=1&tail=%s&follow=%s&timestamps=1",
		url.PathEscape(containerID), url.QueryEscape(tail), followVal)

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to fetch logs: %s", string(body))
	}

	// Demultiplex Docker log stream:
	// Header: 8 bytes -> [STREAM_TYPE, 0, 0, 0, SIZE1, SIZE2, SIZE3, SIZE4]
	hdr := make([]byte, 8)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_, err := io.ReadFull(resp.Body, hdr)
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "closed") {
				return nil
			}
			return err
		}

		payloadSize := binary.BigEndian.Uint32(hdr[4:8])
		if payloadSize > 0 {
			// Read payload
			lr := io.LimitReader(resp.Body, int64(payloadSize))
			if _, err := io.Copy(w, lr); err != nil {
				return err
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}
