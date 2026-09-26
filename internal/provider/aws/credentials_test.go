package aws

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"dawnbx/internal/provider"
)

// load must fix the region on the config and keep the injected HTTP client, or
// the adapter cannot be pointed at a test server and a handle naming a region
// would be a request against whatever the machine's environment says.
func TestLoadFixesRegionAndClient(t *testing.T) {
	testEnv(t)

	hc := &countingClient{}
	cfg, err := load(context.Background(), "ap-southeast-2", hc)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "ap-southeast-2" {
		t.Errorf("region = %q", cfg.Region)
	}
	if cfg.HTTPClient != aws.HTTPClient(hc) { //nolint:staticcheck // the test is that the exact instance survives
		t.Errorf("the injected client was dropped: %T", cfg.HTTPClient)
	}
	if _, err := cfg.Credentials.Retrieve(context.Background()); err != nil {
		t.Errorf("credentials from the environment chain: %v", err)
	}
}

// identity is the check that turns "no AWS access" into provider.ErrUnavailable
// at startup rather than a failed create twenty minutes in. The message has to
// name the missing capability and nothing else: no key id, no secret, no
// assumed-role session, no profile contents.
func TestIdentityFailureIsUnavailableAndSaysNothingSecret(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int
		payload string
		wantIn  string
	}{
		{
			name:    "no credentials in the chain",
			status:  http.StatusForbidden,
			payload: `<ErrorResponse><Error><Code>InvalidClientTokenId</Code><Message>The security token included in the request is invalid</Message></Error></ErrorResponse>`,
			wantIn:  "InvalidClientTokenId",
		},
		{
			name:    "an expired session",
			status:  http.StatusForbidden,
			payload: `<ErrorResponse><Error><Code>ExpiredToken</Code><Message>The security token included in the request is expired</Message></Error></ErrorResponse>`,
			wantIn:  "ExpiredToken",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRudeFake(t, func(string, []byte) (int, string) { return c.status, c.payload })
			testEnv(t)
			cfg, err := load(context.Background(), "eu-west-1", f.srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			cfg.BaseEndpoint = aws.String(f.srv.URL)
			_, err = identity(context.Background(), cfg)
			if !errors.Is(err, provider.ErrUnavailable) {
				t.Fatalf("error = %v, want ErrUnavailable", err)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("error does not name the failure: %v", err)
			}
			for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI", "Bearer", "arn:aws"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error leaks %q: %v", leak, err)
				}
			}
		})
	}
}

// A credentials source that resolves but identifies nothing is still a failure:
// the adapter must not treat an empty account as a usable one.
func TestIdentityWithoutAnAccountIsUnavailable(t *testing.T) {
	f := newRudeFake(t, func(string, []byte) (int, string) {
		return 200, `<GetCallerIdentityResponse xmlns="x"><GetCallerIdentityResult><Arn>arn</Arn>` +
			`<UserId>u</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`
	})
	testEnv(t)
	cfg, err := load(context.Background(), "eu-west-1", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseEndpoint = aws.String(f.srv.URL)
	if _, err := identity(context.Background(), cfg); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable", err)
	}
}

func TestIdentityReturnsTheAccount(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	testEnv(t)
	cfg, err := load(context.Background(), "eu-west-1", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseEndpoint = aws.String(f.srv.URL)
	got, err := identity(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != testAccount {
		t.Errorf("account = %q, want %q", got, testAccount)
	}
}

// classify is what keeps a diagnostic available without the diagnostic's string,
// so it is checked against both kinds of error the chain produces.
func TestClassify(t *testing.T) {
	f := newRudeFake(t, func(string, []byte) (int, string) {
		return http.StatusForbidden, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>no</Message></Error></ErrorResponse>`
	})
	testEnv(t)
	cfg, err := load(context.Background(), "eu-west-1", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseEndpoint = aws.String(f.srv.URL)
	_, err = identity(context.Background(), cfg)
	// identity applies classify itself, so the code the service returned has to
	// be readable in the message and nowhere else.
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("error does not carry the service's code: %v", err)
	}
	if got := classify(errors.New("plain")); got != "errorString" {
		t.Errorf("classify(plain error) = %q, want its type", got)
	}
	if got := classify(nil); got != "unknown" {
		t.Errorf("classify(nil) = %q", got)
	}
}

// countingClient proves the injected client is the one that carries the request.
type countingClient struct{ calls int }

func (c *countingClient) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, errors.New("not called in this test")
}
