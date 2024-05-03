// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"
	"log"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/collections"
	"github.com/hashicorp/terraform/internal/dag"
)

// graphNodeEphemeralResourceConsumer is implemented by graph node types that
// can validly refer to ephemeral resources, to announce which ephemeral
// resources they each depend on.
//
// This is used to decide the dependencies for [nodeEphemeralResourceClose]
// nodes.
type graphNodeEphemeralResourceConsumer interface {
	// requiredEphemeralResources returns a set of all of the ephemeral
	// resources that the receiver directly depends on when performing
	// the given walk operation.
	//
	// Although the addrs package types can't constrain this statically,
	// this method should return only addresses of mode
	// [addrs.EphemeralResourceMode]. Resources of any other mode are invalid
	// to return.
	//
	// walkOperation is normalized for implementation simplicity: it can be
	// either [walkPlan] or [walkApply], and no other type.
	requiredEphemeralResources(op walkOperation) addrs.Set[addrs.ConfigResource]
}

// requiredEphemeralResourcesForReferencer is a helper for implementing
// [graphNodeEphemeralResourceConsumer] for any node type which implements
// [GraphNodeReferencer] and whose reported references can entirely describe
// the needed ephemeral resources.
func requiredEphemeralResourcesForReferencer[T GraphNodeReferencer](n T) addrs.Set[addrs.ConfigResource] {
	moduleAddr := n.ModulePath()
	refs := n.References()
	if len(refs) == 0 {
		return nil
	}
	ret := addrs.MakeSet[addrs.ConfigResource]()
	for _, ref := range refs {
		var resourceAddr addrs.Resource
		switch refAddr := ref.Subject.(type) {
		case addrs.Resource:
			resourceAddr = refAddr
		case addrs.ResourceInstance:
			resourceAddr = refAddr.Resource
		default:
			continue
		}
		if resourceAddr.Mode != addrs.EphemeralResourceMode {
			continue // we only care about ephemeral resources here
		}
		ret.Add(resourceAddr.InModule(moduleAddr))
	}
	return ret
}

// ephemeralResourceCloseTransformer is a graph transformer that inserts
// a [nodeEphemeralResourceClose] node for each ephemeral resource whose "open"
// is represented by at least one existing node, and arranges for the close
// node to depend on the open node and on any other node that consumes the
// relevant ephemeral resource.
//
// This transformer also prunes nodes for any ephemeral resources that have
// no consumers for the given walk operation. In particular this means that
// Terraform will not open any instances of an ephemeral resource that is
// only used in resource provisioners if the graph is not being built for the
// apply phase, because only the apply phase actually executes provisioners.
//
// This transformer must run after any other transformer that might introduce
// an ephemeral resource node into the graph, or that might given an existing
// node information it needs to properly announce any ephemeral resources it
// consumes.
type ephemeralResourceCloseTransformer struct {
	// op must be one of walkValidate, walkPlan, or walkApply. For other walk
	// operations, choose walkApply if the walk will execute resource
	// provisioners or walkPlan otherwise.
	//
	// if op is walkValidate then this transformer does absolutely nothing,
	// because we don't open or close ephemeral resources during the validate
	// walk.
	op walkOperation
}

func (t *ephemeralResourceCloseTransformer) Transform(g *Graph) error {
	if t.op != walkApply && t.op != walkPlan {
		// Nothing to do for any other walks, because only plan-like or
		// apply-like walks actually open ephemeral resource instances.
		return nil
	}

	// We'll freeze the set of vertices we started with so that we can
	// visit it multiple times while we're modifying the graph.
	verts := g.Vertices()

	// First we'll find all of the ephemeral resources that already have
	// at least one node in the graph, and we'll assume those are all
	// "open" nodes. Each distinct ephemeral resource address gets one
	// close node that depends on all of the nodes that might open instances
	// of it.
	openNodes := addrs.MakeMap[addrs.ConfigResource, collections.Set[dag.Vertex]]()
	closeNodes := addrs.MakeMap[addrs.ConfigResource, *nodeEphemeralResourceClose]()
	for _, v := range verts {
		v, ok := v.(GraphNodeConfigResource)
		if !ok {
			continue
		}
		addr := v.ResourceAddr()
		if addr.Resource.Mode != addrs.EphemeralResourceMode {
			continue
		}
		if !openNodes.Has(addr) {
			openNodes.Put(addr, collections.NewSetCmp[dag.Vertex]())
		}
		openNodes.Get(addr).Add(v)

		if !closeNodes.Has(addr) {
			closeNode := &nodeEphemeralResourceClose{
				addr: addr,
			}
			closeNodes.Put(addr, closeNode)
			log.Printf("[TRACE] ephemeralResourceCloseTransformer: adding close node for %s", addr)
			g.Add(closeNode)
		}
		closeNode := closeNodes.Get(addr)

		// The close node depends on the open node, because we can't
		// close an ephemeral resource instance until we've opened it.
		g.Connect(dag.BasicEdge(closeNode, v))
	}

	consumerCount := addrs.MakeMap[addrs.ConfigResource, int]()
	for _, v := range verts {
		v, ok := v.(graphNodeEphemeralResourceConsumer)
		if !ok {
			continue
		}
		for _, consumedAddr := range v.requiredEphemeralResources(t.op) {
			if consumedAddr.Resource.Mode != addrs.EphemeralResourceMode {
				// Should not happen: correct implementations of
				// [graphNodeEphemeralResourceConsumer] only return
				// ephemeral resource addresses.
				panic(fmt.Sprintf("node %s incorrectly reported %s as an ephemeral resource", dag.VertexName(v), consumedAddr))
			}
			closeNode := closeNodes.Get(consumedAddr)
			if closeNode == nil {
				// Suggests that there's a reference to an ephemeral resource
				// that isn't declared, which is invalid but it's not this
				// transformer's responsibility to detect that invalidity,
				// so we'll just ignore it.
				log.Printf("[TRACE] ephemeralResourceCloseTransformer: %s refers to undeclared ephemeral resource %s", dag.VertexName(v), consumedAddr)
				continue
			}
			consumerCount.Put(consumedAddr, consumerCount.Get(consumedAddr)+1)

			// The close node depends on anything that consumes instances of
			// the ephemeral resource, because we mustn't close it while
			// other components are still using it.
			g.Connect(dag.BasicEdge(closeNode, v))
		}
	}

	// Finally, if we found any ephemeral resources that don't have any
	// consumers then we'll prune out all of their open and close nodes
	// to avoid redundantly opening and closing something that we aren't
	// going to use anyway.
	// (We don't use this transformer in the validate walk,
	for _, elem := range openNodes.Elems {
		if consumerCount.Get(elem.Key) == 0 {
			for _, v := range elem.Value.Elems() {
				log.Printf("[TRACE] ephemeralResourceCloseTransformer: pruning %s because it has no consumers", dag.VertexName(v))
				g.Remove(v)
			}
		}
	}
	for _, elem := range closeNodes.Elems {
		if consumerCount.Get(elem.Key) == 0 {
			log.Printf("[TRACE] ephemeralResourceCloseTransformer: pruning %s because it has no consumers", dag.VertexName(elem.Value))
			g.Remove(elem.Value)
		}
	}

	return nil
}