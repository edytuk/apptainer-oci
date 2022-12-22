// Copyright (c) Contributors to the Apptainer project, established as
//   Apptainer a Series of LF Projects LLC.
//   For website terms of use, trademark policy, privacy policy and other
//   project policies see https://lfprojects.org/policies
// Copyright (c) 2022, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package oci

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/apptainer/apptainer/internal/pkg/fakeroot"
	"github.com/apptainer/apptainer/internal/pkg/runtime/engine/config/oci/generate"
	"github.com/apptainer/apptainer/internal/pkg/util/env"
	"github.com/apptainer/apptainer/internal/pkg/util/shell/interpreter"
	"github.com/apptainer/apptainer/internal/pkg/util/user"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/term"
)

const apptainerLibs = "/.singularity.d/libs"

func (l *Launcher) getProcess(ctx context.Context, imgSpec imgspecv1.Image, image, bundle, process string, args []string) (*specs.Process, error) {
	// Assemble the runtime & user-requested environment, which will be merged
	// with the image ENV and set in the container at runtime.
	rtEnv := defaultEnv(image, bundle)
	// APPTAINERENV_ has lowest priority
	rtEnv = mergeMap(rtEnv, apptainerEnvMap())
	// --env-file can override APPTAINERENV_
	if l.cfg.EnvFile != "" {
		e, err := envFileMap(ctx, l.cfg.EnvFile)
		if err != nil {
			return nil, err
		}
		rtEnv = mergeMap(rtEnv, e)
	}
	// --env flag can override --env-file and APPTAINERENV_
	rtEnv = mergeMap(rtEnv, l.cfg.Env)

	cwd, err := l.getProcessCwd()
	if err != nil {
		return nil, err
	}

	p := specs.Process{
		Args:     getProcessArgs(imgSpec, process, args),
		Cwd:      cwd,
		Env:      getProcessEnv(imgSpec, rtEnv),
		User:     l.getProcessUser(),
		Terminal: getProcessTerminal(),
	}

	return &p, nil
}

// getProcessTerminal determines whether the container process should run with a terminal.
func getProcessTerminal() bool {
	// Override the default Process.Terminal to false if our stdin is not a terminal.
	if term.IsTerminal(syscall.Stdin) {
		return true
	}
	return false
}

// getProcessArgs returns the process args for a container, with reference to the OCI Image Spec.
// The process and image parameters may override the image CMD and/or ENTRYPOINT.
func getProcessArgs(imageSpec imgspecv1.Image, process string, args []string) []string {
	var processArgs []string

	if process != "" {
		processArgs = []string{process}
	} else {
		processArgs = imageSpec.Config.Entrypoint
	}

	if len(args) > 0 {
		processArgs = append(processArgs, args...)
	} else {
		if process == "" {
			processArgs = append(processArgs, imageSpec.Config.Cmd...)
		}
	}

	return processArgs
}

// getProcessUser computes the uid/gid(s) to be set on process execution.
// Currently this only supports the same uid / primary gid as on the host.
// TODO - expand for fakeroot, and arbitrary mapped user.
func (l *Launcher) getProcessUser() specs.User {
	if l.cfg.Fakeroot {
		return specs.User{
			UID: 0,
			GID: 0,
		}
	}
	return specs.User{
		UID: uint32(os.Getuid()),
		GID: uint32(os.Getgid()),
	}
}

// getProcessCwd computes the Cwd that the container process should start in.
// Currently this is the user's tmpfs home directory (see --containall).
func (l *Launcher) getProcessCwd() (dir string, err error) {
	if l.cfg.Fakeroot {
		return "/root", nil
	}

	pw, err := user.CurrentOriginal()
	if err != nil {
		return "", err
	}
	return pw.Dir, nil
}

// getReverseUserMaps returns uid and gid mappings that re-map container uid to host
// uid. This 'reverses' the host user to container root mapping in the initial
// userns from which the OCI runtime is launched.
//
//	host 1001 -> fakeroot userns 0 -> container 1001
func (l *Launcher) getReverseUserMaps() (uidMap, gidMap []specs.LinuxIDMapping, err error) {
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	// Get user's configured subuid & subgid ranges
	subuidRange, err := fakeroot.GetIDRange(fakeroot.SubUIDFile, uid)
	if err != nil {
		return nil, nil, err
	}
	// We must always be able to map at least 0->65535 inside the container, so we cover 'nobody'.
	if subuidRange.Size < 65536 {
		return nil, nil, fmt.Errorf("subuid range size (%d) must be at least 65536", subuidRange.Size)
	}
	subgidRange, err := fakeroot.GetIDRange(fakeroot.SubGIDFile, uid)
	if err != nil {
		return nil, nil, err
	}
	if subgidRange.Size < 65536 {
		return nil, nil, fmt.Errorf("subuid range size (%d) must be at least 65536", subgidRange.Size)
	}

	if uid < subuidRange.Size {
		uidMap = []specs.LinuxIDMapping{
			{
				ContainerID: 0,
				HostID:      1,
				Size:        uid,
			},
			{
				ContainerID: uid,
				HostID:      0,
				Size:        1,
			},
			{
				ContainerID: uid + 1,
				HostID:      uid + 1,
				Size:        subuidRange.Size - uid,
			},
		}
	} else {
		uidMap = []specs.LinuxIDMapping{
			{
				ContainerID: 0,
				HostID:      1,
				Size:        subuidRange.Size,
			},
			{
				ContainerID: uid,
				HostID:      0,
				Size:        1,
			},
		}
	}

	if gid < subgidRange.Size {
		gidMap = []specs.LinuxIDMapping{
			{
				ContainerID: 0,
				HostID:      1,
				Size:        gid,
			},
			{
				ContainerID: gid,
				HostID:      0,
				Size:        1,
			},
			{
				ContainerID: gid + 1,
				HostID:      gid + 1,
				Size:        subgidRange.Size - gid,
			},
		}
	} else {
		gidMap = []specs.LinuxIDMapping{
			{
				ContainerID: 0,
				HostID:      1,
				Size:        subgidRange.Size,
			},
			{
				ContainerID: gid,
				HostID:      0,
				Size:        1,
			},
		}
	}

	return uidMap, gidMap, nil
}

// getProcessEnv combines the image config ENV with the ENV requested at runtime.
// APPEND_PATH and PREPEND_PATH are honored as with the native apptainer runtime.
// LD_LIBRARY_PATH is modified to always include the apptainer lib bind directory.
func getProcessEnv(imageSpec imgspecv1.Image, runtimeEnv map[string]string) []string {
	path := ""
	appendPath := ""
	prependPath := ""
	ldLibraryPath := ""

	// Start with the environment from the image config.
	g := generate.New(nil)
	g.Config.Process = &specs.Process{Env: imageSpec.Config.Env}

	// Obtain PATH, and LD_LIBRARY_PATH if set in the image config, for special handling.
	for _, env := range imageSpec.Config.Env {
		e := strings.SplitN(env, "=", 2)
		if len(e) < 2 {
			continue
		}
		if e[0] == "PATH" {
			path = e[1]
		}
		if e[0] == "LD_LIBRARY_PATH" {
			ldLibraryPath = e[1]
		}
	}

	// Apply env vars from runtime, except PATH and LD_LIBRARY_PATH releated.
	for k, v := range runtimeEnv {
		switch k {
		case "PATH":
			path = v
		case "APPEND_PATH":
			appendPath = v
		case "PREPEND_PATH":
			prependPath = v
		case "LD_LIBRARY_PATH":
			ldLibraryPath = v
		default:
			g.SetProcessEnv(k, v)
		}
	}

	// Compute and set optionally APPEND-ed / PREPEND-ed PATH.
	if appendPath != "" {
		path = path + ":" + appendPath
	}
	if prependPath != "" {
		path = prependPath + ":" + path
	}
	if path != "" {
		g.SetProcessEnv("PATH", path)
	}

	// Ensure LD_LIBRARY_PATH always contains apptainer lib binding dir.
	if !strings.Contains(ldLibraryPath, apptainerLibs) {
		ldLibraryPath = strings.TrimPrefix(ldLibraryPath+":"+apptainerLibs, ":")
	}
	g.SetProcessEnv("LD_LIBRARY_PATH", ldLibraryPath)

	return g.Config.Process.Env
}

// defaultEnv returns default environment variables set in the container.
func defaultEnv(image, bundle string) map[string]string {
	return map[string]string{
		env.ApptainerPrefix + "CONTAINER": bundle,
		env.ApptainerPrefix + "NAME":      image,
	}
}

// apptainerEnvMap returns a map of APPTAINERENV_ prefixed env vars to their values.
func apptainerEnvMap() map[string]string {
	apptainerEnv := map[string]string{}

	for _, envVar := range os.Environ() {
		if !strings.HasPrefix(envVar, env.ApptainerEnvPrefix) {
			continue
		}
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimPrefix(parts[0], env.ApptainerEnvPrefix)
		apptainerEnv[key] = parts[1]
	}

	return apptainerEnv
}

// envFileMap returns a map of KEY=VAL env vars from an environment file
func envFileMap(ctx context.Context, f string) (map[string]string, error) {
	envMap := map[string]string{}

	content, err := os.ReadFile(f)
	if err != nil {
		return envMap, fmt.Errorf("could not read environment file %q: %w", f, err)
	}

	// Use the embedded shell interpreter to evaluate the env file, with an empty starting environment.
	// Shell takes care of comments, quoting etc. for us and keeps compatibility with native runtime.
	env, err := interpreter.EvaluateEnv(ctx, content, []string{}, []string{})
	if err != nil {
		return envMap, fmt.Errorf("while processing %s: %w", f, err)
	}

	for _, envVar := range env {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) < 2 {
			continue
		}
		// Strip out the runtime env vars set by the shell interpreter
		if parts[0] == "GID" ||
			parts[0] == "HOME" ||
			parts[0] == "IFS" ||
			parts[0] == "OPTIND" ||
			parts[0] == "PWD" ||
			parts[0] == "UID" {
			continue
		}
		envMap[parts[0]] = parts[1]
	}

	return envMap, nil
}
