package clusterapply

import (
	"fmt"
	"time"

	ctldiff "github.com/k14s/kapp/pkg/kapp/diff"
	ctldgraph "github.com/k14s/kapp/pkg/kapp/diffgraph"
)

type ClusterChangeSetOpts struct {
	WaitTimeout       time.Duration
	WaitCheckInterval time.Duration
}

type ClusterChangeSet struct {
	changes              []ctldiff.Change
	opts                 ClusterChangeSetOpts
	clusterChangeFactory ClusterChangeFactory
	ui                   UI
}

func NewClusterChangeSet(changes []ctldiff.Change, opts ClusterChangeSetOpts,
	clusterChangeFactory ClusterChangeFactory, ui UI) ClusterChangeSet {

	return ClusterChangeSet{changes, opts, clusterChangeFactory, ui}
}

func (c ClusterChangeSet) Calculate() ([]*ClusterChange, *ctldgraph.ChangeGraph, error) {
	var wrappedClusterChanges []ctldgraph.ActualChange

	for _, change := range c.changes {
		clusterChange := c.clusterChangeFactory.NewClusterChange(change)
		wrappedClusterChanges = append(wrappedClusterChanges, wrappedClusterChange{clusterChange})
	}

	changesGraph, err := ctldgraph.NewChangeGraph(wrappedClusterChanges)
	if err != nil {
		return nil, nil, err
	}

	changesGraph.AllMatching(func(change *ctldgraph.Change) bool {
		c.markChangesToWait(change)
		return false
	})

	// Prune out changes that are not involved with anything
	changesGraph.RemoveMatching(func(change *ctldgraph.Change) bool {
		clusterChange := change.Change.(wrappedClusterChange).ClusterChange

		return clusterChange.ApplyOp() == ClusterChangeApplyOpNoop &&
			clusterChange.WaitOp() == ClusterChangeWaitOpNoop
	})

	var clusterChanges []*ClusterChange

	for _, change := range changesGraph.All() {
		clusterChange := change.Change.(wrappedClusterChange).ClusterChange
		clusterChanges = append(clusterChanges, clusterChange)
	}

	return clusterChanges, changesGraph, nil
}

func (c ClusterChangeSet) markChangesToWait(change *ctldgraph.Change) bool {
	var needsWaiting bool
	for _, ch := range change.WaitingFor {
		if c.markChangesToWait(ch) {
			needsWaiting = true
			break
		}
	}
	if needsWaiting {
		change.Change.(wrappedClusterChange).MarkNeedsWaiting()
		return true
	}
	return change.Change.(wrappedClusterChange).WaitOp() != ClusterChangeWaitOpNoop
}

func (c ClusterChangeSet) Apply(changesGraph *ctldgraph.ChangeGraph) error {
	allChanges := changesGraph.All()
	expectedNumChanges := len(allChanges)

	blockedChanges := ctldgraph.NewBlockedChanges(changesGraph)
	applyingChanges := NewApplyingChanges(expectedNumChanges, c.clusterChangeFactory, c.ui)
	waitingChanges := NewWaitingChanges(expectedNumChanges, c.opts, c.ui)

	for {
		appliedChanges, err := applyingChanges.Apply(blockedChanges.Unblocked())
		if err != nil {
			return err
		}

		waitingChanges.Track(appliedChanges)

		if waitingChanges.IsEmpty() {
			// Sanity check that we applied all changes
			appliedNumChanges := applyingChanges.NumApplied()

			if expectedNumChanges != appliedNumChanges {
				fmt.Printf("%s\n", blockedChanges.WhyBlocked(blockedChanges.Blocked()))
				return fmt.Errorf("Internal inconsistency: did not apply all changes: %d != %d",
					expectedNumChanges, appliedNumChanges)
			}

			c.ui.NotifySection("changes applied")
			return nil
		}

		doneChanges, err := waitingChanges.WaitForAny()
		if err != nil {
			return err
		}

		for _, change := range doneChanges {
			blockedChanges.Unblock(change.Graph)
		}
	}
}

func ClusterChangesAsChangeViews(changes []*ClusterChange) []ChangeView {
	var result []ChangeView
	for _, change := range changes {
		result = append(result, change)
	}
	return result
}

func ClusterChangesCount(changes []*ClusterChange, counted func(*ClusterChange) bool) int {
	var result int
	for _, change := range changes {
		if counted(change) {
			result += 1
		}
	}
	return result
}

type wrappedClusterChange struct {
	*ClusterChange
}

func (c wrappedClusterChange) Op() ctldgraph.ActualChangeOp {
	op := c.ApplyOp()

	switch op {
	case ClusterChangeApplyOpAdd, ClusterChangeApplyOpUpdate:
		return ctldgraph.ActualChangeOpUpsert

	case ClusterChangeApplyOpDelete:
		return ctldgraph.ActualChangeOpDelete

	case ClusterChangeApplyOpNoop:
		return ctldgraph.ActualChangeOpNoop

	default:
		panic(fmt.Sprintf("Unknown change apply operation: %s", op))
	}
}

func (c wrappedClusterChange) WaitOp() ClusterChangeWaitOp {
	return c.ClusterChange.WaitOp()
}

func (c wrappedClusterChange) MarkNeedsWaiting() {
	c.ClusterChange.MarkNeedsWaiting()
}
