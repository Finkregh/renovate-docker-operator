// Package containerLogs reads and streams logs from Docker containers.
// Replaces the upstream podLogs package that reads from Kubernetes pods.
package containerLogs

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// DockerLogReader reads logs from Docker containers.
type DockerLogReader struct {
	docker *client.Client
}

// New creates a new DockerLogReader.
func New(docker *client.Client) *DockerLogReader {
	return &DockerLogReader{docker: docker}
}

// StreamLogs returns a reader for container logs (stdout+stderr multiplexed).
func (r *DockerLogReader) StreamLogs(ctx context.Context, containerID string, follow bool) (io.ReadCloser, error) {
	return r.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Timestamps: false,
	})
}

// GetLogs returns all logs for a completed container.
func (r *DockerLogReader) GetLogs(ctx context.Context, containerID string) ([]byte, error) {
	reader, err := r.StreamLogs(ctx, containerID, false)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}
