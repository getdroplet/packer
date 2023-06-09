package convert

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"

	"github.com/moby/buildkit/client/llb"
	"github.com/opencontainers/go-digest"

	"github.com/getdroplet/packer/pkg/constants"
	"github.com/getdroplet/packer/pkg/environment"
	"github.com/getdroplet/packer/pkg/solver"
	"github.com/getdroplet/packer/pkg/types/v1alpha1"
)

const (
	tmpDirTemplate = "/tmp/build/%d"
	pkgDir         = "/pkg"
)

func defaultCopyOptions(options *environment.Options, reproducible bool) *llb.CopyInfo {
	copyOptions := &llb.CopyInfo{
		CopyDirContentsOnly: true,
		CreateDestPath:      true,
		FollowSymlinks:      true,
	}

	if reproducible {
		copyOptions.ChownOpt = &llb.ChownOpt{
			User: &llb.UserOpt{
				UID: 0,
			},
			Group: &llb.UserOpt{
				UID: 0,
			},
		}

		if !options.SourceDateEpoch.IsZero() {
			copyOptions.CreatedTime = &options.SourceDateEpoch
		}
	}

	return copyOptions
}

// NodeLLB wraps PackageNode to provide LLB conversion.
type NodeLLB struct {
	*solver.PackageNode

	Graph  *GraphLLB
	Prefix string
}

// NewNodeLLB wraps PackageNode for LLB conversion.
func NewNodeLLB(node *solver.PackageNode, graph *GraphLLB) *NodeLLB {
	return &NodeLLB{
		PackageNode: node,

		Graph:  graph,
		Prefix: graph.Options.CommonPrefix + node.Name + ":",
	}
}

func (node *NodeLLB) base() llb.State {
	return node.Graph.BaseImages[node.Pkg.Variant]
}

func (node *NodeLLB) install(root llb.State) llb.State {
	if len(node.Pkg.Install) > 0 {
		root = root.Run(
			append(node.Graph.commonRunOptions,
				llb.Args(
					append([]string{"/sbin/apk", "add", "--no-cache"}, node.Pkg.Install...)),
				llb.WithCustomName(node.Prefix+"apk-install"),
			)...,
		).Root()
	}

	return root
}

func (node *NodeLLB) context(root llb.State) llb.State {
	relPath := node.Pkg.BaseDir

	return root.File(
		llb.Copy(node.Graph.LocalContext, filepath.Join("/", relPath), pkgDir, defaultCopyOptions(node.Graph.Options, false)),
		llb.WithCustomNamef(node.Prefix+"context %s -> %s", relPath, pkgDir),
	)
}

func (node *NodeLLB) convertDependency(dep solver.PackageDependency) (depState llb.State, srcName string, err error) {
	if dep.IsInternal() {
		depState, err = NewNodeLLB(dep.Node, node.Graph).Build()
		if err != nil {
			return llb.Scratch(), "", err
		}

		srcName = dep.Node.Name
	} else {
		depState = llb.Image(dep.Image)
		srcName = dep.Image
	}

	return
}

func (node *NodeLLB) dependencies(root llb.State) (llb.State, error) {
	deps := make([]solver.PackageDependency, 0, len(node.Dependencies))

	// collect all the dependencies including transitive runtime dependencies
	// into a list, and then build LLB deduplicating dependencies on the fly

	// order is preserved in general with runtime dependencies following direct dependency,
	// but due to deduplication all the duplicates are removed (only first appearance
	// stays in the list)

	for _, dep := range node.Dependencies {
		deps = append(deps, dep)
		if dep.Node != nil {
			deps = append(deps, dep.Node.RuntimeDependencies()...)
		}
	}

	seen := map[string]struct{}{}

	stages := []llb.State{root}

	for _, dep := range deps {
		if _, alreadyProcessed := seen[dep.ID()]; alreadyProcessed {
			continue
		}

		seen[dep.ID()] = struct{}{}

		depState, srcName, err := node.convertDependency(dep)
		if err != nil {
			return llb.Scratch(), err
		}

		if dep.Src() == "/" && dep.Dest() == "/" {
			// skip copying if the source and destination are "/"
			stages = append(stages, depState)
		} else {
			stages = append(stages,
				llb.Scratch().File(
					llb.Copy(depState, dep.Src(), dep.Dest(), defaultCopyOptions(node.Graph.Options, false)),
					llb.WithCustomNamef(node.Prefix+"copy --from %s %s -> %s", srcName, dep.Src(), dep.Dest()),
				),
			)
		}
	}

	if len(stages) == 1 {
		return root, nil
	}

	return root.WithOutput(llb.Merge(stages, llb.WithCustomName(node.Prefix+"copy")).Output()), nil
}

func (node *NodeLLB) stepTmpDir(root llb.State, i int, step *v1alpha1.Step) llb.State {
	if step.TmpDir == "" {
		step.TmpDir = fmt.Sprintf(tmpDirTemplate, i)
	}

	return root.File(
		llb.Mkdir(step.TmpDir, constants.DefaultDirMode, llb.WithParents(true)),
		llb.WithCustomName(node.Prefix+"mkdir "+step.TmpDir),
	).Dir(step.TmpDir)
}

func (node *NodeLLB) stepDownload(root llb.State, step v1alpha1.Step) llb.State {
	if len(step.Sources) == 0 {
		return root
	}

	stages := []llb.State{root}

	for _, source := range step.Sources {
		download := llb.HTTP(
			source.URL,
			llb.Filename(filepath.Join("/", source.Destination)),
			llb.Checksum(digest.NewDigestFromEncoded(digest.SHA256, source.SHA256)),
			llb.WithCustomNamef(node.Prefix+"download %s -> %s", source.URL, source.Destination),
		)

		checksummer := node.Graph.Checksummer.File(
			llb.Mkfile("/checksums", 0o644, source.ToSHA512Sum()).
				Copy(download, "/", "/", defaultCopyOptions(node.Graph.Options, false)).
				Mkdir("/empty", constants.DefaultDirMode),
			llb.WithCustomName(node.Prefix+"cksum-prepare"),
		).Run(
			append(node.Graph.commonRunOptions,
				llb.Shlex("sha512sum -c --strict /checksums"),
				llb.WithCustomName(node.Prefix+"cksum-verify"),
			)...,
		).Root()

		stages = append(stages,
			llb.Scratch().File(
				llb.Copy(download, "/", step.TmpDir, defaultCopyOptions(node.Graph.Options, false)).
					Copy(checksummer, "/empty", "/", defaultCopyOptions(node.Graph.Options, false)), // TODO: this is "fake" dependency on checksummer
				llb.WithCustomName(node.Prefix+"download finalize"),
			),
		)
	}

	return root.WithOutput(llb.Merge(stages, llb.WithCustomName(node.Prefix+"download")).Output())
}

func (node *NodeLLB) stepEnvironment(root llb.State, step v1alpha1.Step) llb.State {
	vars := step.Env
	keys := make([]string, 0, len(vars))

	for key := range vars {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		root = root.AddEnv(key, vars[key])
	}

	return root
}

func (node *NodeLLB) stepScripts(root llb.State, i int, step v1alpha1.Step) llb.State {
	for _, script := range []struct {
		Desc         string
		Instructions v1alpha1.Instructions
	}{
		{"prepare", step.Prepare},
		{"build", step.Build},
		{"install", step.Install},
		{"test", step.Test},
	} {
		for _, instruction := range script.Instructions {
			runOptions := append([]llb.RunOption(nil), node.Graph.commonRunOptions...)

			if step.CachePath != "" {
				runOptions = append(runOptions,
					llb.AddMount(
						step.CachePath,
						llb.Scratch(),
						llb.AsPersistentCacheDir(
							path.Clean(node.Graph.Options.CacheIDNamespace+"/"+step.CachePath),
							llb.CacheMountShared,
						),
					),
				)
			}

			runOptions = append(runOptions,
				llb.Args([]string{
					node.Pkg.Shell.Get(),
					"-c",
					instruction.Script(),
				}),
				llb.WithCustomName(fmt.Sprintf("%s%s-%d", node.Prefix, script.Desc, i)),
			)

			root = root.Run(runOptions...).Root()
		}
	}

	return root
}

func (node *NodeLLB) step(root llb.State, i int, step v1alpha1.Step) llb.State {
	root = node.stepTmpDir(root, i, &step)
	root = node.stepDownload(root, step)
	root = node.stepEnvironment(root, step)
	root = node.stepScripts(root, i, step)

	return root
}

func (node *NodeLLB) finalize(root llb.State) llb.State {
	finalize := node.Pkg.Finalize
	return llb.Scratch().File(
		llb.Copy(root, finalize.From, finalize.To, defaultCopyOptions(node.Graph.Options, true)),
		llb.WithCustomNamef(node.Prefix+"finalize %s -> %s", finalize.From, finalize.To),
	)
}

// Build converts PackageNode to buildkit LLB.
func (node *NodeLLB) Build() (llb.State, error) {
	if state, ok := node.Graph.cache[node.PackageNode]; ok {
		return state, nil
	}

	root := node.base()

	root, err := node.dependencies(root)
	if err != nil {
		return llb.Scratch(), err
	}

	root = node.install(root)
	root = node.context(root)

	for i, step := range node.Pkg.Steps {
		root = node.step(root, i, step)
	}

	root = node.finalize(root)

	node.Graph.cache[node.PackageNode] = root

	return root, nil
}
