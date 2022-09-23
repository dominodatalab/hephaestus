package cloudauth

import (
	"context"
	"errors"
	"github.com/go-logr/logr"
	"regexp"

	"github.com/docker/docker/api/types"
)

var ErrNoLoader = errors.New("no loader found")

type AuthLoader func(ctx context.Context, logger logr.Logger, server string) (*types.AuthConfig, error)

type Registry struct {
	loaders map[*regexp.Regexp]AuthLoader
}

// RetrieveAuthorization will multiplex registered auth loaders based on url pattern and use the appropriate one to
// make an authorization request. The returned value can be marshalled into the contents of a Docker config.json file.
func (r *Registry) RetrieveAuthorization(
	ctx context.Context,
	logger logr.Logger,
	server string,
) (*types.AuthConfig, error) {
	for r, loader := range r.loaders {
		if r.MatchString(server) {
			return loader(ctx, logger, server)
		}
	}
	return nil, ErrNoLoader
}

// Register will create a new url regex -> authorization loader scheme.
func (r *Registry) Register(re *regexp.Regexp, loader AuthLoader) {
	if r.loaders == nil {
		r.loaders = map[*regexp.Regexp]AuthLoader{}
	}
	r.loaders[re] = loader
}
