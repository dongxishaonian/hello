/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package compose

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/types"
	"github.com/containerd/containerd/platforms"
	moby "github.com/docker/docker/api/types"
	containerType "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/docker/compose/v2/pkg/utils"
)

const (
	extLifecycle  = "x-lifecycle"
	forceRecreate = "force_recreate"

	doubledContainerNameWarning = "WARNING: The %q service is using the custom container name %q. " +
		"Docker requires each container to have a unique name. " +
		"Remove the custom name to scale the service.\n"
)

// convergence manages service's container lifecycle.
// Based on initially observed state, it reconciles the existing container with desired state, which might include
// re-creating container, adding or removing replicas, or starting stopped containers.
// Cross services dependencies are managed by creating services in expected order and updating `service:xx` reference
// when a service has converged, so dependent ones can be managed with resolved containers references.
type convergence struct {
	service       *composeService
	observedState map[string]Containers
	stateMutex    sync.Mutex
}

func (c *convergence) getObservedState(serviceName string) Containers {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	return c.observedState[serviceName]
}

func (c *convergence) setObservedState(serviceName string, containers Containers) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	c.observedState[serviceName] = containers
}

func newConvergence(services []string, state Containers, s *composeService) *convergence {
	observedState := map[string]Containers{}
	for _, s := range services {
		observedState[s] = Containers{}
	}
	for _, c := range state.filter(isNotOneOff) {
		service := c.Labels[api.ServiceLabel]
		observedState[service] = append(observedState[service], c)
	}
	return &convergence{
		service:       s,
		observedState: observedState,
	}
}

func (c *convergence) apply(ctx context.Context, project *types.Project, options api.CreateOptions) error {
	return InDependencyOrder(ctx, project, func(ctx context.Context, name string) error {
		service, err := project.GetService(name)
		if err != nil {
			return err
		}

		strategy := options.RecreateDependencies
		if utils.StringContains(options.Services, name) {
			strategy = options.Recreate
		}
		err = c.ensureService(ctx, project, service, strategy, options.Inherit, options.Timeout)
		if err != nil {
			return err
		}

		c.updateProject(project, name)
		return nil
	})
}

var mu sync.Mutex

// updateProject updates project after service converged, so dependent services relying on `service:xx` can refer to actual containers.
func (c *convergence) updateProject(project *types.Project, serviceName string) {
	// operation is protected by a Mutex so that we can safely update project.Services while running concurrent convergence on services
	mu.Lock()
	defer mu.Unlock()

	cnts := c.getObservedState(serviceName)
	for i, s := range project.Services {
		updateServices(&s, cnts)
		project.Services[i] = s
	}
}

func updateServices(service *types.ServiceConfig, cnts Containers) {
	if len(cnts) == 0 {
		return
	}
	cnt := cnts[0]
	serviceName := cnt.Labels[api.ServiceLabel]

	if d := getDependentServiceFromMode(service.NetworkMode); d == serviceName {
		service.NetworkMode = types.NetworkModeContainerPrefix + cnt.ID
	}
	if d := getDependentServiceFromMode(service.Ipc); d == serviceName {
		service.Ipc = types.NetworkModeContainerPrefix + cnt.ID
	}
	if d := getDependentServiceFromMode(service.Pid); d == serviceName {
		service.Pid = types.NetworkModeContainerPrefix + cnt.ID
	}
	var links []string
	for _, serviceLink := range service.Links {
		parts := strings.Split(serviceLink, ":")
		serviceName := serviceLink
		serviceAlias := ""
		if len(parts) == 2 {
			serviceName = parts[0]
			serviceAlias = parts[1]
		}
		if serviceName != service.Name {
			links = append(links, serviceLink)
			continue
		}
		for _, container := range cnts {
			name := getCanonicalContainerName(container)
			if serviceAlias != "" {
				links = append(links,
					fmt.Sprintf("%s:%s", name, serviceAlias))
			}
			links = append(links,
				fmt.Sprintf("%s:%s", name, name),
				fmt.Sprintf("%s:%s", name, getContainerNameWithoutProject(container)))
		}
		service.Links = links
	}
}

func (c *convergence) ensureService(ctx context.Context, project *types.Project, service types.ServiceConfig, recreate string, inherit bool, timeout *time.Duration) error {
	expected, err := getScale(service)
	if err != nil {
		return err
	}
	containers := c.getObservedState(service.Name)
	actual := len(containers)
	updated := make(Containers, expected)

	eg, _ := errgroup.WithContext(ctx)

	for i, container := range containers {
		if i >= expected {
			// Scale Down
			container := container
			eg.Go(func() error {
				timeoutInSecond := utils.DurationSecondToInt(timeout)
				err := c.service.apiClient().ContainerStop(ctx, container.ID, containerType.StopOptions{
					Timeout: timeoutInSecond,
				})
				if err != nil {
					return err
				}
				return c.service.apiClient().ContainerRemove(ctx, container.ID, moby.ContainerRemoveOptions{})
			})
			continue
		}

		mustRecreate, err := mustRecreate(service, container, recreate)
		if err != nil {
			return err
		}
		if mustRecreate {
			i, container := i, container
			eg.Go(func() error {
				recreated, err := c.service.recreateContainer(ctx, project, service, container, inherit, timeout)
				updated[i] = recreated
				return err
			})
			continue
		}

		// Enforce non-diverged containers are running
		w := progress.ContextWriter(ctx)
		name := getContainerProgressName(container)
		switch container.State {
		case ContainerRunning:
			w.Event(progress.RunningEvent(name))
		case ContainerCreated:
		case ContainerRestarting:
		case ContainerExited:
			w.Event(progress.CreatedEvent(name))
		default:
			container := container
			eg.Go(func() error {
				return c.service.startContainer(ctx, container)
			})
		}
		updated[i] = container
	}

	next := nextContainerNumber(containers)
	for i := 0; i < expected-actual; i++ {
		// Scale UP
		number := next + i
		name := getContainerName(project.Name, service, number)
		i := i
		eg.Go(func() error {
			container, err := c.service.createContainer(ctx, project, service, name, number, false, true, false)
			updated[actual+i] = container
			return err
		})
		continue
	}

	err = eg.Wait()
	c.setObservedState(service.Name, updated)
	return err
}

func mustRecreate(expected types.ServiceConfig, actual moby.Container, policy string) (bool, error) {
	if policy == api.RecreateNever {
		return false, nil
	}
	if policy == api.RecreateForce || expected.Extensions[extLifecycle] == forceRecreate {
		return true, nil
	}
	configHash, err := ServiceHash(expected)
	if err != nil {
		return false, err
	}
	configChanged := actual.Labels[api.ConfigHashLabel] != configHash
	imageUpdated := actual.Labels[api.ImageDigestLabel] != expected.CustomLabels[api.ImageDigestLabel]
	return configChanged || imageUpdated, nil
}

func getContainerName(projectName string, service types.ServiceConfig, number int) string {
	name := strings.Join([]string{projectName, service.Name, strconv.Itoa(number)}, api.Separator)
	if service.ContainerName != "" {
		name = service.ContainerName
	}
	return name
}

func getContainerProgressName(container moby.Container) string {
	return "Container " + getCanonicalContainerName(container)
}

func containerEvents(containers Containers, eventFunc func(string) progress.Event) []progress.Event {
	events := []progress.Event{}
	for _, container := range containers {
		events = append(events, eventFunc(getContainerProgressName(container)))
	}
	return events
}

// ServiceConditionRunningOrHealthy is a service condition on statys running or healthy
const ServiceConditionRunningOrHealthy = "running_or_healthy"

func (s *composeService) waitDependencies(ctx context.Context, project *types.Project, dependencies types.DependsOnConfig) error {
	eg, _ := errgroup.WithContext(ctx)
	w := progress.ContextWriter(ctx)
	for dep, config := range dependencies {
		if shouldWait, err := shouldWaitForDependency(dep, config, project); err != nil {
			return err
		} else if !shouldWait {
			continue
		}

		containers, err := s.getContainers(ctx, project.Name, oneOffExclude, false, dep)
		if err != nil {
			return err
		}
		w.Events(containerEvents(containers, progress.Waiting))

		dep, config := dep, config
		eg.Go(func() error {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				<-ticker.C
				switch config.Condition {
				case ServiceConditionRunningOrHealthy:
					healthy, err := s.isServiceHealthy(ctx, project, dep, true)
					if err != nil {
						return err
					}
					if healthy {
						w.Events(containerEvents(containers, progress.Healthy))
						return nil
					}
				case types.ServiceConditionHealthy:
					healthy, err := s.isServiceHealthy(ctx, project, dep, false)
					if err != nil {
						return err
					}
					if healthy {
						w.Events(containerEvents(containers, progress.Healthy))
						return nil
					}
				case types.ServiceConditionCompletedSuccessfully:
					exited, code, err := s.isServiceCompleted(ctx, project, dep)
					if err != nil {
						return err
					}
					if exited {
						w.Events(containerEvents(containers, progress.Exited))
						if code != 0 {
							return fmt.Errorf("service %q didn't completed successfully: exit %d", dep, code)
						}
						return nil
					}
				default:
					logrus.Warnf("unsupported depends_on condition: %s", config.Condition)
					return nil
				}
			}
		})
	}
	return eg.Wait()
}

func shouldWaitForDependency(serviceName string, dependencyConfig types.ServiceDependency, project *types.Project) (bool, error) {
	if dependencyConfig.Condition == types.ServiceConditionStarted {
		// already managed by InDependencyOrder
		return false, nil
	}
	if service, err := project.GetService(serviceName); err != nil {
		for _, ds := range project.DisabledServices {
			if ds.Name == serviceName {
				// don't wait for disabled service (--no-deps)
				return false, nil
			}
		}
		return false, err
	} else if service.Scale == 0 {
		// don't wait for the dependency which configured to have 0 containers running
		return false, nil
	}
	return true, nil
}

func nextContainerNumber(containers []moby.Container) int {
	max := 0
	for _, c := range containers {
		s, ok := c.Labels[api.ContainerNumberLabel]
		if !ok {
			logrus.Warnf("container %s is missing %s label", c.ID, api.ContainerNumberLabel)
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			logrus.Warnf("container %s has invalid %s label: %s", c.ID, api.ContainerNumberLabel, s)
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1

}

func getScale(config types.ServiceConfig) (int, error) {
	scale := 1
	if config.Deploy != nil && config.Deploy.Replicas != nil {
		scale = int(*config.Deploy.Replicas)
	}
	if scale > 1 && config.ContainerName != "" {
		return 0, fmt.Errorf(doubledContainerNameWarning,
			config.Name,
			config.ContainerName)
	}
	return scale, nil
}

func (s *composeService) createContainer(ctx context.Context, project *types.Project, service types.ServiceConfig,
	name string, number int, autoRemove bool, useNetworkAliases bool, attachStdin bool) (container moby.Container, err error) {
	w := progress.ContextWriter(ctx)
	eventName := "Container " + name
	w.Event(progress.CreatingEvent(eventName))
	container, err = s.createMobyContainer(ctx, project, service, name, number, nil, autoRemove, useNetworkAliases, attachStdin, w)
	if err != nil {
		return
	}
	w.Event(progress.CreatedEvent(eventName))
	return
}

func (s *composeService) recreateContainer(ctx context.Context, project *types.Project, service types.ServiceConfig,
	replaced moby.Container, inherit bool, timeout *time.Duration) (moby.Container, error) {
	var created moby.Container
	w := progress.ContextWriter(ctx)
	w.Event(progress.NewEvent(getContainerProgressName(replaced), progress.Working, "Recreate"))
	timeoutInSecond := utils.DurationSecondToInt(timeout)
	err := s.apiClient().ContainerStop(ctx, replaced.ID, containerType.StopOptions{Timeout: timeoutInSecond})
	if err != nil {
		return created, err
	}
	name := getCanonicalContainerName(replaced)
	tmpName := fmt.Sprintf("%s_%s", replaced.ID[:12], name)
	err = s.apiClient().ContainerRename(ctx, replaced.ID, tmpName)
	if err != nil {
		return created, err
	}
	number, err := strconv.Atoi(replaced.Labels[api.ContainerNumberLabel])
	if err != nil {
		return created, err
	}

	var inherited *moby.Container
	if inherit {
		inherited = &replaced
	}
	name = getContainerName(project.Name, service, number)
	created, err = s.createMobyContainer(ctx, project, service, name, number, inherited, false, true, false, w)
	if err != nil {
		return created, err
	}
	err = s.apiClient().ContainerRemove(ctx, replaced.ID, moby.ContainerRemoveOptions{})
	if err != nil {
		return created, err
	}
	w.Event(progress.NewEvent(getContainerProgressName(replaced), progress.Done, "Recreated"))
	setDependentLifecycle(project, service.Name, forceRecreate)
	return created, err
}

// setDependentLifecycle define the Lifecycle strategy for all services to depend on specified service
func setDependentLifecycle(project *types.Project, service string, strategy string) {
	for i, s := range project.Services {
		if utils.StringContains(s.GetDependencies(), service) {
			if s.Extensions == nil {
				s.Extensions = map[string]interface{}{}
			}
			s.Extensions[extLifecycle] = strategy
			project.Services[i] = s
		}
	}
}

func (s *composeService) startContainer(ctx context.Context, container moby.Container) error {
	w := progress.ContextWriter(ctx)
	w.Event(progress.NewEvent(getContainerProgressName(container), progress.Working, "Restart"))
	err := s.apiClient().ContainerStart(ctx, container.ID, moby.ContainerStartOptions{})
	if err != nil {
		return err
	}
	w.Event(progress.NewEvent(getContainerProgressName(container), progress.Done, "Restarted"))
	return nil
}

func (s *composeService) createMobyContainer(ctx context.Context, project *types.Project, service types.ServiceConfig,
	name string, number int, inherit *moby.Container, autoRemove bool, useNetworkAliases bool, attachStdin bool, w progress.Writer) (moby.Container, error) {
	var created moby.Container
	containerConfig, hostConfig, networkingConfig, err := s.getCreateOptions(ctx, project, service, number, inherit, autoRemove, attachStdin)
	if err != nil {
		return created, err
	}
	var plat *specs.Platform
	if service.Platform != "" {
		var p specs.Platform
		p, err = platforms.Parse(service.Platform)
		if err != nil {
			return created, err
		}
		plat = &p
	}
	response, err := s.apiClient().ContainerCreate(ctx, containerConfig, hostConfig, networkingConfig, plat, name)
	if err != nil {
		return created, err
	}
	for _, warning := range response.Warnings {
		w.Event(progress.Event{
			ID:     service.Name,
			Status: progress.Warning,
			Text:   warning,
		})
	}
	inspectedContainer, err := s.apiClient().ContainerInspect(ctx, response.ID)
	if err != nil {
		return created, err
	}
	created = moby.Container{
		ID:     inspectedContainer.ID,
		Labels: inspectedContainer.Config.Labels,
		Names:  []string{inspectedContainer.Name},
		NetworkSettings: &moby.SummaryNetworkSettings{
			Networks: inspectedContainer.NetworkSettings.Networks,
		},
	}
	links, err := s.getLinks(ctx, project.Name, service, number)
	if err != nil {
		return created, err
	}
	for _, netName := range service.NetworksByPriority() {
		netwrk := project.Networks[netName]
		cfg := service.Networks[netName]
		aliases := []string{getContainerName(project.Name, service, number)}
		if useNetworkAliases {
			aliases = append(aliases, service.Name)
			if cfg != nil {
				aliases = append(aliases, cfg.Aliases...)
			}
		}
		if val, ok := created.NetworkSettings.Networks[netwrk.Name]; ok {
			if shortIDAliasExists(created.ID, val.Aliases...) {
				continue
			}
			err = s.apiClient().NetworkDisconnect(ctx, netwrk.Name, created.ID, false)
			if err != nil {
				return created, err
			}
		}
		err = s.connectContainerToNetwork(ctx, created.ID, netwrk.Name, cfg, links, aliases...)
		if err != nil {
			return created, err
		}
	}

	err = s.injectSecrets(ctx, project, service, created.ID)
	return created, err
}

// getLinks mimics V1 compose/service.py::Service::_get_links()
func (s composeService) getLinks(ctx context.Context, projectName string, service types.ServiceConfig, number int) ([]string, error) {
	var links []string
	format := func(k, v string) string {
		return fmt.Sprintf("%s:%s", k, v)
	}
	getServiceContainers := func(serviceName string) (Containers, error) {
		return s.getContainers(ctx, projectName, oneOffExclude, true, serviceName)
	}

	for _, rawLink := range service.Links {
		linkSplit := strings.Split(rawLink, ":")
		linkServiceName := linkSplit[0]
		linkName := linkServiceName
		if len(linkSplit) == 2 {
			linkName = linkSplit[1] // linkName if informed like in: "serviceName:linkName"
		}
		cnts, err := getServiceContainers(linkServiceName)
		if err != nil {
			return nil, err
		}
		for _, c := range cnts {
			containerName := getCanonicalContainerName(c)
			links = append(links,
				format(containerName, linkName),
				format(containerName, linkServiceName+api.Separator+strconv.Itoa(number)),
				format(containerName, strings.Join([]string{projectName, linkServiceName, strconv.Itoa(number)}, api.Separator)),
			)
		}
	}

	if service.Labels[api.OneoffLabel] == "True" {
		cnts, err := getServiceContainers(service.Name)
		if err != nil {
			return nil, err
		}
		for _, c := range cnts {
			containerName := getCanonicalContainerName(c)
			links = append(links,
				format(containerName, service.Name),
				format(containerName, strings.TrimPrefix(containerName, projectName+api.Separator)),
				format(containerName, containerName),
			)
		}
	}

	for _, rawExtLink := range service.ExternalLinks {
		extLinkSplit := strings.Split(rawExtLink, ":")
		externalLink := extLinkSplit[0]
		linkName := externalLink
		if len(extLinkSplit) == 2 {
			linkName = extLinkSplit[1]
		}
		links = append(links, format(externalLink, linkName))
	}
	return links, nil
}

func shortIDAliasExists(containerID string, aliases ...string) bool {
	for _, alias := range aliases {
		if alias == containerID[:12] {
			return true
		}
	}
	return false
}

func (s *composeService) connectContainerToNetwork(ctx context.Context, id string, netwrk string, cfg *types.ServiceNetworkConfig, links []string, aliases ...string) error {
	var (
		ipv4Address string
		ipv6Address string
		ipam        *network.EndpointIPAMConfig
	)
	if cfg != nil {
		ipv4Address = cfg.Ipv4Address
		ipv6Address = cfg.Ipv6Address
		ipam = &network.EndpointIPAMConfig{
			IPv4Address:  ipv4Address,
			IPv6Address:  ipv6Address,
			LinkLocalIPs: cfg.LinkLocalIPs,
		}
	}
	err := s.apiClient().NetworkConnect(ctx, netwrk, id, &network.EndpointSettings{
		Aliases:           aliases,
		IPAddress:         ipv4Address,
		GlobalIPv6Address: ipv6Address,
		Links:             links,
		IPAMConfig:        ipam,
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *composeService) isServiceHealthy(ctx context.Context, project *types.Project, service string, fallbackRunning bool) (bool, error) {
	containers, err := s.getContainers(ctx, project.Name, oneOffExclude, false, service)
	if err != nil {
		return false, err
	}

	if len(containers) == 0 {
		return false, nil
	}
	for _, c := range containers {
		container, err := s.apiClient().ContainerInspect(ctx, c.ID)
		if err != nil {
			return false, err
		}
		if container.Config.Healthcheck == nil && fallbackRunning {
			// Container does not define a health check, but we can fall back to "running" state
			return container.State != nil && container.State.Status == "running", nil
		}

		if container.State == nil || container.State.Health == nil {
			return false, fmt.Errorf("container for service %q has no healthcheck configured", service)
		}
		switch container.State.Health.Status {
		case moby.Healthy:
			// Continue by checking the next container.
		case moby.Unhealthy:
			return false, fmt.Errorf("container for service %q is unhealthy", service)
		case moby.Starting:
			return false, nil
		default:
			return false, fmt.Errorf("container for service %q had unexpected health status %q", service, container.State.Health.Status)
		}
	}
	return true, nil
}

func (s *composeService) isServiceCompleted(ctx context.Context, project *types.Project, dep string) (bool, int, error) {
	containers, err := s.getContainers(ctx, project.Name, oneOffExclude, true, dep)
	if err != nil {
		return false, 0, err
	}
	for _, c := range containers {
		container, err := s.apiClient().ContainerInspect(ctx, c.ID)
		if err != nil {
			return false, 0, err
		}
		if container.State != nil && container.State.Status == "exited" {
			return true, container.State.ExitCode, nil
		}
	}
	return false, 0, nil
}

func (s *composeService) startService(ctx context.Context, project *types.Project, service types.ServiceConfig) error {
	if service.Deploy != nil && service.Deploy.Replicas != nil && *service.Deploy.Replicas == 0 {
		return nil
	}

	err := s.waitDependencies(ctx, project, service.DependsOn)
	if err != nil {
		return err
	}
	containers, err := s.apiClient().ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(project.Name),
			serviceFilter(service.Name),
			oneOffFilter(false),
		),
		All: true,
	})
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		if scale, err := getScale(service); err != nil && scale == 0 {
			return nil
		}
		return fmt.Errorf("service %q has no container to start", service.Name)
	}

	w := progress.ContextWriter(ctx)
	eg, ctx := errgroup.WithContext(ctx)
	for _, container := range containers {
		if container.State == ContainerRunning {
			continue
		}
		container := container
		eg.Go(func() error {
			eventName := getContainerProgressName(container)
			w.Event(progress.StartingEvent(eventName))
			err := s.apiClient().ContainerStart(ctx, container.ID, moby.ContainerStartOptions{})
			if err == nil {
				w.Event(progress.StartedEvent(eventName))
			}
			return err
		})
	}
	return eg.Wait()
}
