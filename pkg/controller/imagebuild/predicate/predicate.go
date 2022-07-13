package predicate

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var _ predicate.Predicate = BlindDeletePredicate{}

// BlindDeletePredicate implements a deleted predicate function that always returns true.
//
// All other events are ignored.
type BlindDeletePredicate struct{}

func (p BlindDeletePredicate) Create(event.CreateEvent) bool   { return false }
func (p BlindDeletePredicate) Delete(event.DeleteEvent) bool   { return true }
func (p BlindDeletePredicate) Update(event.UpdateEvent) bool   { return false }
func (p BlindDeletePredicate) Generic(event.GenericEvent) bool { return false }
