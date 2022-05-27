package worker

import (
	"time"

	"github.com/go-logr/logr"
)

var defaultOpts = options{
	log:                 logr.Discard(),
	syncWaitTime:        30 * time.Second,
	maxIdleTime:         10 * time.Minute,
	watchTimeoutSeconds: 60,
}

type options struct {
	log                 logr.Logger
	maxIdleTime         time.Duration
	syncWaitTime        time.Duration
	watchTimeoutSeconds int64
}

type PoolOption func(o options) options

func SyncWaitTime(d time.Duration) PoolOption {
	return func(o options) options {
		o.syncWaitTime = d
		return o
	}
}

func MaxIdleTime(d time.Duration) PoolOption {
	return func(o options) options {
		o.maxIdleTime = d
		return o
	}
}

func WatchTimeoutSeconds(s int64) PoolOption {
	return func(o options) options {
		o.watchTimeoutSeconds = s
		return o
	}
}

func Logger(log logr.Logger) PoolOption {
	return func(o options) options {
		o.log = log
		return o
	}
}
