// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"log"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/dag"
)

// ephemeralResourceCloseTransformer is a graph transformer that inserts a
// nodeEphemeralResourceClose node for each ephemeral resource, and arranges for
// the close node to depend on any other node that consumes the relevant
// ephemeral resource.
type ephemeralResourceCloseTransformer struct {
	// This does not need to run during validate walks since the ephemeral
	// resources will never be opened.
	skip bool
}

func (t *ephemeralResourceCloseTransformer) Transform(g *Graph) error {
	if t.skip {
		// Nothing to do if ephemeral resources are not opened
		return nil
	}

	verts := g.Vertices()
	for _, v := range verts {
		// find any ephemeral resource nodes
		v, ok := v.(GraphNodeConfigResource)
		if !ok {
			continue
		}
		addr := v.ResourceAddr()
		if addr.Resource.Mode != addrs.EphemeralResourceMode {
			continue
		}

		closeNode := &nodeEphemeralResourceClose{
			// the node must also be a ProviderConsumer
			resourceNode: v.(GraphNodeProviderConsumer),
			addr:         addr,
		}
		log.Printf("[TRACE] ephemeralResourceCloseTransformer: adding close node for %s", addr)
		g.Add(closeNode)
		g.Connect(dag.BasicEdge(closeNode, v))

		// Now we have an ephemeral resource, we need to depend on all
		// dependents of that resource. Rather than connect directly to them all
		// however, we'll only connect to leaf nodes by finding those that have
		// no up edges.
		for _, des := range g.Descendents(v) {
			// We want something which is both a referencer and has no incoming
			// edges from referencers. While it wouldn't be incorrect to just
			// check for all leaf nodes, we are trying to connect to the end of
			// evaluation chain, otherwise we may just as well wait til the end
			// of the walk and close everything together.
			//
			// FIXME: This can still get delayed excessively when intermediary
			// non-referencing nodes exist in the chain, like a nested  module
			// close node for example. What we've needed a couple times already
			// is some sort of breadth-first descend-until walk, which will stop
			// the current branch descent on some condition to act on that node,
			// yet still continue the rest of the walk.
			if _, ok := des.(GraphNodeReferencer); !ok {
				continue
			}

			up := g.UpEdges(des)
			up = up.Filter(func(v any) bool {
				_, ok := v.(GraphNodeReferencer)
				return ok
			})

			// This node has a referencer
			if len(up) > 0 {
				continue
			}

			g.Connect(dag.BasicEdge(closeNode, des))
		}
	}
	return nil
}
