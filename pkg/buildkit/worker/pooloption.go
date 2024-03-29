package worker

import (
	"time"

	"github.com/go-logr/logr"
)

var defaultOpts = Options{
	Log:                         logr.Discard(),
	SyncWaitTime:                30 * time.Second,
	MaxIdleTime:                 10 * time.Minute,
	EndpointWatchTimeoutSeconds: 180,
}

type Options struct {
	Log                         logr.Logger
	MaxIdleTime                 time.Duration
	SyncWaitTime                time.Duration
	EndpointWatchTimeoutSeconds int64
}

type PoolOption func(o Options) Options

func SyncWaitTime(d time.Duration) PoolOption {
	return func(o Options) Options {
		o.SyncWaitTime = d
		return o
	}
}

func MaxIdleTime(d time.Duration) PoolOption {
	return func(o Options) Options {
		o.MaxIdleTime = d
		return o
	}
}

func EndpointWatchTimeoutSeconds(s int64) PoolOption {
	return func(o Options) Options {
		o.EndpointWatchTimeoutSeconds = s
		return o
	}
}

func Logger(log logr.Logger) PoolOption {
	return func(o Options) Options {
		o.Log = log
		return o
	}
}
