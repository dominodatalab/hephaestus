package worker

import (
	"container/list"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

type RequestQueue interface {
	Enqueue(chan PodRequestResult)
	Dequeue() chan PodRequestResult
	Len() int
	Remove(chan PodRequestResult) bool
}

type PodRequestResult struct {
	pod *corev1.Pod
	err error
}

type requestQueue struct {
	mu  sync.Mutex
	dll *list.List
}

func NewRequestQueue() *requestQueue {
	return &requestQueue{dll: list.New()}
}

func (q *requestQueue) Enqueue(ch chan PodRequestResult) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.dll.PushBack(ch)
}

func (q *requestQueue) Remove(ch chan PodRequestResult) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for el := q.dll.Front(); el != nil; el = el.Next() {
		if el.Value == ch {
			q.dll.Remove(el)
			return true
		}
	}

	return false
}

func (q *requestQueue) Dequeue() chan PodRequestResult {
	q.mu.Lock()
	defer q.mu.Unlock()

	e := q.dll.Front()
	if e == nil {
		return nil
	}

	q.dll.Remove(e)
	return e.Value.(chan PodRequestResult)
}

func (q *requestQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.dll.Len()
}
