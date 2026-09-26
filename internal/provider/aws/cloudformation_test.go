package aws

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"dawnbx/internal/provider"
)

// A stateful CloudFormation, so a test can say "the stack is rolling back" and
// then watch what the adapter does about it.
type cfn struct {
	t *testing.T
	f *fake

	mu      sync.Mutex
	status  string
	reason  string
	outputs map[string]string
	events  string
	exists  bool
	calls   []string
}

func newCFN(t *testing.T) *cfn {
	c := &cfn{t: t, status: "CREATE_IN_PROGRESS", exists: true}
	c.f = newFake(t, c.reply)
	return c
}

func (c *cfn) reply(action string, _ []byte) (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, action)
	switch action {
	case "DescribeStacks":
		if !c.exists {
			return gone("dawnbx-s1")
		}
		var outs strings.Builder
		for _, k := range []string{outURL, outPublicIP, outSecurity, outLaunchTpl} {
			if v, ok := c.outputs[k]; ok {
				fmt.Fprintf(&outs, "<member><OutputKey>%s</OutputKey><OutputValue>%s</OutputValue></member>", k, v)
			}
		}
		reason := ""
		if c.reason != "" {
			reason = "<StackStatusReason>" + c.reason + "</StackStatusReason>"
		}
		return 200, query("DescribeStacks", "<Stacks><member><StackName>dawnbx-s1</StackName>"+
			"<StackStatus>"+c.status+"</StackStatus>"+reason+
			"<Outputs>"+outs.String()+"</Outputs></member></Stacks>")
	case "DescribeStackEvents":
		return 200, query("DescribeStackEvents", "<StackEvents>"+c.events+"</StackEvents>")
	case "DeleteStack":
		// A stack mid-rollback cannot be deleted; CloudFormation says so. The
		// adapter must not ask, and the test notices if it does.
		if c.status == "ROLLBACK_IN_PROGRESS" {
			return http.StatusBadRequest, `<ErrorResponse><Error><Code>ValidationError</Code>` +
				`<Message>Cannot delete a stack while it is ROLLBACK_IN_PROGRESS</Message></Error></ErrorResponse>`
		}
		c.exists = false
		return 200, query("DeleteStack", "")
	}
	return 200, "{}"
}

func (c *cfn) set(status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

func (c *cfn) event(id, status, reason, physical string) string {
	r := ""
	if reason != "" {
		r = "<ResourceStatusReason>" + reason + "</ResourceStatusReason>"
	}
	p := ""
	if physical != "" {
		p = "<PhysicalResourceId>" + physical + "</PhysicalResourceId>"
	}
	return "<member><EventId>e</EventId><StackId>arn</StackId><StackName>dawnbx-s1</StackName>" +
		"<Timestamp>2026-01-01T00:00:00Z</Timestamp><LogicalResourceId>" + id + "</LogicalResourceId>" +
		"<ResourceType>AWS::EC2::Instance</ResourceType><ResourceStatus>" + status + "</ResourceStatus>" +
		r + p + "</member>"
}

func (c *cfn) did(action string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return contains(c.calls, action)
}

func (c *cfn) handle() provider.Handle {
	h, err := toHandle(handle{Stack: "dawnbx-s1", Parameter: "/dawnbx/bootstrap/s1", Region: "eu-west-1"})
	if err != nil {
		c.t.Fatal(err)
	}
	return h
}

// A stack's status maps onto the neutral state machine, and the mapping is
// checked for every state this build knows about. The two that are easy to get
// wrong are the ones with a comment in the code: a stack mid-rollback is the
// adapter's own teardown in flight, and a stack that is gone is a normal end of
// life rather than an error.
func TestStackStatusMapsToProviderState(t *testing.T) {
	for _, c := range []struct {
		stack  string
		events string
		want   provider.State
		url    string
	}{
		{stack: "CREATE_IN_PROGRESS", want: provider.Creating},
		{stack: "CREATE_IN_PROGRESS", events: "host", want: provider.Bootstrapping},
		{stack: "REVIEW_IN_PROGRESS", want: provider.Creating},
		{stack: "CREATE_COMPLETE", want: provider.Ready, url: "https://team.example.com"},
		{stack: "DELETE_IN_PROGRESS", want: provider.Gone},
		{stack: "DELETE_COMPLETE", want: provider.Gone},
		// A rollback is the adapter's own teardown: it is reported as still
		// working, because a state that meant "my cloud is cleaning up" would be
		// a branch the orchestrator would have to know about.
		{stack: "ROLLBACK_IN_PROGRESS", want: provider.Bootstrapping},
		{stack: "UPDATE_ROLLBACK_IN_PROGRESS", want: provider.Bootstrapping},
		// A status this build has never seen is a stack that has not finished.
		{stack: "SOMETHING_NEW", want: provider.Creating},
	} {
		t.Run(c.stack+" "+string(c.want), func(t *testing.T) {
			cf := newCFN(t)
			cf.set(c.stack)
			cf.outputs = map[string]string{outURL: "https://team.example.com", outPublicIP: "203.0.113.7"}
			if c.events == "host" {
				cf.events = cf.event(serverID, "CREATE_COMPLETE", "", "i-1")
			}
			got, err := newAWS(t, cf.f, nil).Status(testContext(t), cf.handle())
			if err != nil {
				t.Fatal(err)
			}
			if got.State != c.want {
				t.Errorf("state = %q, want %q", got.State, c.want)
			}
			if got.URL != c.url {
				t.Errorf("url = %q, want %q", got.URL, c.url)
			}
		})
	}
}

// A stack CloudFormation has never heard of is Gone, not an error and not a
// transport failure. The distinction is destructive if it is wrong: Gone is what
// makes the caller delete a cluster.
func TestAbsentStackIsGone(t *testing.T) {
	cf := newCFN(t)
	cf.exists = false
	got, err := newAWS(t, cf.f, nil).Status(testContext(t), cf.handle())
	if err != nil {
		t.Fatalf("a cluster that is gone is not an error: %v", err)
	}
	if got.State != provider.Gone {
		t.Errorf("state = %q, want %q", got.State, provider.Gone)
	}
}

// A transport failure is not "gone". Getting this wrong destroys a live cluster,
// so the test refuses to be satisfied by a message that merely looks like one.
func TestUnreachableCloudFormationIsNotGone(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) {
		return http.StatusBadRequest, `<ErrorResponse><Error><Code>Throttling</Code>` +
			`<Message>Rate exceeded</Message></Error></ErrorResponse>`
	})
	_, err := newAWS(t, f, nil).Status(testContext(t), provider.NewHandle([]byte(`{"stack":"s","parameter":"p","region":"eu-west-1"}`)))
	if err == nil {
		t.Fatal("a throttled CloudFormation reported no error at all")
	}
	if strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a throttle was read as an absent stack: %v", err)
	}
}

// The install's own verdict reaches the operator through Reason, and nothing else
// does: the wait condition's text is attacker-influenced, it goes to a dashboard
// and an audit log, and it is sanitised on the way.
func TestWaitConditionFailureBecomesASanitisedReason(t *testing.T) {
	cf := newCFN(t)
	cf.set("CREATE_FAILED")
	cf.reason = "The following resource(s) failed to create: [Server]."
	cf.events = cf.event(readyWaitID, "FAILED",
		"checksum mismatch for dawnbx-server-linux-arm64\nADMIN_PASSWORD=hunter2hunter2\nsee https://releases.example.com/v1.0.0/checksums.txt", "")

	got, err := newAWS(t, cf.f, nil).Status(testContext(t), cf.handle())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provider.Failed {
		t.Fatalf("state = %q, want %q", got.State, provider.Failed)
	}
	if !strings.Contains(got.Reason, "checksum mismatch") {
		t.Errorf("the install's own reason was lost: %q", got.Reason)
	}
	if strings.Contains(got.Reason, "hunter2hunter2") {
		t.Errorf("the reason carries an assignment's value: %q", got.Reason)
	}
	if strings.Contains(got.Reason, "\n") {
		t.Errorf("the reason is multi-line, which no table row can hold: %q", got.Reason)
	}
	// The stack's own reason is the fallback when the install said nothing. This
	// is a second stack, because the first one's teardown has already happened.
	cf2 := newCFN(t)
	cf2.set("CREATE_FAILED")
	cf2.reason = "The following resource(s) failed to create: [Server]."
	got, _ = newAWS(t, cf2.f, nil).Status(testContext(t), cf2.handle())
	if got.State != provider.Failed || !strings.Contains(got.Reason, "failed to create") {
		t.Errorf("without an install reason, state = %q reason = %q", got.State, got.Reason)
	}
}

// The rollback case the whole file exists for. A stack that is already rolling
// back is being undone by CloudFormation on its own, so the adapter waits for it
// rather than asking for a second delete — and a delete during a rollback is
// refused, which the fake reports — and it never reports the rollback to the
// caller.
func TestRollbackInProgressIsTheAdaptersOwnBusiness(t *testing.T) {
	cf := newCFN(t)
	cf.set("ROLLBACK_IN_PROGRESS")
	a := newAWS(t, cf.f, nil)

	got, err := a.Status(testContext(t), cf.handle())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provider.Bootstrapping || got.Reason != "" {
		t.Fatalf("a rollback was reported to the caller: %+v", got)
	}
	if cf.did("DeleteStack") {
		t.Error("asked CloudFormation to delete a stack it is already rolling back")
	}
}

// When the rollback finishes, the adapter finishes the job itself: it deletes the
// stack, waits until the stack is really gone, and only then says Failed. A
// caller that saw Failed while the teardown was still running would run its own
// cleanup against resources that were on their way out.
func TestTerminalFailureSettlesTeardownBeforeReportingFailed(t *testing.T) {
	cf := newCFN(t)
	cf.set("ROLLBACK_COMPLETE")
	cf.reason = "Resource creation cancelled"
	a := newAWS(t, cf.f, nil)

	got, err := a.Status(testContext(t), cf.handle())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provider.Failed {
		t.Fatalf("state = %q, want %q", got.State, provider.Failed)
	}
	if !cf.did("DeleteStack") {
		t.Error("a rolled-back stack was left behind")
	}
	// DeleteStack removed it, and Failed was only returned once DescribeStacks
	// said so — the fake refuses to report a deleted stack.
	if cf.exists {
		t.Error("Failed was reported over a stack that still existed")
	}
	if strings.Contains(got.Reason, "teardown incomplete") {
		t.Errorf("a teardown that completed is reported as incomplete: %q", got.Reason)
	}
}

// A teardown that cannot finish must still say the cluster failed, and must say
// so in a way an operator can act on. Returning an error instead would leave the
// orchestrator polling a cluster that will never come up.
func TestTeardownThatCannotFinishIsReportedNotRetried(t *testing.T) {
	cf := newCFN(t)
	cf.set("CREATE_FAILED")
	cf.reason = "no capacity"
	// DeleteStack succeeds but the stack keeps being there, which is what a
	// stuck EIP attachment looks like.
	cf.f = newFake(t, func(action string, _ []byte) (int, string) {
		if action == "DeleteStack" {
			return 200, query("DeleteStack", "")
		}
		return cf.reply(action, nil)
	})
	a := newAWS(t, cf.f, nil)

	got, err := a.Status(testContext(t), cf.handle())
	if err != nil {
		t.Fatalf("a cluster that failed must not turn into a poll loop: %v", err)
	}
	if got.State != provider.Failed {
		t.Fatalf("state = %q, want %q", got.State, provider.Failed)
	}
	if !strings.Contains(got.Reason, "teardown incomplete") || !strings.Contains(got.Reason, "eu-west-1") {
		t.Errorf("reason does not tell the operator what is left: %q", got.Reason)
	}
}

// The wait is bounded by the adapter's own clock, and the clock is what a test
// replaces. This is the test for the bound: without it, a stuck teardown would
// hold a Status call open for as long as AWS took.
func TestSettleGivesUpAtItsDeadline(t *testing.T) {
	cf := newCFN(t)
	cf.set("CREATE_FAILED")
	// The delete is accepted and the stack is still there: a resource that will
	// not let go, which is the only way the wait can run out.
	cf.f = newFake(t, func(action string, body []byte) (int, string) {
		if action == "DeleteStack" {
			return 200, query("DeleteStack", "")
		}
		return cf.reply(action, body)
	})
	var polls int
	a := newAWS(t, cf.f, func(o *Options) {
		o.SettleTimeout = 5 * time.Minute
		o.Poll = func(context.Context, time.Duration) error { polls++; return nil }
	})
	got, err := a.Status(testContext(t), cf.handle())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provider.Failed {
		t.Errorf("state = %q", got.State)
	}
	if polls == 0 {
		t.Error("settle gave up without waiting at all")
	}
	if polls > 10 {
		t.Errorf("settle polled %d times past a 5 minute deadline", polls)
	}
}

// Destroy has to be safe to call twice: the control plane calls it again after a
// timeout, and an operator calls it after a cancel. Every step of the second call
// answers "already gone", and both calls succeed.
func TestDestroyIsIdempotent(t *testing.T) {
	cf := newCFN(t)
	cf.set("CREATE_COMPLETE")
	f := newFake(t, func(action string, body []byte) (int, string) {
		switch action {
		case "AmazonSSM.DeleteParameter":
			if strings.Contains(string(body), "gone") {
				return http.StatusBadRequest, `{"__type":"ParameterNotFound","message":"parameter not found"}`
			}
			return 200, "{}"
		}
		return cf.reply(action, body)
	})
	a := newAWS(t, f, nil)
	h := cf.handle()

	if err := a.Destroy(testContext(t), h); err != nil {
		t.Fatal(err)
	}
	// Second call: the stack is already gone and so is the parameter.
	if err := a.Destroy(testContext(t), h); err != nil {
		t.Fatalf("the second Destroy failed: %v", err)
	}
	if _, err := a.Status(testContext(t), h); err != nil {
		t.Fatal(err)
	}
	// The stack goes first: deleting the credential first would leave an
	// instance that boots without one.
	got := f.order()
	delStack, delParam := indexOf(got, "DeleteStack"), indexOf(got, "AmazonSSM.DeleteParameter")
	if delStack < 0 || delParam < 0 || delStack > delParam {
		t.Errorf("destroy order = %v, want the stack before the parameter", got)
	}
}

// SetBootstrap is a rotation: the same parameter name, written again, and nothing
// about the host or the stack changes.
func TestSetBootstrapRewritesTheSameParameter(t *testing.T) {
	cf := newCFN(t)
	const rotated = "a different long password"
	if err := newAWS(t, cf.f, nil).SetBootstrap(testContext(t), cf.handle(), provider.Bootstrap{AdminPassword: rotated}); err != nil {
		t.Fatal(err)
	}
	body := cf.f.bodyOf("AmazonSSM.PutParameter")
	if !strings.Contains(body, "/dawnbx/bootstrap/s1") {
		t.Errorf("rotated into a new parameter instead of the existing one: %s", body)
	}
	if !strings.Contains(body, `"Overwrite":true`) {
		t.Errorf("rotation did not overwrite: %s", body)
	}
	if !strings.Contains(body, rotated) {
		t.Error("the rotated value was not sent")
	}
	cf.f.notCalled("CreateStack")
	cf.f.notCalled("UpdateStack")
}

// The create posture is what makes the failure readable: rollback off, so a
// CREATE_FAILED keeps its reason and its resources for the adapter to settle.
func TestCreateStackDisablesRollback(t *testing.T) {
	cf := newCFN(t)
	a := newAWS(t, cf.f, nil)
	h := handle{Stack: "dawnbx-s1", Parameter: "/dawnbx/bootstrap/s1", Region: "eu-west-1"}
	c, err := a.forRegion("eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.createStack(testContext(t), c, provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30}, h); err != nil {
		t.Fatal(err)
	}
	body := cf.f.bodyOf("CreateStack")
	if !strings.Contains(body, "DisableRollback=true") {
		t.Errorf("a failure would be rolled back before its reason could be read: %s", body)
	}
	if strings.Contains(body, "OnFailure") {
		t.Errorf("OnFailure would delete the stack out from under the poll: %s", body)
	}
	// The token is a digest: the API caps it at 128 characters and the parameter
	// list is longer than that for every configuration.
	q := form(t, body)
	if tok := q.Get("ClientRequestToken"); len(tok) != 64 {
		t.Errorf("client request token = %q, want a 64 character digest", tok)
	}
}

// A cluster created without a domain still has a url, and only the stack's
// output knows it. A handle that carries the two resource ids but no url must
// therefore still be read before a worker can be told where the cluster is.
func TestResourcesFillInAURLTheHandleNeverHad(t *testing.T) {
	cf := newCFN(t)
	cf.events = cf.event(sgID, "CREATE_COMPLETE", "", "sg-0abc") + cf.event(ltID, "CREATE_COMPLETE", "", "lt-0abc")
	cf.outputs = map[string]string{outPublicIP: "203.0.113.7"}
	a := newAWS(t, cf.f, nil)
	c, _ := a.forRegion("eu-west-1")

	got, err := a.resources(testContext(t), c, handle{Stack: "dawnbx-s1", Parameter: "p", Region: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://203.0.113.7.sslip.io" {
		t.Errorf("url = %q, want the one the stack publishes", got.URL)
	}
}

// A resource id is read out of the event list, which is the only place it exists
// before the stack finishes. A worker cannot wait for the stack.
func TestResourcesComeFromTheEventList(t *testing.T) {
	cf := newCFN(t)
	cf.events = cf.event(sgID, "CREATE_COMPLETE", "", "sg-0abc") + cf.event(ltID, "CREATE_COMPLETE", "", "lt-0abc")
	a := newAWS(t, cf.f, nil)
	c, _ := a.forRegion("eu-west-1")

	got, err := a.resources(testContext(t), c, handle{Stack: "dawnbx-s1", Parameter: "p", Region: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SecurityGroup != "sg-0abc" || got.LaunchTemplate != "lt-0abc" {
		t.Errorf("resources = %+v", got)
	}
}

// A reason is bounded and squashed, whatever the service sent. An unbounded
// reason would end up in a database column, a JSON response and a table row.
func TestSanitise(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "install failed", "install failed"},
		{"newlines", "line one\n\tline   two", "line one line two"},
		{"an assignment", "ADMIN_PASSWORD=hunter2hunter2 and ok", "ADMIN_PASSWORD="},
		{"a bearer token", "Authorization: Bearer abc123def", "Authorization:"},
		{"api key", "api_key = AKIAIOSFODNN7EXAMPLE", "api_key ="},
		{"a word that is only part of a key", "passwords are stored hashed", "passwords are stored hashed"},
	} {
		if got := sanitise(c.in); got != c.want {
			t.Errorf("%s: sanitise(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	long := strings.Repeat("x", 1000)
	if got := sanitise(long); len(got) != reasonMax+3 {
		t.Errorf("a long reason came out %d characters, want it bounded", len(got))
	}
}
