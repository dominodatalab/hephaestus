package predicate

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	hephv1 "github.com/dominodatalab/hephaestus/pkg/api/hephaestus/v1"
)

var _ predicate.Predicate = UnprocessedTransitionsPredicate{}

// UnprocessedTransitionsPredicate implements an updated predicate function on ImageBuildTransition changes.
//
// This predicate will skip update events when the object's status.transitions collection is either empty or has no
// unprocessed transitions (i.e. status.transitions[*].processed == true). All other events (create/delete/generic) will
// be entirely.
type UnprocessedTransitionsPredicate struct{}

func (p UnprocessedTransitionsPredicate) Update(e event.UpdateEvent) bool {
	ib := e.ObjectNew.(*hephv1.ImageBuild)

	for _, transition := range ib.Status.Transitions {
		if !transition.Processed {
			return true
		}
	}

	return false
}
func (p UnprocessedTransitionsPredicate) Create(event.CreateEvent) bool   { return false }
func (p UnprocessedTransitionsPredicate) Delete(event.DeleteEvent) bool   { return false }
func (p UnprocessedTransitionsPredicate) Generic(event.GenericEvent) bool { return false }
