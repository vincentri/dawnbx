package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dawnbx/internal/provider"
)

// Every test in this package runs against an httptest server. Nothing here
// reaches AWS, and the harness below is what makes that true: it is the only way
// an AWS client is ever constructed, and the endpoint it points at is the one the
// test owns.

const testAccount = "123456789012"

// fake is a stand-in for the four AWS services this adapter uses. It is not a
// simulator: a test decides, per action, exactly what the service answers, and
// the harness only knows the wire formats and remembers what was asked.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	seen   []string
	bodies []string
	reply  func(action string, body []byte) (status int, payload string)
}

// newFake starts a server that answers STS with a fixed account and everything
// else with the test's reply function. A test about the identity call itself
// uses newRudeFake, which answers that one too.
func newFake(t *testing.T, reply func(action string, body []byte) (int, string)) *fake {
	t.Helper()
	return newRudeFake(t, func(action string, body []byte) (int, string) {
		if action == "GetCallerIdentity" {
			return 0, stsBody
		}
		return reply(action, body)
	})
}

// newRudeFake answers every action, including STS, with the test's own reply.
func newRudeFake(t *testing.T, reply func(action string, body []byte) (int, string)) *fake {
	t.Helper()
	f := &fake{t: t, reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// stsBody is a well-formed GetCallerIdentity reply; the account it carries is
// what New records and what the tests below compare against.
const stsBody = `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">` +
	`<GetCallerIdentityResult><Arn>arn:aws:iam::` + testAccount + `:user/dawnbx</Arn>` +
	`<UserId>AIDAEXAMPLE</UserId><Account>` + testAccount + `</Account></GetCallerIdentityResult>` +
	`<ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read body: %v", err)
		return
	}
	action := r.Header.Get("X-Amz-Target")
	if action == "" {
		// The query protocols (CloudFormation, EC2, STS) put the operation in
		// the form body.
		form, err := url.ParseQuery(string(body))
		if err != nil {
			f.t.Errorf("parse form: %v", err)
			return
		}
		action = form.Get("Action")
	}
	f.mu.Lock()
	f.seen = append(f.seen, action)
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()

	status, payload := f.reply(action, body)
	if status == 0 {
		status = http.StatusOK
	}
	if strings.HasPrefix(payload, "{") {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	} else {
		w.Header().Set("Content-Type", "text/xml")
	}
	w.WriteHeader(status)
	fmt.Fprint(w, payload)
}

// query wraps inner in the response envelope the awsquery protocol uses. EC2 and
// CloudFormation are both query, and both wrap their payload in <Action>Result.
func query(action, inner string) string {
	return fmt.Sprintf(`<%sResponse xmlns="urn:test"><%sResult>%s</%sResult>`+
		`<ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata></%sResponse>`,
		action, action, inner, action, action)
}

// ec2xml wraps inner in the single element the EC2 query protocol reads from.
// EC2 differs from CloudFormation twice over and both differences are silent: the
// decoder starts at the root and walks its children rather than unwrapping a
// per-operation Result node, and a list's members are <item> rather than
// <member>. Get either wrong and the reply parses to an empty result with no
// error at all, which is a test that passes without having reached the code it
// means to.
func ec2xml(inner string) string {
	return `<Response xmlns="urn:test">` + inner + `<requestID>req-1</requestID></Response>`
}

// gone answers the way CloudFormation answers for a stack it has never heard of:
// a 400 with a generic code and the words in the message. It is not a modelled
// error, and a test that only passed against a modelled one would be testing
// nothing.
func gone(stack string) (int, string) {
	return http.StatusBadRequest, `<ErrorResponse><Error><Code>ValidationError</Code>` +
		`<Message>Stack with id ` + stack + ` does not exist</Message></Error>` +
		`<RequestId>req-1</RequestId></ErrorResponse>`
}

// newAWSOptions is the harness's adapter configuration, pointed at f's server.
// Separate from newAWS so a test can drive New itself and see the error.
func newAWSOptions(t *testing.T, f *fake) Options {
	t.Helper()
	testEnv(t)
	return Options{
		Region:       "eu-west-1",
		ReleaseURL:   "https://releases.example.com/v1.0.0",
		Template:     "AWSTemplateFormatVersion: \"2010-09-09\"\n",
		KeyName:      "rescue",
		SSHCIDR:      "203.0.113.7/32",
		HTTPClient:   f.srv.Client(),
		BaseEndpoint: f.srv.URL,
		Now:          advancingClock(),
		Poll:         func(context.Context, time.Duration) error { return nil },
	}
}

// newAWS builds the adapter against f, with the clock and the sleep replaced so a
// teardown wait costs nothing.
func newAWS(t *testing.T, f *fake, mutate func(*Options)) *AWS {
	t.Helper()
	o := newAWSOptions(t, f)
	if mutate != nil {
		mutate(&o)
	}
	p, err := New(context.Background(), o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, ok := p.(*AWS)
	if !ok {
		t.Fatalf("New returned %T, not *AWS", p)
	}
	return a
}

// testEnv gives the credential chain exactly one source and shuts the others
// off. A test that leaves IMDS enabled is a test that reaches for 169.254.169.254
// and hopes nothing answers, and a test that reads the developer's own ~/.aws is
// a test that passes on one machine and fails on the next.
func testEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
}

// advancingClock is a clock that moves a minute every time it is read, so a
// bounded wait really does end without a test taking a minute.
func advancingClock() func() time.Time {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Minute)
		return now
	}
}

// called fails when one of these actions never happened.
func (f *fake) called(want ...string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var missing []string
	for _, w := range want {
		if !contains(f.seen, w) {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		f.t.Errorf("adapter never called %v; it called %v", missing, f.seen)
	}
}

// notCalled fails when an action was made anyway. It is how the ordering claims
// are checked: the secret is written before the stack is asked for, and nothing
// is asked for after a create has failed.
func (f *fake) notCalled(action string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := indexOf(f.seen, action); i >= 0 {
		f.t.Errorf("unexpected %s at step %d, after %v", action, i, f.seen[:i])
	}
}

func (f *fake) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// bodyOf is the request body of the first call to action, which is how a test
// asserts what the adapter actually sent rather than what it meant to.
func (f *fake) bodyOf(action string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.seen {
		if a == action {
			return f.bodies[i]
		}
	}
	return ""
}

func indexOf(vs []string, v string) int {
	for i, s := range vs {
		if s == v {
			return i
		}
	}
	return -1
}

func isUnavailable(err error) bool { return errors.Is(err, provider.ErrUnavailable) }

// testContext is the context every test passes: there is no deadline anywhere in
// this package, because every wait in it is bounded by the adapter's own clock
// and a context deadline on top would make a failure look like a timeout.
func testContext(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// jsonBody decodes a request body for the tests that have to read the filters
// the adapter sent before they can answer them.
func jsonBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode request %q: %v", b, err)
	}
	return m
}

// The adapter is a provider because the assertion in aws.go compiles; what this
// test adds is that New hands back that type and not something else.
func TestAWSImplementsProvider(t *testing.T) {
	var _ provider.Provider = (*AWS)(nil)
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	if a := newAWS(t, f, nil); a.ID() != "aws" {
		t.Errorf("ID() = %q, want %q", a.ID(), "aws")
	}
}

// Capabilities is what a dashboard promises an operator, so its three fields are
// checked by value: available, the delivery mechanism in the adapter's own
// words, and a non-empty region list.
func TestCapabilities(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	c := newAWS(t, f, nil).Capabilities()
	if !c.Available {
		t.Error("Available = false: the adapter exists because credentials resolved")
	}
	if c.Delivery != "aws-ssm-securestring" {
		t.Errorf("Delivery = %q, want the SSM SecureString", c.Delivery)
	}
	if len(c.Regions) == 0 || !contains(c.Regions, "eu-west-1") {
		t.Errorf("Regions = %v, want the configured region among them", c.Regions)
	}
	// A caller that sorts the list in place must not be able to reorder what the
	// next caller is promised.
	c.Regions[0] = "mutated"
	if again := newAWS(t, f, nil).Capabilities(); again.Regions[0] == "mutated" {
		t.Error("Capabilities handed out the adapter's own slice")
	}
}

// A control plane with no AWS configuration must say so at startup, once, with
// the same error for every missing piece, and must never open the rescue path to
// the world because the operator forgot a flag.
func TestNewRefusesUnusableConfiguration(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*Options)
	}{
		{"no region", func(o *Options) { o.Region = "" }},
		{"no release url", func(o *Options) { o.ReleaseURL = "" }},
		{"no template", func(o *Options) { o.Template = "" }},
		{"no key pair", func(o *Options) { o.KeyName = "" }},
		{"no rescue cidr", func(o *Options) { o.SSHCIDR = "" }},
		{"unknown region", func(o *Options) { o.Region = "mars-central-1" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := Options{Region: "eu-west-1", ReleaseURL: "u", Template: "t", KeyName: "k", SSHCIDR: "1.2.3.4/32"}
			c.mutate(&o)
			if _, err := New(context.Background(), o); !isUnavailable(err) {
				t.Errorf("New(%s) error = %v, want ErrUnavailable", c.name, err)
			}
		})
	}
}

// New asks STS who it is before it hands back a provider, so a credential that
// cannot answer is refused at startup rather than at the first create. The
// refusal has to be provider.ErrUnavailable — the answer every route above
// understands — and it must not carry the credential that failed.
//
// newRudeFake, not newFake: newFake answers GetCallerIdentity itself, so the
// 403 written here never reached STS and this test passed without ever seeing
// the failure it names.
func TestNewProvesCredentialsAtStartup(t *testing.T) {
	f := newRudeFake(t, func(string, []byte) (int, string) {
		return http.StatusForbidden, `<ErrorResponse><Error><Code>AccessDenied</Code>` +
			`<Message>User is not authorized to perform sts:GetCallerIdentity</Message></Error>` +
			`<RequestId>req-1</RequestId></ErrorResponse>`
	})
	_, err := New(context.Background(), newAWSOptions(t, f))
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("a credential that cannot answer STS: got %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), testAccount) {
		t.Errorf("the refusal carries the account it was checking: %v", err)
	}
	f.called("GetCallerIdentity")
}

// Create writes the secret before it asks for the stack, and the stack's
// parameters carry the parameter's name and never its value.
func TestCreateStoresSecretThenAsksForStack(t *testing.T) {
	const password = "correct horse battery staple"
	f := newFake(t, func(action string, _ []byte) (int, string) {
		switch action {
		case "AmazonSSM.PutParameter":
			return 200, `{"Version":1}`
		case "CreateStack":
			return 200, query("CreateStack", "")
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	spec := provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30, Domain: "team.example.com"}

	h, err := a.Create(context.Background(), spec, provider.Bootstrap{AdminPassword: password})
	if err != nil {
		t.Fatal(err)
	}
	// The identity call belongs to New; what Create adds is these two, in order.
	if got := f.order()[1:]; len(got) != 2 || got[0] != "AmazonSSM.PutParameter" || got[1] != "CreateStack" {
		t.Errorf("call order = %v, want the secret written before the stack is asked for", got)
	}
	body := f.bodyOf("CreateStack")
	if !strings.Contains(body, "BootstrapParameter") {
		t.Errorf("create did not pass the parameter name: %s", body)
	}
	if strings.Contains(body, password) {
		t.Error("the create request carried the admin password")
	}
	// The handle is what the control plane stores and hands back verbatim, so it
	// has to survive the round trip with the stack and the parameter in it.
	ph, err := fromHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	if ph.Stack == "" || ph.Parameter != "/dawnbx/bootstrap/"+ph.Stack || ph.Region != "eu-west-1" {
		t.Errorf("handle = %+v", ph)
	}
	if ph.URL != "https://team.example.com" {
		t.Errorf("handle URL = %q", ph.URL)
	}
}

// A create that fails must not leave a stack or a SecureString behind, and must
// not say anything an operator could paste into a ticket.
func TestCreateFailureLeavesNothingBehind(t *testing.T) {
	const password = "correct horse battery staple"
	f := newFake(t, func(action string, _ []byte) (int, string) {
		switch action {
		case "AmazonSSM.PutParameter":
			return 200, `{"Version":1}`
		case "CreateStack":
			return http.StatusBadRequest, `<ErrorResponse><Error><Code>ValidationError</Code>` +
				`<Message>Parameters: [BootstrapParameter] must have values</Message></Error></ErrorResponse>`
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	spec := provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30}

	_, err := a.Create(context.Background(), spec, provider.Bootstrap{AdminPassword: password})
	if err == nil {
		t.Fatal("Create succeeded against a refusing CloudFormation")
	}
	f.called("AmazonSSM.DeleteParameter", "DeleteStack")
	if strings.Contains(err.Error(), password) {
		t.Errorf("error leaks the admin password: %v", err)
	}
}

// The other half of provider neutrality: this package is the only place an AWS
// SDK may appear, and it is also the only place that may know one. It may import
// the SDK, the standard library and the neutral interface, and nothing else — in
// particular not internal/cluster or internal/api, which sit above it and would
// make a cycle the moment a second provider needed either. The neutral package
// has the mirror-image test for the leak that goes the other way.
func TestAdapterImportsNothingAboveProvider(t *testing.T) {
	allowed := map[string]bool{
		"dawnbx/internal/provider": true,
	}
	fset := token.NewFileSet()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			switch {
			case strings.HasPrefix(p, "github.com/aws/aws-sdk-go-v2"):
			case allowed[p]:
			case strings.Contains(p, "dawnbx/internal"):
				t.Errorf("%s imports %s: the adapter may only know the neutral interface", name, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no files checked: the walk is wrong, not the package")
	}
}

// A retried create for the same domain is the same cluster, because the domain
// names the stack. CloudFormation refusing a second stack is the answer a retry
// needs, and turning it into a teardown would destroy a cluster that is halfway
// up.
func TestCreateRetryIsNotASecondClusterNorATeardown(t *testing.T) {
	const password = "correct horse battery staple"
	f := newFake(t, func(action string, _ []byte) (int, string) {
		switch action {
		case "AmazonSSM.PutParameter":
			return 200, `{"Version":1}`
		case "CreateStack":
			return http.StatusBadRequest, `<ErrorResponse><Error><Code>AlreadyExistsException</Code>` +
				`<Message>Stack [dawnbx-team-example-com] already exists</Message></Error></ErrorResponse>`
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	spec := provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30, Domain: "team.example.com"}

	h, err := a.Create(testContext(t), spec, provider.Bootstrap{AdminPassword: password})
	if err != nil {
		t.Fatalf("a retry was treated as a failure: %v", err)
	}
	ph, err := fromHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	if ph.Stack != "dawnbx-team-example-com" {
		t.Errorf("stack = %q", ph.Stack)
	}
	// Nothing was torn down: the cluster that is already coming up still exists.
	f.notCalled("DeleteStack")
	f.notCalled("AmazonSSM.DeleteParameter")
}

// TestDestroyWaitsForTheStackToBeGone: a DeleteStack call that returns is a
// request accepted, not a teardown finished. FR-018 promises the record is kept
// until the provider confirms the resources are gone, and Forget follows this
// call — so a caller that returned at the acknowledgement would drop the only
// handle on a stack that is still being deleted.
func TestDestroyWaitsForTheStackToBeGone(t *testing.T) {
	var described int
	f := newRudeFake(t, func(action string, _ []byte) (int, string) {
		switch action {
		case "GetCallerIdentity":
			return 200, stsBody
		case "DeleteStack":
			return 200, query("DeleteStack", "")
		case "DescribeStacks":
			// The stack is still visible for the first two readings, then gone.
			described++
			if described <= 2 {
				return 200, query("DescribeStacks", "<Stacks><member><StackName>s</StackName>"+
					"<StackStatus>DELETE_IN_PROGRESS</StackStatus></member></Stacks>")
			}
			return gone("s")
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	if err := a.Destroy(testContext(t), provider.NewHandle([]byte(
		`{"stack":"s","region":"eu-west-1","parameter":"/dawnbx/p"}`))); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if described < 3 {
		t.Errorf("Destroy returned after %d reads; it must wait until the stack is gone", described)
	}
}

// When the create fails, Create compensates by deleting the secret and the
// stack. Those two calls used to have their errors dropped, so a compensation
// that also failed left an instance running that nothing named: the handle is
// never returned, the caller only sees the create error, and there is no record
// anywhere pointing at what is now billing. The failure has to travel with the
// one the caller already has.
func TestAFailedCreateReportsItsOwnFailedCompensation(t *testing.T) {
	f := newFake(t, func(action string, _ []byte) (int, string) {
		switch action {
		case "AmazonSSM.PutParameter":
			return 200, `{"Version":1}`
		case "CreateStack":
			return 400, `<ErrorResponse><Error><Code>ValidationError</Code>` +
				`<Message>Stack creation failed</Message></ErrorResponse>`
		case "DeleteStack":
			return 403, `<ErrorResponse><Error><Code>AccessDenied</Code>` +
				`<Message>not permitted to delete</Message></ErrorResponse>`
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	spec := provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30, Domain: "team.example.com"}

	_, err := a.Create(context.Background(), spec, provider.Bootstrap{AdminPassword: secret})
	if err == nil {
		t.Fatal("a refused create reported success")
	}
	// The operations, not the service's prose: what an operator needs is the
	// name of the call that failed, so a retry or a cleanup knows what to aim
	// at, and the AWS message text is not part of any contract here.
	for _, want := range []string{"CreateStack", "DeleteStack"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s, so the orphaned stack is undiscoverable: %v", want, err)
		}
	}
}
