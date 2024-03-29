package worker

import (
	"container/list"
	"sync"
)

type RequestQueue interface {
	Enqueue(r *PodRequest)
	Dequeue() *PodRequest
	Len() int
	Remove(r *PodRequest) bool
}

type PodRequest struct {
	owner  string
	result chan PodRequestResult
}

type PodRequestResult struct {
	addr string
	err  error
}

type Queue struct {
	mu  sync.Mutex
	dll *list.List
}

func NewRequestQueue() *Queue {
	return &Queue{dll: list.New()}
}

func (q *Queue) Enqueue(req *PodRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.dll.PushBack(req)
}

func (q *Queue) Remove(req *PodRequest) bool {
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

func (q *Queue) Dequeue() *PodRequest {
	q.mu.Lock()
	defer q.mu.Unlock()

	e := q.dll.Front()
	if e == nil {
		return nil
	}

	q.dll.Remove(e)
	return e.Value.(*PodRequest)
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.dll.Len()
}
