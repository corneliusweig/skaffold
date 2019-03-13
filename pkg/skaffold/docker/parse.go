/*
Copyright 2019 The Skaffold Authors

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

package docker

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/docker/docker/builder/dockerignore"
	"github.com/docker/docker/pkg/fileutils"
	registry_v1 "github.com/google/go-containerregistry/pkg/v1"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/karrick/godirwalk"
	"github.com/moby/buildkit/frontend/dockerfile/command"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type from struct {
	image string
	as    string
}

type destinationsBySourcePath map[string][]string

func newdestinationsBySourcePath() destinationsBySourcePath {
	return destinationsBySourcePath(make(map[string][]string))
}

func (d destinationsBySourcePath) put(sourcePath, dest string) {
	if entry, ok := d[sourcePath]; ok {
		d[sourcePath] = append(entry, dest)
	} else {
		d[sourcePath] = []string{dest}
	}
}

func (d destinationsBySourcePath) toMap() map[string][]string {
	return d
}

var (
	// WorkingDir is overridden for unit testing
	WorkingDir = retrieveWorkingDir

	// RetrieveImage is overridden for unit testing
	RetrieveImage = retrieveImage
)

func ValidateDockerfile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		logrus.Warnf("opening file %s: %s", path, err.Error())
		return false
	}
	res, err := parser.Parse(f)
	if err != nil || res == nil || len(res.AST.Children) == 0 {
		return false
	}
	// validate each node contains valid dockerfile directive
	for _, child := range res.AST.Children {
		_, ok := command.Commands[child.Value]
		if !ok {
			return false
		}
	}

	return true
}

func expandBuildArgs(nodes []*parser.Node, buildArgs map[string]*string) {
	for i, node := range nodes {
		if node.Value != command.Arg {
			continue
		}

		// build arg's key
		keyValue := strings.Split(node.Next.Value, "=")
		key := keyValue[0]

		// build arg's value
		var value string
		if buildArgs[key] != nil {
			value = *buildArgs[key]
		} else if len(keyValue) > 1 {
			value = keyValue[1]
		}

		for _, node := range nodes[i+1:] {
			// Stop replacements if an arg is redefined with the same key
			if node.Value == command.Arg && strings.Split(node.Next.Value, "=")[0] == key {
				break
			}

			// replace $key with value
			for curr := node; curr != nil; curr = curr.Next {
				curr.Value = util.Expand(curr.Value, key, value)
			}
		}
	}
}

func fromInstruction(node *parser.Node) from {
	var as string
	if next := node.Next.Next; next != nil && strings.ToLower(next.Value) == "as" && next.Next != nil {
		as = next.Next.Value
	}

	return from{
		image: node.Next.Value,
		as:    strings.ToLower(as),
	}
}

func expandOnbuildInstructions(nodes []*parser.Node) ([]*parser.Node, error) {
	onbuildNodesCache := map[string][]*parser.Node{}
	var expandedNodes []*parser.Node
	n := 0
	for m, node := range nodes {
		if node.Value == command.From {
			from := fromInstruction(node)

			// `scratch` is case insensitive
			if strings.ToLower(from.image) == "scratch" {
				continue
			}

			// onbuild should immediately follow the from command
			expandedNodes = append(expandedNodes, nodes[n:m+1]...)
			n = m + 1

			var onbuildNodes []*parser.Node
			if ons, found := onbuildNodesCache[strings.ToLower(from.image)]; found {
				onbuildNodes = ons
			} else if ons, err := parseOnbuild(from.image); err == nil {
				onbuildNodes = ons
			} else {
				return nil, errors.Wrap(err, "parsing ONBUILD instructions")
			}

			// Stage names are case insensitive
			onbuildNodesCache[strings.ToLower(from.as)] = nodes
			onbuildNodesCache[strings.ToLower(from.image)] = nodes

			expandedNodes = append(expandedNodes, onbuildNodes...)
		}
	}
	expandedNodes = append(expandedNodes, nodes[n:]...)

	return expandedNodes, nil
}

func parseOnbuild(image string) ([]*parser.Node, error) {
	logrus.Debugf("Checking base image %s for ONBUILD triggers.", image)

	// Image names are case SENSITIVE
	img, err := RetrieveImage(image)
	if err != nil {
		logrus.Warnf("Error processing base image (%s) for ONBUILD triggers: %s. Dependencies may be incomplete.", image, err)
		return []*parser.Node{}, nil
	}

	if len(img.Config.OnBuild) == 0 {
		return []*parser.Node{}, nil
	}

	logrus.Debugf("Found ONBUILD triggers %v in image %s", img.Config.OnBuild, image)

	obRes, err := parser.Parse(strings.NewReader(strings.Join(img.Config.OnBuild, "\n")))
	if err != nil {
		return nil, err
	}

	return obRes.AST.Children, nil
}

func copiedFiles(nodes []*parser.Node) (map[string][]string, error) {
	slex := shell.NewLex('\\')
	copied := make(map[string][]string)

	var workdir string
	envs := make([]string, 0)
	for _, node := range nodes {
		switch node.Value {
		case command.From:
			wd, err := WorkingDir(node.Next.Value)
			if err != nil {
				return nil, err
			}
			workdir = wd
		case command.Workdir:
			value, err := slex.ProcessWord(node.Next.Value, envs)
			if err != nil {
				return nil, errors.Wrap(err, "processing word")
			}
			workdir = changeWorkingDir(workdir, value)
		case command.Add, command.Copy:
			dest, files, err := processCopy(node, envs, workdir)
			if err != nil {
				return nil, err
			}

			if len(files) > 0 {
				copied[dest] = files
			}
		case command.Env:
			// one env command may define multiple variables
			for node := node.Next; node != nil && node.Next != nil; node = node.Next.Next {
				envs = append(envs, fmt.Sprintf("%s=%s", node.Value, node.Next.Value))
			}
		}
	}

	return copied, nil
}

func readDockerfile(workspace, absDockerfilePath string, buildArgs map[string]*string) (map[string][]string, error) {
	f, err := os.Open(absDockerfilePath)
	if err != nil {
		return nil, errors.Wrapf(err, "opening dockerfile: %s", absDockerfilePath)
	}
	defer f.Close()

	res, err := parser.Parse(f)
	if err != nil {
		return nil, errors.Wrap(err, "parsing dockerfile")
	}

	dockerfileLines := res.AST.Children

	expandBuildArgs(dockerfileLines, buildArgs)

	dockerfileLinesWithOnbuild, err := expandOnbuildInstructions(dockerfileLines)
	if err != nil {
		return nil, errors.Wrap(err, "expanding ONBUILD instructions")
	}

	copied, err := copiedFiles(dockerfileLinesWithOnbuild)
	if err != nil {
		return nil, errors.Wrap(err, "listing copied files")
	}

	return expandPaths(workspace, copied)
}

func expandPaths(workspace string, copied map[string][]string) (map[string][]string, error) {
	destsBySourcePath := newdestinationsBySourcePath()
	for dest, files := range copied {
		matchesOne := false

		for _, p := range files {
			path := filepath.Join(workspace, p)
			if _, err := os.Stat(path); err == nil {
				destsBySourcePath.put(p, dest)
				matchesOne = true
				continue
			}

			files, err := filepath.Glob(path)
			if err != nil {
				return nil, errors.Wrap(err, "invalid glob pattern")
			}
			if files == nil {
				continue
			}

			for _, f := range files {
				rel, err := filepath.Rel(workspace, f)
				if err != nil {
					return nil, fmt.Errorf("getting relative path of %s", f)
				}

				destsBySourcePath.put(rel, dest)
			}
			matchesOne = true
		}

		if !matchesOne {
			return nil, fmt.Errorf("file pattern %s must match at least one file", files)
		}
	}

	logrus.Debugf("Found dependencies for dockerfile: %v", destsBySourcePath.toMap())

	return destsBySourcePath.toMap(), nil
}

// NormalizeDockerfilePath returns the absolute path to the dockerfile.
func NormalizeDockerfilePath(context, dockerfile string) (string, error) {
	if filepath.IsAbs(dockerfile) {
		return dockerfile, nil
	}

	if !strings.HasPrefix(dockerfile, context) {
		dockerfile = filepath.Join(context, dockerfile)
	}
	return filepath.Abs(dockerfile)
}

// GetDependencies finds the sources dependencies for the given docker artifact.
// All paths are relative to the workspace.
func GetDependencies(ctx context.Context, workspace string, dockerfilePath string, buildArgs map[string]*string) (map[string][]string, error) {
	absDockerfilePath, err := NormalizeDockerfilePath(workspace, dockerfilePath)
	if err != nil {
		return nil, errors.Wrap(err, "normalizing dockerfile path")
	}

	deps, err := readDockerfile(workspace, absDockerfilePath, buildArgs)
	if err != nil {
		return nil, err
	}

	// Read patterns to ignore
	var excludes []string
	dockerignorePath := filepath.Join(workspace, ".dockerignore")
	if _, err := os.Stat(dockerignorePath); !os.IsNotExist(err) {
		r, err := os.Open(dockerignorePath)
		if err != nil {
			return nil, err
		}
		defer r.Close()

		excludes, err = dockerignore.ReadAll(r)
		if err != nil {
			return nil, err
		}
	}

	pExclude, err := fileutils.NewPatternMatcher(excludes)
	if err != nil {
		return nil, errors.Wrap(err, "invalid exclude patterns")
	}

	// Walk the workspace
	files := make(map[string]bool)
	for dep := range deps {
		dep = filepath.Clean(dep)
		absDep := filepath.Join(workspace, dep)

		fi, err := os.Stat(absDep)
		if err != nil {
			return nil, errors.Wrapf(err, "stating file %s", absDep)
		}

		switch mode := fi.Mode(); {
		case mode.IsDir():
			if err := godirwalk.Walk(absDep, &godirwalk.Options{
				Unsorted: true,
				Callback: func(fpath string, info *godirwalk.Dirent) error {
					if fpath == absDep {
						return nil
					}

					relPath, err := filepath.Rel(workspace, fpath)
					if err != nil {
						return err
					}

					ignored, err := pExclude.Matches(relPath)
					if err != nil {
						return err
					}

					if info.IsDir() {
						if ignored {
							return filepath.SkipDir
						}
					} else if !ignored {
						files[relPath] = true
					}

					return nil
				},
			}); err != nil {
				return nil, errors.Wrapf(err, "walking folder %s", absDep)
			}
		case mode.IsRegular():
			ignored, err := pExclude.Matches(dep)
			if err != nil {
				return nil, err
			}

			if !ignored {
				files[dep] = true
			}
		}
	}

	// Always add dockerfile even if it's .dockerignored. The daemon will need it anyways.
	if !filepath.IsAbs(dockerfilePath) {
		files[dockerfilePath] = true
	} else {
		files[absDockerfilePath] = true
	}

	// Ignore .dockerignore
	delete(files, ".dockerignore")

	dependencies := map[string][]string{}
	defaultDst := []string{""}
	for file := range files {
		dependencies[file] = defaultDst
	}

	return dependencies, nil
}

func retrieveImage(image string) (*v1.ConfigFile, error) {
	localDaemon, err := NewAPIClient() // Cached after first call
	if err != nil {
		return nil, errors.Wrap(err, "getting docker client")
	}

	return localDaemon.ConfigFile(context.Background(), image)
}

func processCopy(value *parser.Node, envs []string, workdir string) (destination string, copied []string, err error) {
	slex := shell.NewLex('\\')
	for {
		// Skip last node, since it is the destination, and stop if we arrive at a comment
		if value.Next.Next == nil || strings.HasPrefix(value.Next.Next.Value, "#") {
			destination = changeWorkingDir(workdir, value.Next.Value)
			break
		}
		src, err := slex.ProcessWord(value.Next.Value, envs)
		if err != nil {
			return "", nil, errors.Wrap(err, "processing word")
		}
		// If the --from flag is provided, we are dealing with a multi-stage dockerfile
		// Adding a dependency from a different stage does not imply a source dependency
		if hasMultiStageFlag(value.Flags) {
			return "", nil, nil
		}
		if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			copied = append(copied, src)
		} else {
			logrus.Debugf("Skipping watch on remote dependency %s", src)
		}

		value = value.Next
	}

	return
}

func hasMultiStageFlag(flags []string) bool {
	for _, f := range flags {
		if strings.HasPrefix(f, "--from=") {
			return true
		}
	}
	return false
}

func retrieveWorkingDir(tagged string) (string, error) {
	var cf *registry_v1.ConfigFile
	var err error

	localDocker, err := NewAPIClient()
	if err != nil {
		// No local Docker is available
		cf, err = RetrieveRemoteConfig(tagged)
	} else {
		cf, err = localDocker.ConfigFile(context.Background(), tagged)
	}
	if err != nil {
		return "", errors.Wrap(err, "retrieving image config")
	}

	if cf.Config.WorkingDir == "" {
		return "/", nil
	}
	return cf.Config.WorkingDir, nil
}

func changeWorkingDir(cur, to string) string {
	if path.IsAbs(to) {
		return path.Clean(to)
	}
	return path.Clean(path.Join(to, cur))
}
