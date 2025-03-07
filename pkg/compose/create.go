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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/types"
	moby "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/blkiodev"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	volume_api "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	"github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/docker/compose/v2/pkg/utils"
)

func (s *composeService) Create(ctx context.Context, project *types.Project, options api.CreateOptions) error {
	return progress.Run(ctx, func(ctx context.Context) error {
		return s.create(ctx, project, options)
	})
}

func (s *composeService) create(ctx context.Context, project *types.Project, options api.CreateOptions) error {
	if len(options.Services) == 0 {
		options.Services = project.ServiceNames()
	}

	var observedState Containers
	observedState, err := s.getContainers(ctx, project.Name, oneOffInclude, true)
	if err != nil {
		return err
	}

	err = s.ensureImagesExists(ctx, project, options.QuietPull)
	if err != nil {
		return err
	}

	prepareNetworks(project)

	err = prepareVolumes(project)
	if err != nil {
		return err
	}

	if err := s.ensureNetworks(ctx, project.Networks); err != nil {
		return err
	}

	if err := s.ensureProjectVolumes(ctx, project); err != nil {
		return err
	}

	allServices := project.AllServices()
	allServiceNames := []string{}
	for _, service := range allServices {
		allServiceNames = append(allServiceNames, service.Name)
	}
	orphans := observedState.filter(isNotService(allServiceNames...))
	if len(orphans) > 0 && !options.IgnoreOrphans {
		if options.RemoveOrphans {
			w := progress.ContextWriter(ctx)
			err := s.removeContainers(ctx, w, orphans, nil, false)
			if err != nil {
				return err
			}
		} else {
			logrus.Warnf("Found orphan containers (%s) for this project. If "+
				"you removed or renamed this service in your compose "+
				"file, you can run this command with the "+
				"--remove-orphans flag to clean it up.", orphans.names())
		}
	}

	err = prepareServicesDependsOn(project)
	if err != nil {
		return err
	}

	return newConvergence(options.Services, observedState, s).apply(ctx, project, options)
}

func prepareVolumes(p *types.Project) error {
	for i := range p.Services {
		volumesFrom, dependServices, err := getVolumesFrom(p, p.Services[i].VolumesFrom)
		if err != nil {
			return err
		}
		p.Services[i].VolumesFrom = volumesFrom
		if len(dependServices) > 0 {
			if p.Services[i].DependsOn == nil {
				p.Services[i].DependsOn = make(types.DependsOnConfig, len(dependServices))
			}
			for _, service := range p.Services {
				if utils.StringContains(dependServices, service.Name) &&
					p.Services[i].DependsOn[service.Name].Condition == "" {
					p.Services[i].DependsOn[service.Name] = types.ServiceDependency{
						Condition: types.ServiceConditionStarted,
					}
				}
			}
		}
	}
	return nil
}

func prepareNetworks(project *types.Project) {
	for k, network := range project.Networks {
		network.Labels = network.Labels.Add(api.NetworkLabel, k)
		network.Labels = network.Labels.Add(api.ProjectLabel, project.Name)
		network.Labels = network.Labels.Add(api.VersionLabel, api.ComposeVersion)
		project.Networks[k] = network
	}
}

func prepareServicesDependsOn(p *types.Project) error {
	allServices := types.Project{}
	allServices.Services = p.AllServices()

	for i, service := range p.Services {
		var dependencies []string
		networkDependency := getDependentServiceFromMode(service.NetworkMode)
		if networkDependency != "" {
			dependencies = append(dependencies, networkDependency)
		}

		ipcDependency := getDependentServiceFromMode(service.Ipc)
		if ipcDependency != "" {
			dependencies = append(dependencies, ipcDependency)
		}

		pidDependency := getDependentServiceFromMode(service.Pid)
		if pidDependency != "" {
			dependencies = append(dependencies, pidDependency)
		}

		for _, vol := range service.VolumesFrom {
			spec := strings.Split(vol, ":")
			if len(spec) == 0 {
				continue
			}
			if spec[0] == "container" {
				continue
			}
			dependencies = append(dependencies, spec[0])
		}

		for _, link := range service.Links {
			dependencies = append(dependencies, strings.Split(link, ":")[0])
		}

		for d := range service.DependsOn {
			dependencies = append(dependencies, d)
		}

		if len(dependencies) == 0 {
			continue
		}

		// Verify dependencies exist in the project, whether disabled or not
		deps, err := allServices.GetServices(dependencies...)
		if err != nil {
			return err
		}

		if service.DependsOn == nil {
			service.DependsOn = make(types.DependsOnConfig)
		}

		for _, d := range deps {
			if _, ok := service.DependsOn[d.Name]; !ok {
				service.DependsOn[d.Name] = types.ServiceDependency{
					Condition: types.ServiceConditionStarted,
				}
			}
		}
		p.Services[i] = service
	}
	return nil
}

func (s *composeService) ensureNetworks(ctx context.Context, networks types.Networks) error {
	for _, network := range networks {
		err := s.ensureNetwork(ctx, network)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *composeService) ensureProjectVolumes(ctx context.Context, project *types.Project) error {
	for k, volume := range project.Volumes {
		volume.Labels = volume.Labels.Add(api.VolumeLabel, k)
		volume.Labels = volume.Labels.Add(api.ProjectLabel, project.Name)
		volume.Labels = volume.Labels.Add(api.VersionLabel, api.ComposeVersion)
		err := s.ensureVolume(ctx, volume, project.Name)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *composeService) getCreateOptions(ctx context.Context, p *types.Project, service types.ServiceConfig,
	number int, inherit *moby.Container, autoRemove bool, attachStdin bool) (*container.Config, *container.HostConfig, *network.NetworkingConfig, error) {

	labels, err := s.prepareLabels(service, number)
	if err != nil {
		return nil, nil, nil, err
	}

	var (
		runCmd     strslice.StrSlice
		entrypoint strslice.StrSlice
	)
	if service.Command != nil {
		runCmd = strslice.StrSlice(service.Command)
	}
	if service.Entrypoint != nil {
		entrypoint = strslice.StrSlice(service.Entrypoint)
	}

	var (
		tty       = service.Tty
		stdinOpen = service.StdinOpen
	)

	volumeMounts, binds, mounts, err := s.buildContainerVolumes(ctx, *p, service, inherit)
	if err != nil {
		return nil, nil, nil, err
	}

	proxyConfig := types.MappingWithEquals(s.configFile().ParseProxyConfig(s.apiClient().DaemonHost(), nil))
	env := proxyConfig.OverrideBy(service.Environment)

	containerConfig := container.Config{
		Hostname:        service.Hostname,
		Domainname:      service.DomainName,
		User:            service.User,
		ExposedPorts:    buildContainerPorts(service),
		Tty:             tty,
		OpenStdin:       stdinOpen,
		StdinOnce:       attachStdin && stdinOpen,
		AttachStdin:     attachStdin,
		AttachStderr:    true,
		AttachStdout:    true,
		Cmd:             runCmd,
		Image:           api.GetImageNameOrDefault(service, p.Name),
		WorkingDir:      service.WorkingDir,
		Entrypoint:      entrypoint,
		NetworkDisabled: service.NetworkMode == "disabled",
		MacAddress:      service.MacAddress,
		Labels:          labels,
		StopSignal:      service.StopSignal,
		Env:             ToMobyEnv(env),
		Healthcheck:     ToMobyHealthCheck(service.HealthCheck),
		Volumes:         volumeMounts,
		StopTimeout:     ToSeconds(service.StopGracePeriod),
	}

	portBindings := buildContainerPortBindingOptions(service)

	resources := getDeployResources(service)

	if service.NetworkMode == "" {
		service.NetworkMode = getDefaultNetworkMode(p, service)
	}

	var networkConfig *network.NetworkingConfig

	for _, id := range service.NetworksByPriority() {
		net := p.Networks[id]
		config := service.Networks[id]
		var ipam *network.EndpointIPAMConfig
		var (
			ipv4Address string
			ipv6Address string
		)
		if config != nil {
			ipv4Address = config.Ipv4Address
			ipv6Address = config.Ipv6Address
			ipam = &network.EndpointIPAMConfig{
				IPv4Address:  ipv4Address,
				IPv6Address:  ipv6Address,
				LinkLocalIPs: config.LinkLocalIPs,
			}
		}
		networkConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				net.Name: {
					Aliases:     getAliases(service, config),
					IPAddress:   ipv4Address,
					IPv6Gateway: ipv6Address,
					IPAMConfig:  ipam,
				},
			},
		}
		break //nolint:staticcheck
	}

	tmpfs := map[string]string{}
	for _, t := range service.Tmpfs {
		if arr := strings.SplitN(t, ":", 2); len(arr) > 1 {
			tmpfs[arr[0]] = arr[1]
		} else {
			tmpfs[arr[0]] = ""
		}
	}

	var logConfig container.LogConfig
	if service.Logging != nil {
		logConfig = container.LogConfig{
			Type:   service.Logging.Driver,
			Config: service.Logging.Options,
		}
	}

	var volumesFrom []string
	for _, v := range service.VolumesFrom {
		if !strings.HasPrefix(v, "container:") {
			return nil, nil, nil, fmt.Errorf("invalid volume_from: %s", v)
		}
		volumesFrom = append(volumesFrom, v[len("container:"):])
	}

	links, err := s.getLinks(ctx, p.Name, service, number)
	if err != nil {
		return nil, nil, nil, err
	}

	securityOpts, err := parseSecurityOpts(p, service.SecurityOpt)
	if err != nil {
		return nil, nil, nil, err
	}
	hostConfig := container.HostConfig{
		AutoRemove:     autoRemove,
		Binds:          binds,
		Mounts:         mounts,
		CapAdd:         strslice.StrSlice(service.CapAdd),
		CapDrop:        strslice.StrSlice(service.CapDrop),
		NetworkMode:    container.NetworkMode(service.NetworkMode),
		Init:           service.Init,
		IpcMode:        container.IpcMode(service.Ipc),
		ReadonlyRootfs: service.ReadOnly,
		RestartPolicy:  getRestartPolicy(service),
		ShmSize:        int64(service.ShmSize),
		Sysctls:        service.Sysctls,
		PortBindings:   portBindings,
		Resources:      resources,
		VolumeDriver:   service.VolumeDriver,
		VolumesFrom:    volumesFrom,
		DNS:            service.DNS,
		DNSSearch:      service.DNSSearch,
		DNSOptions:     service.DNSOpts,
		ExtraHosts:     service.ExtraHosts.AsList(),
		SecurityOpt:    securityOpts,
		UsernsMode:     container.UsernsMode(service.UserNSMode),
		Privileged:     service.Privileged,
		PidMode:        container.PidMode(service.Pid),
		Tmpfs:          tmpfs,
		Isolation:      container.Isolation(service.Isolation),
		Runtime:        service.Runtime,
		LogConfig:      logConfig,
		GroupAdd:       service.GroupAdd,
		Links:          links,
		OomScoreAdj:    int(service.OomScoreAdj),
	}

	return &containerConfig, &hostConfig, networkConfig, nil
}

// copy/pasted from https://github.com/docker/cli/blob/9de1b162f/cli/command/container/opts.go#L673-L697 + RelativePath
// TODO find so way to share this code with docker/cli
func parseSecurityOpts(p *types.Project, securityOpts []string) ([]string, error) {
	for key, opt := range securityOpts {
		con := strings.SplitN(opt, "=", 2)
		if len(con) == 1 && con[0] != "no-new-privileges" {
			if strings.Contains(opt, ":") {
				con = strings.SplitN(opt, ":", 2)
			} else {
				return securityOpts, errors.Errorf("Invalid security-opt: %q", opt)
			}
		}
		if con[0] == "seccomp" && con[1] != "unconfined" {
			f, err := os.ReadFile(p.RelativePath(con[1]))
			if err != nil {
				return securityOpts, errors.Errorf("opening seccomp profile (%s) failed: %v", con[1], err)
			}
			b := bytes.NewBuffer(nil)
			if err := json.Compact(b, f); err != nil {
				return securityOpts, errors.Errorf("compacting json for seccomp profile (%s) failed: %v", con[1], err)
			}
			securityOpts[key] = fmt.Sprintf("seccomp=%s", b.Bytes())
		}
	}

	return securityOpts, nil
}

func (s *composeService) prepareLabels(service types.ServiceConfig, number int) (map[string]string, error) {
	labels := map[string]string{}
	for k, v := range service.Labels {
		labels[k] = v
	}
	for k, v := range service.CustomLabels {
		labels[k] = v
	}

	hash, err := ServiceHash(service)
	if err != nil {
		return nil, err
	}
	labels[api.ConfigHashLabel] = hash

	labels[api.ContainerNumberLabel] = strconv.Itoa(number)

	var dependencies []string
	for s, d := range service.DependsOn {
		dependencies = append(dependencies, s+":"+d.Condition)
	}
	labels[api.DependenciesLabel] = strings.Join(dependencies, ",")
	return labels, nil
}

func getDefaultNetworkMode(project *types.Project, service types.ServiceConfig) string {
	if len(project.Networks) == 0 {
		return "none"
	}

	if len(service.Networks) > 0 {
		name := service.NetworksByPriority()[0]
		return project.Networks[name].Name
	}

	return project.Networks["default"].Name
}

func getRestartPolicy(service types.ServiceConfig) container.RestartPolicy {
	var restart container.RestartPolicy
	if service.Restart != "" {
		split := strings.Split(service.Restart, ":")
		var attempts int
		if len(split) > 1 {
			attempts, _ = strconv.Atoi(split[1])
		}
		restart = container.RestartPolicy{
			Name:              split[0],
			MaximumRetryCount: attempts,
		}
	}
	if service.Deploy != nil && service.Deploy.RestartPolicy != nil {
		policy := *service.Deploy.RestartPolicy
		var attempts int
		if policy.MaxAttempts != nil {
			attempts = int(*policy.MaxAttempts)
		}
		restart = container.RestartPolicy{
			Name:              mapRestartPolicyCondition(policy.Condition),
			MaximumRetryCount: attempts,
		}
	}
	return restart
}

func mapRestartPolicyCondition(condition string) string {
	// map definitions of deploy.restart_policy to engine definitions
	switch condition {
	case "none", "no":
		return "no"
	case "on-failure", "unless-stopped":
		return condition
	case "any", "always":
		return "always"
	default:
		return condition
	}
}

func getDeployResources(s types.ServiceConfig) container.Resources {
	var swappiness *int64
	if s.MemSwappiness != 0 {
		val := int64(s.MemSwappiness)
		swappiness = &val
	}
	resources := container.Resources{
		CgroupParent:       s.CgroupParent,
		Memory:             int64(s.MemLimit),
		MemorySwap:         int64(s.MemSwapLimit),
		MemorySwappiness:   swappiness,
		MemoryReservation:  int64(s.MemReservation),
		OomKillDisable:     &s.OomKillDisable,
		CPUCount:           s.CPUCount,
		CPUPeriod:          s.CPUPeriod,
		CPUQuota:           s.CPUQuota,
		CPURealtimePeriod:  s.CPURTPeriod,
		CPURealtimeRuntime: s.CPURTRuntime,
		CPUShares:          s.CPUShares,
		CPUPercent:         int64(s.CPUS * 100),
		CpusetCpus:         s.CPUSet,
		DeviceCgroupRules:  s.DeviceCgroupRules,
	}

	if s.PidsLimit != 0 {
		resources.PidsLimit = &s.PidsLimit
	}

	setBlkio(s.BlkioConfig, &resources)

	if s.Deploy != nil {
		setLimits(s.Deploy.Resources.Limits, &resources)
		setReservations(s.Deploy.Resources.Reservations, &resources)
	}

	for _, device := range s.Devices {
		// FIXME should use docker/cli parseDevice, unfortunately private
		src := ""
		dst := ""
		permissions := "rwm"
		arr := strings.Split(device, ":")
		switch len(arr) {
		case 3:
			permissions = arr[2]
			fallthrough
		case 2:
			dst = arr[1]
			fallthrough
		case 1:
			src = arr[0]
		}
		if dst == "" {
			dst = src
		}
		resources.Devices = append(resources.Devices, container.DeviceMapping{
			PathOnHost:        src,
			PathInContainer:   dst,
			CgroupPermissions: permissions,
		})
	}

	for name, u := range s.Ulimits {
		soft := u.Single
		if u.Soft != 0 {
			soft = u.Soft
		}
		hard := u.Single
		if u.Hard != 0 {
			hard = u.Hard
		}
		resources.Ulimits = append(resources.Ulimits, &units.Ulimit{
			Name: name,
			Hard: int64(hard),
			Soft: int64(soft),
		})
	}
	return resources
}

func setReservations(reservations *types.Resource, resources *container.Resources) {
	if reservations == nil {
		return
	}
	// Cpu reservation is a swarm option and PIDs is only a limit
	// So we only need to map memory reservation and devices
	if reservations.MemoryBytes != 0 {
		resources.MemoryReservation = int64(reservations.MemoryBytes)
	}

	for _, device := range reservations.Devices {
		resources.DeviceRequests = append(resources.DeviceRequests, container.DeviceRequest{
			Capabilities: [][]string{device.Capabilities},
			Count:        int(device.Count),
			DeviceIDs:    device.IDs,
			Driver:       device.Driver,
		})
	}
}

func setLimits(limits *types.Resource, resources *container.Resources) {
	if limits == nil {
		return
	}
	if limits.MemoryBytes != 0 {
		resources.Memory = int64(limits.MemoryBytes)
	}
	if limits.NanoCPUs != "" {
		if f, err := strconv.ParseFloat(limits.NanoCPUs, 64); err == nil {
			resources.NanoCPUs = int64(f * 1e9)
		}
	}
	if limits.PIds > 0 {
		resources.PidsLimit = &limits.PIds
	}
}

func setBlkio(blkio *types.BlkioConfig, resources *container.Resources) {
	if blkio == nil {
		return
	}
	resources.BlkioWeight = blkio.Weight
	for _, b := range blkio.WeightDevice {
		resources.BlkioWeightDevice = append(resources.BlkioWeightDevice, &blkiodev.WeightDevice{
			Path:   b.Path,
			Weight: b.Weight,
		})
	}
	for _, b := range blkio.DeviceReadBps {
		resources.BlkioDeviceReadBps = append(resources.BlkioDeviceReadBps, &blkiodev.ThrottleDevice{
			Path: b.Path,
			Rate: b.Rate,
		})
	}
	for _, b := range blkio.DeviceReadIOps {
		resources.BlkioDeviceReadIOps = append(resources.BlkioDeviceReadIOps, &blkiodev.ThrottleDevice{
			Path: b.Path,
			Rate: b.Rate,
		})
	}
	for _, b := range blkio.DeviceWriteBps {
		resources.BlkioDeviceWriteBps = append(resources.BlkioDeviceWriteBps, &blkiodev.ThrottleDevice{
			Path: b.Path,
			Rate: b.Rate,
		})
	}
	for _, b := range blkio.DeviceWriteIOps {
		resources.BlkioDeviceWriteIOps = append(resources.BlkioDeviceWriteIOps, &blkiodev.ThrottleDevice{
			Path: b.Path,
			Rate: b.Rate,
		})
	}
}

func buildContainerPorts(s types.ServiceConfig) nat.PortSet {
	ports := nat.PortSet{}
	for _, s := range s.Expose {
		p := nat.Port(s)
		ports[p] = struct{}{}
	}
	for _, p := range s.Ports {
		p := nat.Port(fmt.Sprintf("%d/%s", p.Target, p.Protocol))
		ports[p] = struct{}{}
	}
	return ports
}

func buildContainerPortBindingOptions(s types.ServiceConfig) nat.PortMap {
	bindings := nat.PortMap{}
	for _, port := range s.Ports {
		p := nat.Port(fmt.Sprintf("%d/%s", port.Target, port.Protocol))
		binding := nat.PortBinding{
			HostIP:   port.HostIP,
			HostPort: port.Published,
		}
		bindings[p] = append(bindings[p], binding)
	}
	return bindings
}

func getVolumesFrom(project *types.Project, volumesFrom []string) ([]string, []string, error) {
	var volumes = []string{}
	var services = []string{}
	// parse volumes_from
	if len(volumesFrom) == 0 {
		return volumes, services, nil
	}
	for _, vol := range volumesFrom {
		spec := strings.Split(vol, ":")
		if len(spec) == 0 {
			continue
		}
		if spec[0] == "container" {
			volumes = append(volumes, vol)
			continue
		}
		serviceName := spec[0]
		services = append(services, serviceName)
		service, err := project.GetService(serviceName)
		if err != nil {
			return nil, nil, err
		}

		firstContainer := getContainerName(project.Name, service, 1)
		v := fmt.Sprintf("container:%s", firstContainer)
		if len(spec) > 2 {
			v = fmt.Sprintf("container:%s:%s", firstContainer, strings.Join(spec[1:], ":"))
		}
		volumes = append(volumes, v)
	}
	return volumes, services, nil

}

func getDependentServiceFromMode(mode string) string {
	if strings.HasPrefix(mode, types.NetworkModeServicePrefix) {
		return mode[len(types.NetworkModeServicePrefix):]
	}
	return ""
}

func (s *composeService) buildContainerVolumes(ctx context.Context, p types.Project, service types.ServiceConfig,
	inherit *moby.Container) (map[string]struct{}, []string, []mount.Mount, error) {
	var mounts = []mount.Mount{}

	image := api.GetImageNameOrDefault(service, p.Name)
	imgInspect, _, err := s.apiClient().ImageInspectWithRaw(ctx, image)
	if err != nil {
		return nil, nil, nil, err
	}

	mountOptions, err := buildContainerMountOptions(p, service, imgInspect, inherit)
	if err != nil {
		return nil, nil, nil, err
	}

	volumeMounts := map[string]struct{}{}
	binds := []string{}
MOUNTS:
	for _, m := range mountOptions {
		if m.Type == mount.TypeNamedPipe {
			mounts = append(mounts, m)
			continue
		}
		volumeMounts[m.Target] = struct{}{}
		if m.Type == mount.TypeBind {
			// `Mount` is preferred but does not offer option to created host path if missing
			// so `Bind` API is used here with raw volume string
			// see https://github.com/moby/moby/issues/43483
			for _, v := range service.Volumes {
				if v.Target == m.Target {
					switch {
					case string(m.Type) != v.Type:
						v.Source = m.Source
						fallthrough
					case v.Bind != nil && v.Bind.CreateHostPath:
						binds = append(binds, v.String())
						continue MOUNTS
					}
				}
			}
		}
		mounts = append(mounts, m)
	}
	return volumeMounts, binds, mounts, nil
}

func buildContainerMountOptions(p types.Project, s types.ServiceConfig, img moby.ImageInspect, inherit *moby.Container) ([]mount.Mount, error) {
	var mounts = map[string]mount.Mount{}
	if inherit != nil {
		for _, m := range inherit.Mounts {
			if m.Type == "tmpfs" {
				continue
			}
			src := m.Source
			if m.Type == "volume" {
				src = m.Name
			}
			m.Destination = path.Clean(m.Destination)

			if img.Config != nil {
				if _, ok := img.Config.Volumes[m.Destination]; ok {
					// inherit previous container's anonymous volume
					mounts[m.Destination] = mount.Mount{
						Type:     m.Type,
						Source:   src,
						Target:   m.Destination,
						ReadOnly: !m.RW,
					}
				}
			}
			volumes := []types.ServiceVolumeConfig{}
			for _, v := range s.Volumes {
				if v.Target != m.Destination || v.Source != "" {
					volumes = append(volumes, v)
					continue
				}
				// inherit previous container's anonymous volume
				mounts[m.Destination] = mount.Mount{
					Type:     m.Type,
					Source:   src,
					Target:   m.Destination,
					ReadOnly: !m.RW,
				}
			}
			s.Volumes = volumes
		}
	}

	mounts, err := fillBindMounts(p, s, mounts)
	if err != nil {
		return nil, err
	}

	values := make([]mount.Mount, 0, len(mounts))
	for _, v := range mounts {
		values = append(values, v)
	}
	return values, nil
}

func fillBindMounts(p types.Project, s types.ServiceConfig, m map[string]mount.Mount) (map[string]mount.Mount, error) {
	for _, v := range s.Volumes {
		bindMount, err := buildMount(p, v)
		if err != nil {
			return nil, err
		}
		m[bindMount.Target] = bindMount
	}

	secrets, err := buildContainerSecretMounts(p, s)
	if err != nil {
		return nil, err
	}
	for _, s := range secrets {
		if _, found := m[s.Target]; found {
			continue
		}
		m[s.Target] = s
	}

	configs, err := buildContainerConfigMounts(p, s)
	if err != nil {
		return nil, err
	}
	for _, c := range configs {
		if _, found := m[c.Target]; found {
			continue
		}
		m[c.Target] = c
	}
	return m, nil
}

func buildContainerConfigMounts(p types.Project, s types.ServiceConfig) ([]mount.Mount, error) {
	var mounts = map[string]mount.Mount{}

	configsBaseDir := "/"
	for _, config := range s.Configs {
		target := config.Target
		if config.Target == "" {
			target = configsBaseDir + config.Source
		} else if !isUnixAbs(config.Target) {
			target = configsBaseDir + config.Target
		}

		definedConfig := p.Configs[config.Source]
		if definedConfig.External.External {
			return nil, fmt.Errorf("unsupported external config %s", definedConfig.Name)
		}

		bindMount, err := buildMount(p, types.ServiceVolumeConfig{
			Type:     types.VolumeTypeBind,
			Source:   definedConfig.File,
			Target:   target,
			ReadOnly: true,
		})
		if err != nil {
			return nil, err
		}
		mounts[target] = bindMount
	}
	values := make([]mount.Mount, 0, len(mounts))
	for _, v := range mounts {
		values = append(values, v)
	}
	return values, nil
}

func buildContainerSecretMounts(p types.Project, s types.ServiceConfig) ([]mount.Mount, error) {
	var mounts = map[string]mount.Mount{}

	secretsDir := "/run/secrets/"
	for _, secret := range s.Secrets {
		target := secret.Target
		if secret.Target == "" {
			target = secretsDir + secret.Source
		} else if !isUnixAbs(secret.Target) {
			target = secretsDir + secret.Target
		}

		definedSecret := p.Secrets[secret.Source]
		if definedSecret.External.External {
			return nil, fmt.Errorf("unsupported external secret %s", definedSecret.Name)
		}

		if definedSecret.Environment != "" {
			continue
		}

		mnt, err := buildMount(p, types.ServiceVolumeConfig{
			Type:     types.VolumeTypeBind,
			Source:   definedSecret.File,
			Target:   target,
			ReadOnly: true,
		})
		if err != nil {
			return nil, err
		}
		mounts[target] = mnt
	}
	values := make([]mount.Mount, 0, len(mounts))
	for _, v := range mounts {
		values = append(values, v)
	}
	return values, nil
}

func isUnixAbs(p string) bool {
	return strings.HasPrefix(p, "/")
}

func buildMount(project types.Project, volume types.ServiceVolumeConfig) (mount.Mount, error) {
	source := volume.Source
	// on windows, filepath.IsAbs(source) is false for unix style abs path like /var/run/docker.sock.
	// do not replace these with  filepath.Abs(source) that will include a default drive.
	if volume.Type == types.VolumeTypeBind && !filepath.IsAbs(source) && !strings.HasPrefix(source, "/") {
		// volume source has already been prefixed with workdir if required, by compose-go project loader
		var err error
		source, err = filepath.Abs(source)
		if err != nil {
			return mount.Mount{}, err
		}
	}
	if volume.Type == types.VolumeTypeVolume {
		if volume.Source != "" {
			pVolume, ok := project.Volumes[volume.Source]
			if ok {
				source = pVolume.Name
			}
		}
	}

	bind, vol, tmpfs := buildMountOptions(project, volume)

	volume.Target = path.Clean(volume.Target)

	if bind != nil {
		volume.Type = types.VolumeTypeBind
	}

	return mount.Mount{
		Type:          mount.Type(volume.Type),
		Source:        source,
		Target:        volume.Target,
		ReadOnly:      volume.ReadOnly,
		Consistency:   mount.Consistency(volume.Consistency),
		BindOptions:   bind,
		VolumeOptions: vol,
		TmpfsOptions:  tmpfs,
	}, nil
}

func buildMountOptions(project types.Project, volume types.ServiceVolumeConfig) (*mount.BindOptions, *mount.VolumeOptions, *mount.TmpfsOptions) {
	switch volume.Type {
	case "bind":
		if volume.Volume != nil {
			logrus.Warnf("mount of type `bind` should not define `volume` option")
		}
		if volume.Tmpfs != nil {
			logrus.Warnf("mount of type `tmpfs` should not define `tmpfs` option")
		}
		return buildBindOption(volume.Bind), nil, nil
	case "volume":
		if volume.Bind != nil {
			logrus.Warnf("mount of type `volume` should not define `bind` option")
		}
		if volume.Tmpfs != nil {
			logrus.Warnf("mount of type `volume` should not define `tmpfs` option")
		}
		if v, ok := project.Volumes[volume.Source]; ok && v.DriverOpts["o"] == types.VolumeTypeBind {
			return buildBindOption(&types.ServiceVolumeBind{
				CreateHostPath: true,
			}), nil, nil
		}
		return nil, buildVolumeOptions(volume.Volume), nil
	case "tmpfs":
		if volume.Bind != nil {
			logrus.Warnf("mount of type `tmpfs` should not define `bind` option")
		}
		if volume.Volume != nil {
			logrus.Warnf("mount of type `tmpfs` should not define `volume` option")
		}
		return nil, nil, buildTmpfsOptions(volume.Tmpfs)
	}
	return nil, nil, nil
}

func buildBindOption(bind *types.ServiceVolumeBind) *mount.BindOptions {
	if bind == nil {
		return nil
	}
	return &mount.BindOptions{
		Propagation: mount.Propagation(bind.Propagation),
		// NonRecursive: false, FIXME missing from model ?
	}
}

func buildVolumeOptions(vol *types.ServiceVolumeVolume) *mount.VolumeOptions {
	if vol == nil {
		return nil
	}
	return &mount.VolumeOptions{
		NoCopy: vol.NoCopy,
		// Labels:       , // FIXME missing from model ?
		// DriverConfig: , // FIXME missing from model ?
	}
}

func buildTmpfsOptions(tmpfs *types.ServiceVolumeTmpfs) *mount.TmpfsOptions {
	if tmpfs == nil {
		return nil
	}
	return &mount.TmpfsOptions{
		SizeBytes: int64(tmpfs.Size),
		// Mode:      , // FIXME missing from model ?
	}
}

func getAliases(s types.ServiceConfig, c *types.ServiceNetworkConfig) []string {
	aliases := []string{s.Name}
	if c != nil {
		aliases = append(aliases, c.Aliases...)
	}
	return aliases
}

func (s *composeService) ensureNetwork(ctx context.Context, n types.NetworkConfig) error {
	// NetworkInspect will match on ID prefix, so NetworkList with a name
	// filter is used to look for an exact match to prevent e.g. a network
	// named `db` from getting erroneously matched to a network with an ID
	// like `db9086999caf`
	networks, err := s.apiClient().NetworkList(ctx, moby.NetworkListOptions{
		Filters: filters.NewArgs(filters.Arg("name", n.Name)),
	})
	if err != nil {
		return err
	}
	networkNotFound := true
	for _, net := range networks {
		if net.Name == n.Name {
			networkNotFound = false
			break
		}
	}
	if networkNotFound {
		if n.External.External {
			if n.Driver == "overlay" {
				// Swarm nodes do not register overlay networks that were
				// created on a different node unless they're in use.
				// Here we assume `driver` is relevant for a network we don't manage
				// which is a non-sense, but this is our legacy ¯\(ツ)/¯
				// networkAttach will later fail anyway if network actually doesn't exists
				return nil
			}
			return fmt.Errorf("network %s declared as external, but could not be found", n.Name)
		}
		var ipam *network.IPAM
		if n.Ipam.Config != nil {
			var config []network.IPAMConfig
			for _, pool := range n.Ipam.Config {
				config = append(config, network.IPAMConfig{
					Subnet:     pool.Subnet,
					IPRange:    pool.IPRange,
					Gateway:    pool.Gateway,
					AuxAddress: pool.AuxiliaryAddresses,
				})
			}
			ipam = &network.IPAM{
				Driver: n.Ipam.Driver,
				Config: config,
			}
		}
		createOpts := moby.NetworkCreate{
			CheckDuplicate: true,
			// TODO NameSpace Labels
			Labels:     n.Labels,
			Driver:     n.Driver,
			Options:    n.DriverOpts,
			Internal:   n.Internal,
			Attachable: n.Attachable,
			IPAM:       ipam,
			EnableIPv6: n.EnableIPv6,
		}

		if n.Ipam.Driver != "" || len(n.Ipam.Config) > 0 {
			createOpts.IPAM = &network.IPAM{}
		}

		if n.Ipam.Driver != "" {
			createOpts.IPAM.Driver = n.Ipam.Driver
		}

		for _, ipamConfig := range n.Ipam.Config {
			config := network.IPAMConfig{
				Subnet:     ipamConfig.Subnet,
				IPRange:    ipamConfig.IPRange,
				Gateway:    ipamConfig.Gateway,
				AuxAddress: ipamConfig.AuxiliaryAddresses,
			}
			createOpts.IPAM.Config = append(createOpts.IPAM.Config, config)
		}
		networkEventName := fmt.Sprintf("Network %s", n.Name)
		w := progress.ContextWriter(ctx)
		w.Event(progress.CreatingEvent(networkEventName))
		if _, err := s.apiClient().NetworkCreate(ctx, n.Name, createOpts); err != nil {
			w.Event(progress.ErrorEvent(networkEventName))
			return errors.Wrapf(err, "failed to create network %s", n.Name)
		}
		w.Event(progress.CreatedEvent(networkEventName))
		return nil
	}
	return nil
}

func (s *composeService) ensureVolume(ctx context.Context, volume types.VolumeConfig, project string) error {
	inspected, err := s.apiClient().VolumeInspect(ctx, volume.Name)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return err
		}
		if volume.External.External {
			return fmt.Errorf("external volume %q not found", volume.Name)
		}
		err := s.createVolume(ctx, volume)
		return err
	}

	if volume.External.External {
		return nil
	}

	// Volume exists with name, but let's double-check this is the expected one
	p, ok := inspected.Labels[api.ProjectLabel]
	if !ok {
		logrus.Warnf("volume %q already exists but was not created by Docker Compose. Use `external: true` to use an existing volume", volume.Name)
	}
	if ok && p != project {
		logrus.Warnf("volume %q already exists but was not created for project %q. Use `external: true` to use an existing volume", volume.Name, p)
	}
	return nil
}

func (s *composeService) createVolume(ctx context.Context, volume types.VolumeConfig) error {
	eventName := fmt.Sprintf("Volume %q", volume.Name)
	w := progress.ContextWriter(ctx)
	w.Event(progress.CreatingEvent(eventName))
	_, err := s.apiClient().VolumeCreate(ctx, volume_api.CreateOptions{
		Labels:     volume.Labels,
		Name:       volume.Name,
		Driver:     volume.Driver,
		DriverOpts: volume.DriverOpts,
	})
	if err != nil {
		w.Event(progress.ErrorEvent(eventName))
		return err
	}
	w.Event(progress.CreatedEvent(eventName))
	return nil
}
