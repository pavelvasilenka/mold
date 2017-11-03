package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"runtime"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

// default timeout when trying to stop a container.
const defaultStopTimeout int = 5

var errAborted = fmt.Errorf("aborted")

// DockerWorker performs a docker based build per the config
type DockerWorker struct {
	mu sync.Mutex

	buildConfig   *MoldConfig     // overall moldconfig
	serviceStates containerStates // service containers
	buildStates   containerStates // build containers
	netID         string          // network id to connect all containers to

	docker *Docker // docker helper client

	done    chan bool // when all builds are completed
	abort   chan bool // cancelled channel
	aborted bool      // whether the worker has begun shutdown

	log io.Writer
	// Auth config for registry operations
	authCfg *DockerAuthConfig
}

// NewDockerWorker instantiates a new worker. If no client is provided and env.
// based client is used.
func NewDockerWorker(dcli *Docker) (d *DockerWorker, err error) {
	d = &DockerWorker{docker: dcli, log: os.Stdout, abort: make(chan bool, 1)}
	// set up registry auth. pushes will not happen if failed
	if d.authCfg, err = readDockerAuthConfig(""); err != nil {
		log.Println("WRN", err)
		err = nil
	}

	if d.docker == nil {
		d.docker, err = NewDocker("")
	}
	return
}

// Configure the job. This converts the MoldConfig to the docker required datastructure normalizing
// values as needed.
func (dw *DockerWorker) Configure(cfg *MoldConfig) error {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	dw.buildConfig = cfg

	// Build service container contfigs
	sc, err := assembleServiceContainers(cfg)
	if err != nil {
		return err
	}

	dw.serviceStates = make([]*containerState, len(sc))
	for i, s := range sc {
		// Initialize state
		cs := &containerState{ContainerConfig: s, Type: ServiceContainerType}
		cs.Name = nameFromImageName(s.Container.Image) + "." + cfg.RepoName
		// Attach network
		cs.Network = dw.defaultNetConfig()
		dw.serviceStates[i] = cs
	}

	// Build build container configs
	bc, err := assembleBuildContainers(cfg)
	if err != nil {
		return fmt.Errorf("Could not assemble build container: %v", err)
	}
	dw.buildStates = make([]*containerState, len(bc))
	for i, s := range bc {
		cs := &containerState{
			ContainerConfig: s,
			Type:            BuildContainerType,
			save:            dw.buildConfig.Build[i].Save,
		}
		cs.Name = fmt.Sprintf("%s-%d-%d", dw.buildConfig.Name(), i, time.Now().UnixNano())
		cs.Network = dw.defaultNetConfig()

		if dw.buildConfig.Build[i].Cache {
			hash, err := getBuildHash(cs.ContainerConfig)
			if err != nil {
				return err
			}
			cs.cache = &cache{
				Name: fmt.Sprintf("cache-%s", dw.buildConfig.RepoName),
				Tag:  hash,
			}
		}
		dw.buildStates[i] = cs
	}

	return nil
}

func assembleServiceContainers(mc *MoldConfig) ([]*ContainerConfig, error) {
	bcs := make([]*ContainerConfig, len(mc.Services))
	for i, b := range mc.Services {
		cc := DefaultContainerConfig(b.Image)
		cc.Container.Cmd = b.Commands

		env, err := appendOsEnv(b.Environment)
		if err != nil {
			return nil, err
		}

		cc.Container.Env = env
		bcs[i] = cc
	}
	return bcs, nil
}

func appendOsEnv(inputEnvironment []string) ([]string, error) {
	output := make([]string, len(inputEnvironment))

	for i, env := range inputEnvironment {
		if !strings.Contains(env, "=") && strings.TrimSpace(env) != "" {
			envValue := os.Getenv(env)
			if strings.TrimSpace(envValue) == "" {
				return nil, fmt.Errorf("Wanted environment value %v but not found", env)
			}

			newEnv := fmt.Sprintf("%v=%v", env, envValue)
			output[i] = newEnv
		} else {
			output[i] = env
		}
	}

	return output, nil
}

// assembleBuildContainers assembles container configs from user provided build config
func assembleBuildContainers(mc *MoldConfig) ([]*ContainerConfig, error) {
	bconts := make([]*ContainerConfig, len(mc.Build))
	for i, b := range mc.Build {
		cc := DefaultContainerConfig(b.Image)
		cc.Container.WorkingDir = b.Workdir

		exposedPorts, portBindings, err := nat.ParsePortSpecs(b.Ports)
		if err != nil {
			return nil, err
		}
		cc.Container.ExposedPorts = exposedPorts
		cc.Host.PortBindings = portBindings

		cc.Container.Volumes = map[string]struct{}{b.Workdir: struct{}{}}
		cc.Container.Cmd = []string{b.Shell, "-cex", b.BuildCmds()}

		env, err := appendOsEnv(b.Environment)
		if err != nil {
			return nil, err
		}
		cc.Container.Env = env

		src := mc.Context
		if runtime.GOOS == "windows" {
			src = toDockerWinPath(src)
		}
		cc.Host.Mounts = []mount.Mount{
			mount.Mount{Target: b.Workdir, Source: src, Type: mount.TypeBind},
		}
		bconts[i] = cc

		// Mount docker.sock in container if requested.
		if mc.AllowDockerAccess {
			bconts[i].Container.Volumes[dockerSockFile] = struct{}{}
			bconts[i].Host.Mounts = append(bconts[i].Host.Mounts,
				mount.Mount{Target: dockerSockFile, Source: dockerSockFile, Type: mount.TypeBind})
		}
	}
	return bconts, nil
}

func (dw *DockerWorker) defaultNetConfig() *network.NetworkingConfig {
	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			dw.buildConfig.Name(): &network.EndpointSettings{
				NetworkID: dw.netID,
			},
		},
	}
}

// GenerateArtifacts builds docker images
func (dw *DockerWorker) GenerateArtifacts(names ...string) error {
	var ics []ImageConfig
	if len(names) == 0 {
		ics = dw.buildConfig.Artifacts.Images
	} else {
		for _, name := range names {
			a := dw.buildConfig.Artifacts.GetImage(name)
			if a == nil {
				return fmt.Errorf("no such artifact: %s", name)
			}
			ics = append(ics, *a)
		}
	}
	var err error
	for _, ic := range ics {
		if dw.aborted {
			return errAborted
		}
		dw.log.Write([]byte(fmt.Sprintf("[artifacts/%s] Building\n", ic.Name)))
		err = mergeErrors(err, dw.generateArtifact(&ic))
		dw.log.Write([]byte(fmt.Sprintf("[artifacts/%s] DONE\n", ic.Name)))
	}
	return err
}

func (dw *DockerWorker) generateArtifact(ic *ImageConfig) error {
	dw.done = make(chan bool)
	err := dw.docker.BuildImageAsync(ic, dw.log, fmt.Sprintf("[artifacts/%s]", ic.Name), dw.done)
	if err != nil {
		return err
	}
	select {
	case ok := <-dw.done:
		if ok {
			dw.log.Write([]byte("[artifacts] Completing...\n"))
		} else {
			dw.log.Write([]byte("[artifacts] Completing with error(s)...\n"))
		}
	case <-dw.abort:
		dw.RemoveArtifacts()
		dw.log.Write([]byte("[artifacts] Aborting...\n"))
	}
	return err
}

// RemoveArtifacts removes all local artifacts it as definted in the config
func (dw *DockerWorker) RemoveArtifacts() error {
	var err error
	for _, a := range dw.buildConfig.Artifacts.Images {
		err = mergeErrors(err, dw.docker.RemoveImage(a.Name, true))
	}
	return err
}

func (dw *DockerWorker) getRegistryAuth(registry string) *types.AuthConfig {
	var auth *types.AuthConfig

	if registry == "" {
		auth = dw.authCfg.DockerHubAuth()
	} else {
		for rh, av := range dw.authCfg.Auths {
			if strings.HasSuffix(rh, registry) {
				auth = &av
				if auth.ServerAddress == "" {
					auth.ServerAddress = rh
				}
				break
			}
		}
	}

	if auth != nil && auth.Auth != "" && auth.Username == "" {
		if s, err := base64.StdEncoding.DecodeString(auth.Auth); err == nil {
			a := strings.Split(string(s), ":")
			if len(a) == 2 {
				auth.Username = a[0]
				auth.Password = a[1]
			}
		}
	}

	return auth
}

// Publish the artifact/s based on the config
func (dw *DockerWorker) Publish(names ...string) error {
	if dw.authCfg == nil || len(dw.authCfg.Auths) == 0 {
		//dw.log.Write([]byte("[publish] Not publishing.  registry auth not specified\n"))
		return fmt.Errorf("registry auth not specified")
	}

	if len(names) == 0 {
		for _, v := range dw.buildConfig.Artifacts.Images {
			if dw.aborted {
				return errAborted
			}
			auth := dw.getRegistryAuth(v.Registry)

			regPaths := v.RegistryPaths()
			for _, rp := range regPaths {

				if err := dw.docker.PushImage(rp, auth, os.Stdout, fmt.Sprintf("[publish/%s]", rp)); err != nil {
					return err
				}

			}
		}
	} else {
		for _, name := range names {
			if dw.aborted {
				return errAborted
			}

			a := dw.buildConfig.Artifacts.GetImage(name)
			if a == nil {
				return fmt.Errorf("no such artifact: %s", name)
			}

			auth := dw.getRegistryAuth(a.Registry)

			regPaths := a.RegistryPaths()
			for _, rp := range regPaths {

				if err := dw.docker.PushImage(rp, auth, os.Stdout, fmt.Sprintf("[publish/%s]", rp)); err != nil {
					return err
				}

			}
		}
	}
	return nil
}

// Build starts the build.  This is a blocking call. index defines one or more
// build steps to run.  They are in the order as seen in teh config. If no index
// is provided all builds are run
func (dw *DockerWorker) Build() error {
	if len(dw.buildStates) == 0 {
		return nil
	}

	done, err := dw.StartBuildAsync(true)
	if err != nil {
		return err
	}

	select {
	case <-done:
		for _, b := range dw.buildStates {
			if b.status != "success" {
				err = mergeErrors(err, fmt.Errorf("build failed: %s %s", b.Name, b.Container.Image))
			} else {
				if e := dw.cacheImage(*b); e != nil {
					err = mergeErrors(err, fmt.Errorf("cache failed:: %s", e))
				}
			}
		}

	case <-dw.abort:
		dw.log.Write([]byte("[build] Aborting...\n"))
		if e := dw.stopBuildContainer(); e != nil {
			dw.log.Write([]byte("ERR Stopping build containers:" + e.Error() + "\n"))
		}
	}

	return err
}

func (dw *DockerWorker) stopBuildContainer() error {
	var err error
	for _, bc := range dw.buildStates {
		dw.log.Write([]byte("[build] Stopping container: " + bc.ID() + "\n"))
		err = mergeErrors(err, dw.docker.StopContainer(bc.ID(), time.Duration(defaultStopTimeout)*time.Second))
	}
	return err
}

// Abort cancels a running build
func (dw *DockerWorker) Abort() error {
	dw.mu.Lock()
	dw.aborted = true
	dw.mu.Unlock()

	dw.abort <- true
	return nil
}

// Setup sets up services needed to perform the build.  These are additional containers
// that are spun up.  If any error occurs the whole build will bail out
func (dw *DockerWorker) Setup() error {
	var err error
	if dw.netID, err = dw.docker.CreateNetwork(dw.buildConfig.Name()); err != nil {
		return err
		// network exists - so move on.
	}
	dw.log.Write([]byte(fmt.Sprintf("[configure/network/%s] Created %s\n", dw.buildConfig.Name(), dw.netID)))

	// Start service containers
	for i, cs := range dw.serviceStates {
		if err := dw.docker.StartContainer(dw.serviceStates[i].ContainerConfig, dw.log, fmt.Sprintf("[setup/service/%s]", cs.Name)); err != nil {
			return err
		}
		dw.log.Write([]byte(fmt.Sprintf("[setup/service/%s] Started %s\n", cs.Name, cs.Container.Image)))
	}
	return nil
}

// StartBuildAsync starts the build container/s
func (dw *DockerWorker) StartBuildAsync(tailLog bool) (chan bool, error) {

	dw.done = make(chan bool)

	go dw.watchBuild()

	for _, cs := range dw.buildStates {
		if cs.cache.IsSet() {
			cacheImgName := cs.cache.ToString()
			if dw.docker.ImageAvailableLocally(cacheImgName) {
				cs.ContainerConfig.Container.Image = cacheImgName
			}
		}

		err := dw.docker.StartContainer(cs.ContainerConfig, dw.log, "")
		if err == nil {
			os.Stdout.Write([]byte(fmt.Sprintf("[build/%s] Started %s\n", cs.Name, cs.Container.Image)))
			if cs.Type == BuildContainerType && tailLog {
				go func(csID, prefix string) {
					// wait otherwise docker may return a 404
					<-time.After(1000 * time.Millisecond)
					if e := dw.docker.TailLogs(csID, dw.log, prefix); e != nil {
						log.Println("ERR Failed to tail log", e)
					}
				}(cs.ID(), fmt.Sprintf("[build/%s]", cs.Name))
			}
			continue
		}
		return dw.done, err
	}
	return dw.done, nil
}

// cacheImage pushes the build image a registry
func (dw *DockerWorker) cacheImage(cs containerState) error {
	if cs.cache.IsSet() {
		img := cs.cache.ToString()
		if err := dw.docker.BuildImageOfContainer(cs.ID(), img); err != nil {
			return err
		}
	}
	return nil
}

// Teardown stops and removes all services spun up before the build as part of cleanup
func (dw *DockerWorker) Teardown() error {
	var err error
	// remove service containers
	for _, cs := range dw.serviceStates {
		e := dw.docker.RemoveContainer(cs.ID(), true)
		err = mergeErrors(err, e)
	}
	// remove build containers.
	for _, cs := range dw.buildStates {
		if !cs.save {
			e := dw.docker.RemoveContainer(cs.ID(), true)
			err = mergeErrors(err, e)
		}
	}

	err = mergeErrors(err, dw.docker.RemoveNetwork(dw.netID))
	return err
}

// TODO: add locking???
// markContainerDone marks the container as done.  Return if all the build containers have completed
func (dw *DockerWorker) markContainerDone(id, status string, state *types.ContainerState) bool {
	for i, v := range dw.buildStates {
		if v.ID() == id {
			dw.mu.Lock()
			dw.buildStates[i].done = true

			if len(status) > 0 {
				dw.buildStates[i].status = status
			}
			if state != nil {
				dw.buildStates[i].state = state
			} else {
				dw.buildStates[i].state = &types.ContainerState{Running: false}
			}
			dw.mu.Unlock()
			dw.log.Write([]byte(fmt.Sprintf("[build/%s] DONE\n", v.Name)))
		}
	}
	// check if all builds are done
	for _, v := range dw.buildStates {
		if !v.done {
			return false
		}
	}
	dw.done <- true
	return true
}

func (dw *DockerWorker) watchBuild() {
	cli := dw.docker.Client()
	msgCh, errCh := cli.Events(context.Background(), types.EventsOptions{})
	for {
		select {
		case msg := <-msgCh:

			switch msg.Action {
			case "destroy":
				// Check if we are interested in this container
				if c := dw.buildStates.Get(msg.Actor.ID); c != nil {
					// Breakout if the whole build is done.  This does not update the status
					// and is there more so the build doesn't block forever in case of failures
					if dw.markContainerDone(msg.Actor.ID, "", nil) {
						return
					}
				}

			case "die", "kill", "stop":
				// Check if we are interested in this container
				if c := dw.buildStates.Get(msg.Actor.ID); c != nil {
					var (
						status string
						state  types.ContainerState
					)
					if cj, err := cli.ContainerInspect(context.Background(), msg.Actor.ID); err == nil {
						if cj.State.ExitCode != 0 {
							status = "failed"
						} else {
							status = "success"
						}
						state = *cj.State
					} else {
						status = msg.Action
					}
					// breakout if the whole build is done
					if dw.markContainerDone(msg.Actor.ID, status, &state) {
						return
					}
				}
			}

		case err := <-errCh:
			log.Println("ERR", err)

		}
	}
}
