package worker

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
)

func TestPoolOptions(t *testing.T) {
	opts := Options{
		Log:                         logr.Logger{},
		MaxIdleTime:                 0,
		SyncWaitTime:                0,
		EndpointWatchTimeoutSeconds: 0,
	}

	opts = SyncWaitTime(10 * time.Minute)(opts)
	assert.Equal(t, 10*time.Minute, opts.SyncWaitTime)

	opts = MaxIdleTime(30 * time.Minute)(opts)
	assert.Equal(t, 30*time.Minute, opts.MaxIdleTime)

	opts = EndpointWatchTimeoutSeconds(300)(opts)
	assert.Equal(t, int64(300), opts.EndpointWatchTimeoutSeconds)

	opts = Logger(logr.Discard())(opts)
	assert.Equal(t, logr.Discard(), opts.Log)
}
