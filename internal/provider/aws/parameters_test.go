package aws

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

const secret = "correct horse battery staple"

// newStore builds a parameter store against f.
func newStore(t *testing.T, f *fake) parameters {
	t.Helper()
	a := newAWS(t, f, nil)
	c, err := a.forRegion("eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	return parametersFor(c.ssm)
}

// A bootstrap credential is a SecureString or it is nothing: the type is the
// control that keeps it out of a create request, out of a log line and out of
// anything that reads a parameter store.
func TestPutSecretIsASecureString(t *testing.T) {
	f := newFake(t, func(action string, _ []byte) (int, string) {
		if action != "AmazonSSM.PutParameter" {
			t.Errorf("unexpected %s", action)
		}
		return 200, `{"Version":1}`
	})
	if err := newStore(t, f).putSecret(testContext(t), "/dawnbx/bootstrap/s1", secret); err != nil {
		t.Fatal(err)
	}
	body := f.bodyOf("AmazonSSM.PutParameter")
	if !strings.Contains(body, `"Type":"SecureString"`) {
		t.Errorf("parameter is not a SecureString: %s", body)
	}
	// Overwrite is the rotation path: the same name is written again.
	if !strings.Contains(body, `"Overwrite":true`) {
		t.Errorf("rotation would create a second parameter instead of replacing it: %s", body)
	}
}

// A name that is not there is not a transport failure: Destroy is called twice,
// and the second call is being asked to confirm what the first one did, so an
// absent parameter is the state it is being asked about rather than an error.
func TestDeletingAnAbsentParameterIsSuccess(t *testing.T) {
	notFound := http.StatusBadRequest
	f := newFake(t, func(string, []byte) (int, string) {
		return notFound, `{"__type":"ParameterNotFound","message":"parameter not found"}`
	})
	ps := newStore(t, f)
	ctx := testContext(t)

	if err := ps.deleteSecret(ctx, "/dawnbx/bootstrap/gone"); err != nil {
		t.Errorf("delete of an absent parameter: %v, want nil", err)
	}
}

// The one rule this file exists for: the value never appears in a formatted
// error. The fake below quotes the whole request back in its failure, which is
// what a debug proxy or a misconfigured endpoint does, and the value must still
// be gone by the time the error reaches a log.
func TestSecretNeverAppearsInAnError(t *testing.T) {
	f := newFake(t, func(action string, body []byte) (int, string) {
		return http.StatusInternalServerError, `{"__type":"InternalServerError","message":"rejected request: ` +
			strings.ReplaceAll(string(body), `"`, `'`) + `"}`
	})
	err := newStore(t, f).putSecret(testContext(t), "/dawnbx/bootstrap/s1", secret)
	if err == nil {
		t.Fatal("putSecret succeeded against a failing SSM")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error carries the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("the value should have been replaced, not merely dropped: %v", err)
	}
	// The name is safe and useful, and the cause is still there for errors.As.
	if !strings.Contains(err.Error(), "/dawnbx/bootstrap/s1") {
		t.Errorf("the error does not say which parameter: %v", err)
	}
	var api interface{ ErrorCode() string }
	if !errors.As(err, &api) || api.ErrorCode() != "InternalServerError" {
		t.Errorf("the wrapped cause was lost: %v", err)
	}
}

// scrub is what makes that possible, and the short-value rule matters: a
// two-character secret is not scrubbed, because replacing every occurrence of it
// would mangle the message without making it safer.
func TestScrub(t *testing.T) {
	cause := errors.New("request body contained " + secret)
	got := scrub(cause, secret)
	if strings.Contains(got.Error(), secret) {
		t.Errorf("not scrubbed: %v", got)
	}
	if !errors.Is(got, cause) {
		t.Error("scrubbing lost the cause")
	}
	if scrub(cause, "") != cause || scrub(cause, "ab") != cause || scrub(nil, secret) != nil {
		t.Error("scrub touched something it should not have")
	}
}
