package worker

import (
	"container/list"
	"sync"
)

type RequestQueue interface {
	Enqueue(*PodRequest)
	Dequeue() *PodRequest
	Len() int
	Remove(*PodRequest) bool
}

type PodRequest struct {
	owner  string
	result chan PodRequestResult
}

type PodRequestResult struct {
	addr string
	err  error
}

type requestQueue struct {
	mu  sync.Mutex
	dll *list.List
}

func NewRequestQueue() *requestQueue {
	return &requestQueue{dll: list.New()}
}

func (q *requestQueue) Enqueue(req *PodRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.dll.PushBack(req)
}

func (q *requestQueue) Remove(req *PodRequest) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for el := q.dll.Front(); el != nil; el = el.Next() {
		if el.Value == req {
			q.dll.Remove(el)
			return true
		}
	}

	return false
}

func (q *requestQueue) Dequeue() *PodRequest {
	q.mu.Lock()
	defer q.mu.Unlock()

	e := q.dll.Front()
	if e == nil {
		return nil
	}

	q.dll.Remove(e)
	return e.Value.(*PodRequest)
}

func (q *requestQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.dll.Len()
}
