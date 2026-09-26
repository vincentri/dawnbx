package aws

import (
	"context"
	"fmt"
	"reflect"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"dawnbx/internal/provider"
)

// load builds an aws.Config from the standard credential chain — environment,
// shared config files, named profiles, SSO, web identity — with the HTTP client
// injected. Injection is not a test convenience alone: the adapter must be able
// to run behind a proxy, and a test must be able to point the whole adapter at
// an httptest server without a network.
//
// Region is fixed on the config. A process provisions into one region; a handle
// names its own, so a client for a different region is a bug, not a fallback.
func load(ctx context.Context, region string, hc aws.HTTPClient) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if hc != nil {
		opts = append(opts, config.WithHTTPClient(hc))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load aws config for %s: %w", region, err)
	}
	return cfg, nil
}

// identity returns the account id the configured credentials belong to.
//
// STS GetCallerIdentity is the one call that answers "can this process spend
// money in this account" without spending any, so New calls it once. A control
// plane with no usable credentials must report the provider unavailable at
// startup, not fail a create twenty minutes into a stack.
//
// A failure becomes provider.ErrUnavailable, because that is what a missing
// credential source means to every route above. The message names the missing
// capability and the region; it never quotes a credential, a key id, an
// assumed-role session, or a token. Only the error's class crosses, so a
// diagnostic survives without the string.
func identity(ctx context.Context, cfg aws.Config) (string, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("%w: aws credentials for %s could not identify an account (%s)",
			provider.ErrUnavailable, cfg.Region, classify(err))
	}
	if out.Account == nil || *out.Account == "" {
		return "", fmt.Errorf("%w: aws credentials for %s returned no account id",
			provider.ErrUnavailable, cfg.Region)
	}
	return *out.Account, nil
}

// classify reduces an error to a credential-free description of its kind: the
// service error code when the service gave one, otherwise the Go type name. A
// type name is a symbol, not a value, and it is what tells an operator whether
// the chain found nothing at all or found something that has expired.
func classify(err error) string {
	if code := apiCode(err); code != "" {
		return code
	}
	t := reflect.TypeOf(err)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "unknown"
	}
	return t.Name()
}
