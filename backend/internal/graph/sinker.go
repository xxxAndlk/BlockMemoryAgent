package graph

import (
	"context"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// NewSinkerNode creates the terminal Sinker node that forces the graph to finish.
// It is exposed as a small factory so bootstrap/testserver/cmd/tui do not each
// redeclare the same private type.
func NewSinkerNode() ThreeLayerNode {
	return &sinkerNode{}
}

type sinkerNode struct{}

func (n *sinkerNode) Name() string { return "Sinker" }

func (n *sinkerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = enums.ActionFinish
	return state, nil
}
