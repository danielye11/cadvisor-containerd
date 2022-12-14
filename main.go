// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"sync"
	"time"

	ptypes "github.com/gogo/protobuf/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	snapshotapi "github.com/containerd/containerd/api/services/snapshots/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	versionapi "github.com/containerd/containerd/api/services/version/v1"
	"github.com/containerd/containerd/api/types"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/google/cadvisor/container/containerd/containers"
	"github.com/google/cadvisor/container/containerd/errdefs"
	"github.com/google/cadvisor/container/containerd/pkg/dialer"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

type client struct {
	containerService containersapi.ContainersClient
	taskService      tasksapi.TasksClient
	versionService   versionapi.VersionClient
	snapshotService  snapshotapi.SnapshotsClient
	criService       criapi.RuntimeServiceClient
}

type ContainerdClient interface {
	LoadContainer(ctx context.Context, id string) (*containers.Container, error)
	TaskPid(ctx context.Context, id string) (uint32, error)
	Version(ctx context.Context) (string, error)
	SnapshotMounts(ctx context.Context, snapshotter, key string) ([]*types.Mount, error)
	ContainerStatus(ctx context.Context, id string) (*criapi.ContainerStatus, error)
	ContainerStats(ctx context.Context, id string) (*criapi.ContainerStats, error)
}

var (
	ErrTaskIsInUnknownState = errors.New("containerd task is in unknown state") // used when process reported in containerd task is in Unknown State
)

var once sync.Once
var ctrdClient ContainerdClient = nil

var ArgContainerdEndpoint = flag.String("containerd", "/run/containerd/containerd.sock", "containerd endpoint")
var ArgContainerdNamespace = flag.String("containerd-namespace", "k8s.io", "containerd namespace")

const (
	maxBackoffDelay   = 3 * time.Second
	baseBackoffDelay  = 100 * time.Millisecond
	connectionTimeout = 2 * time.Second
)

// Client creates a containerd client
func Client(address, namespace string) (ContainerdClient, error) {
	var retErr error
	once.Do(func() {
		tryConn, err := net.DialTimeout("unix", address, connectionTimeout)
		if err != nil {
			retErr = fmt.Errorf("containerd: cannot unix dial containerd api service: %v", err)
			return
		}
		tryConn.Close()

		connParams := grpc.ConnectParams{
			Backoff: backoff.DefaultConfig,
		}
		connParams.Backoff.BaseDelay = baseBackoffDelay
		connParams.Backoff.MaxDelay = maxBackoffDelay
		gopts := []grpc.DialOption{
			grpc.WithInsecure(),
			grpc.WithContextDialer(dialer.ContextDialer),
			grpc.WithBlock(),
			grpc.WithConnectParams(connParams),
		}
		unary, stream := newNSInterceptors(namespace)
		gopts = append(gopts,
			grpc.WithUnaryInterceptor(unary),
			grpc.WithStreamInterceptor(stream),
		)

		ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
		defer cancel()
		conn, err := grpc.DialContext(ctx, dialer.DialAddress(address), gopts...)
		if err != nil {
			retErr = err
			return
		}
		ctrdClient = &client{
			containerService: containersapi.NewContainersClient(conn),
			taskService:      tasksapi.NewTasksClient(conn),
			versionService:   versionapi.NewVersionClient(conn),
			snapshotService:  snapshotapi.NewSnapshotsClient(conn),
			criService:       criapi.NewRuntimeServiceClient(conn),
		}
	})
	return ctrdClient, retErr
}

func (c *client) LoadContainer(ctx context.Context, id string) (*containers.Container, error) {
	r, err := c.containerService.Get(ctx, &containersapi.GetContainerRequest{
		ID: id,
	})
	if err != nil {
		return nil, errdefs.FromGRPC(err)
	}
	return containerFromProto(r.Container), nil
}

func (c *client) TaskPid(ctx context.Context, id string) (uint32, error) {
	response, err := c.taskService.Get(ctx, &tasksapi.GetRequest{
		ContainerID: id,
	})
	if err != nil {
		return 0, errdefs.FromGRPC(err)
	}
	if response.Process.Status == tasktypes.StatusUnknown {
		return 0, ErrTaskIsInUnknownState
	}
	return response.Process.Pid, nil
}

func (c *client) Version(ctx context.Context) (string, error) {
	response, err := c.versionService.Version(ctx, &ptypes.Empty{})
	if err != nil {
		return "", errdefs.FromGRPC(err)
	}
	return response.Version, nil
}

func (c *client) SnapshotMounts(ctx context.Context, snapshotter, key string) ([]*types.Mount, error) {
	response, err := c.snapshotService.Mounts(ctx, &snapshotapi.MountsRequest{
		Snapshotter: snapshotter,
		Key:         key,
	})
	if err != nil {
		return nil, errdefs.FromGRPC(err)
	}
	return response.Mounts, nil
}

func (c *client) ContainerStatus(ctx context.Context, id string) (*criapi.ContainerStatus, error) {
	response, err := c.criService.ContainerStatus(ctx, &criapi.ContainerStatusRequest{
		ContainerId: id,
		Verbose:     false,
	})
	if err != nil {
		return nil, err
	}
	return response.Status, nil
}

func (c *client) ContainerStats(ctx context.Context, id string) (*criapi.ContainerStats, error) {
	response, err := c.criService.ContainerStats(ctx, &criapi.ContainerStatsRequest{
		ContainerId: id,
	})
	if err != nil {
		return nil, err
	}
	return response.Stats, nil
}

func containerFromProto(containerpb containersapi.Container) *containers.Container {
	var runtime containers.RuntimeInfo
	if containerpb.Runtime != nil {
		runtime = containers.RuntimeInfo{
			Name:    containerpb.Runtime.Name,
			Options: containerpb.Runtime.Options,
		}
	}
	return &containers.Container{
		ID:          containerpb.ID,
		Labels:      containerpb.Labels,
		Image:       containerpb.Image,
		Runtime:     runtime,
		Spec:        containerpb.Spec,
		Snapshotter: containerpb.Snapshotter,
		SnapshotKey: containerpb.SnapshotKey,
		Extensions:  containerpb.Extensions,
	}
}

func main() {
	fmt.Println("Hello, Worlds!")
	client, err := Client(*ArgContainerdEndpoint, *ArgContainerdNamespace)
	fmt.Println(5)
	fmt.Println(client.ContainerStats(context.TODO(), "test"))
	fmt.Println(err)
	fmt.Println(client)
}
