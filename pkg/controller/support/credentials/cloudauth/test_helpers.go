package cloudauth

import "context"

func CreateDefaultChallengeLoginServer(
	serviceName,
	realmName string,
	expectedErr error,
) func(ctx context.Context, loginServerURL string) (*AuthDirective, error) {
	var err error
	if expectedErr != nil {
		err = expectedErr
	}
	return func(ctx context.Context, loginServerURL string) (*AuthDirective, error) {
		return &AuthDirective{
			Service: serviceName,
			Realm:   realmName,
		}, err
	}
}
