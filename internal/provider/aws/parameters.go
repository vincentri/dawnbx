package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"dawnbx/internal/provider"
)

// The bootstrap credential never travels in a stack parameter, a tag, a user
// data blob or a create request. It is one SSM SecureString, written before the
// stack exists and read on the host by the instance's own scoped role. That is
// the whole delivery guarantee this adapter makes, and it is why
// Capabilities.Delivery names it.

// parameters is the secret store for one region. It is a value, not a field on
// the adapter, because a cluster's parameter must live in the same region as
// the stack that reads it and a handle may name a different region than the one
// this process was started with.
type parameters struct{ c *ssm.Client }

func parametersFor(c *ssm.Client) parameters { return parameters{c: c} }

// putSecret stores value as a SecureString under name, overwriting whatever was
// there. Overwriting is the rotation path: SetBootstrap writes the same name
// again and the host's next read gets the new value.
//
// The value is never part of an error. The API does not echo it, but a
// misconfigured proxy or a debug HTTP client could, so the message is scrubbed
// of the value before it leaves this function. The cause is still wrapped, so
// errors.As finds the SDK's error type underneath.
func (p parameters) putSecret(ctx context.Context, name, value string) error {
	_, err := p.c.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(name),
		Value:     aws.String(value),
		Type:      ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("write parameter %s: %w", name, scrub(err, value))
	}
	return nil
}

// getSecret reads and decrypts a SecureString. A name that does not exist is
// provider.ErrNotFound rather than a transport error, because every caller of
// this is asking whether a cluster still holds its credential.
func (p parameters) getSecret(ctx context.Context, name string) (string, error) {
	out, err := p.c.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		var missing *ssmtypes.ParameterNotFound
		if errors.As(err, &missing) || absentMessage(err) {
			return "", fmt.Errorf("%w: parameter %s", provider.ErrNotFound, name)
		}
		return "", fmt.Errorf("read parameter %s: %w", name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("%w: parameter %s has no value", provider.ErrNotFound, name)
	}
	return *out.Parameter.Value, nil
}

// deleteSecret removes a parameter and treats an absent one as success. Destroy
// must be safe to call twice, and a parameter that is already gone is the state
// the second call is being asked to confirm.
func (p parameters) deleteSecret(ctx context.Context, name string) error {
	_, err := p.c.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)})
	if err != nil {
		var missing *ssmtypes.ParameterNotFound
		if errors.As(err, &missing) || absentMessage(err) {
			return nil
		}
		return fmt.Errorf("delete parameter %s: %w", name, err)
	}
	return nil
}

// scrubbed is an error whose message has had a known secret removed but which
// still unwraps to the cause. Replacing the error outright would lose the SDK's
// typed error and cost every caller its errors.As; mutating a shared error is
// not possible. So the message is replaced and the cause is kept.
type scrubbed struct {
	msg string
	err error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.err }

// scrub removes every occurrence of secret from err's message. A value short
// enough to appear in unrelated text is left alone: replacing a two-character
// password everywhere would turn the message into nonsense without making it
// safer.
func scrub(err error, secret string) error {
	if err == nil || secret == "" || len(secret) < 8 || !strings.Contains(err.Error(), secret) {
		return err
	}
	return &scrubbed{msg: strings.ReplaceAll(err.Error(), secret, "***"), err: err}
}

// absentMessage recognises "there is no such thing" from services that answer a
// missing resource with a generic error rather than a modelled one. It is only
// used where the caller has already established it is talking about a name, and
// it is checked after the modelled type, so a modelled error never reaches it.
func absentMessage(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "parameter not found") ||
		strings.Contains(m, "does not exist") ||
		strings.Contains(m, "not found")
}
