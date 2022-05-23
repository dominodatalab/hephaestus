package worker

import (
	"container/list"
	"sync"
)

type RequestQueue interface {
	Enqueue(chan LeaseRequest)
	Dequeue() chan LeaseRequest
	Len() int
}

type LeaseRequest struct {
	addr string
	err  error
}

type channelQueue struct {
	mu  sync.Mutex
	dll *list.List
}

func NewRequestQueue() RequestQueue {
	return &channelQueue{dll: list.New()}
}

func (q *channelQueue) Enqueue(ch chan LeaseRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.dll.PushBack(ch)
}

func (q *channelQueue) Dequeue() chan LeaseRequest {
	q.mu.Lock()
	defer q.mu.Unlock()

	e := q.dll.Front()
	q.dll.Remove(e)

	return e.Value.(chan LeaseRequest)
}

func (q *channelQueue) Len() int {
	return q.dll.Len()
}
