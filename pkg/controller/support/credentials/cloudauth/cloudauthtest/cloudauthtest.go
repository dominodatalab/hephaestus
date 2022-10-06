package cloudauthtest

import (
	"context"

	"github.com/dominodatalab/hephaestus/pkg/controller/support/credentials/cloudauth"
)

func FakeChallengeLoginServer(serviceName, realmName string, expectedErr error) cloudauth.LoginChallenger {
	return func(context.Context, string) (*cloudauth.AuthDirective, error) {
		if expectedErr != nil {
			return nil, expectedErr
		}

		return &cloudauth.AuthDirective{
			Service: serviceName,
			Realm:   realmName,
		}, nil
	}
}
