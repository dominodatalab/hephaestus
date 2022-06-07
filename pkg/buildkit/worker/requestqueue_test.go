package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestQueue(t *testing.T) {
	req1 := &PodRequest{}
	req2 := &PodRequest{}

	queue := NewRequestQueue()
	assert.Equal(t, 0, queue.Len())
	assert.Nil(t, queue.Dequeue())
	assert.False(t, queue.Remove(&PodRequest{}))

	queue.Enqueue(req1)
	queue.Enqueue(req2)
	assert.Equal(t, 2, queue.Len())
	assert.Equal(t, req1, queue.Dequeue())
	assert.Equal(t, req2, queue.Dequeue())
	assert.Equal(t, 0, queue.Len())

	queue.Enqueue(req1)
	assert.Equal(t, 1, queue.Len())
	assert.True(t, queue.Remove(req1))
	assert.Equal(t, 0, queue.Len())
}
