package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestQueue(t *testing.T) {
	var ch1 chan PodRequestResult
	var ch2 chan PodRequestResult

	queue := NewRequestQueue()
	assert.Equal(t, 0, queue.Len())
	assert.Nil(t, queue.Dequeue())
	assert.False(t, queue.Remove(ch1))

	queue.Enqueue(ch1)
	queue.Enqueue(ch2)
	assert.Equal(t, 2, queue.Len())
	assert.Equal(t, ch1, queue.Dequeue())
	assert.Equal(t, ch2, queue.Dequeue())
	assert.Equal(t, 0, queue.Len())

	queue.Enqueue(ch1)
	assert.Equal(t, 1, queue.Len())
	assert.True(t, queue.Remove(ch1))
	assert.Equal(t, 0, queue.Len())
}
