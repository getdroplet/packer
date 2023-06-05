package convert

import (
	"github.com/moby/buildkit/client/llb"

	"github.com/getdroplet/packer/pkg/environment"
	"github.com/getdroplet/packer/pkg/solver"
)

// BuildLLB translates package graph into LLB DAG.
func BuildLLB(graph *solver.PackageGraph, options *environment.Options) (llb.State, error) {
	return NewGraphLLB(graph, options).Build()
}

// MarshalLLB translates package graph into LLB DAG and marshals it.
func MarshalLLB(graph *solver.PackageGraph, options *environment.Options) (*llb.Definition, error) {
	return NewGraphLLB(graph, options).Marshal()
}
