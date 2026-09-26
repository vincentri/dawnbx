package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"dawnbx/internal/provider"
)

// ec2 answers the three operations the adapter makes, and records what EC2 was
// actually asked for — which is the only way to test a launch, because the
// request is a query string and the interesting part of it is the launch
// template the worker inherited.
type ec2fake struct {
	t          *testing.T
	f          *fake
	cfn        *cfn
	instanceID string
	privateIP  string
	state      string
	reason     string
	gone       bool
}

// newEC2 starts one server that answers both EC2 and CloudFormation: a worker
// launch reads the stack for the launch template, so a test that faked them
// separately would be testing two servers and a seam that does not exist.
func newEC2(t *testing.T) (*ec2fake, *cfn) {
	e := &ec2fake{t: t, instanceID: "i-0abc", privateIP: "10.0.1.7", state: "running"}
	c := &cfn{t: t, status: "CREATE_COMPLETE", exists: true, outputs: map[string]string{outURL: "https://team.example.com"}}
	e.cfn = c
	e.f = newFake(t, e.reply)
	c.f = e.f
	return e, c
}

func (e *ec2fake) reply(action string, _ []byte) (int, string) {
	switch action {
	case "RunInstances":
		return 200, ec2xml("<reservationId>r-1</reservationId><instancesSet><item>" +
			"<instanceId>" + e.instanceID + "</instanceId><privateIpAddress>10.0.1.7</privateIpAddress>" +
			"</item></instancesSet>")
	case "DescribeInstances":
		if e.gone {
			return http.StatusBadRequest, `<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code>` +
				`<Message>The instance ID does not exist</Message></Error></Errors>` +
				`<RequestID>req-1</RequestID></Response>`
		}
		reason := ""
		if e.reason != "" {
			reason = "<stateReason><message>" + e.reason + "</message></stateReason>"
		}
		return 200, ec2xml("<reservationSet><item><reservationId>r-1</reservationId>" +
			"<instancesSet><item><instanceId>" + e.instanceID + "</instanceId>" +
			// The address too, because the node-address lookup reads this same
			// reply and a fake that omits it cannot answer the question the
			// lookup is asked.
			"<privateIpAddress>" + e.privateIP + "</privateIpAddress>" +
			"<instanceState><code>16</code><name>" + e.state + "</name></instanceState>" + reason +
			"</item></instancesSet></item></reservationSet>")
	case "DescribeInstanceAddresses":
		return 200, ec2xml("<addressesSet/>")
	case "TerminateInstances":
		return 200, ec2xml("<instancesSet><item><instanceId>" + e.instanceID +
			"</instanceId><currentState><code>32</code><name>shutting-down</name></currentState>" +
			"</item></instancesSet>")
	}
	// Anything that is not EC2 is CloudFormation: a worker launch reads the
	// stack first.
	return e.cfn.reply(action, nil)
}

// A worker is the server's own launch template, which is what carries IMDSv2 with
// a hop limit of one and the instance profile that can read the bootstrap
// parameter. A worker built any other way would be a weaker host than the one the
// operator priced, so the request is read back and checked field by field.
func TestRunInstanceReusesTheLaunchTemplateAndGroup(t *testing.T) {
	e, cf := newEC2(t)
	// A handle stored before the stack finished has no ids in it, so the adapter
	// has to find the security group and the launch template in the event list.
	cf.events = cf.event(sgID, "CREATE_COMPLETE", "", "sg-0abc") + cf.event(ltID, "CREATE_COMPLETE", "", "lt-0abc")
	a := newAWS(t, e.f, func(o *Options) {
		o.Join = func(context.Context, string) (string, error) { return "https://x/v1/nodes/join K10::server:t", nil }
	})

	// The handle is what Create would have stored, minus the ids the stack had
	// not published yet: the adapter has to find them itself.
	got, err := a.AddNode(testContext(t), provider.NewHandle([]byte(
		`{"stack":"dawnbx-s1","parameter":"/dawnbx/bootstrap/s1","region":"eu-west-1"}`)),
		provider.NodeSpec{InstanceType: "m7g.large", DiskGiB: 60}, provider.Bootstrap{})
	if err != nil {
		t.Fatal(err)
	}
	if got != e.instanceID {
		t.Errorf("instance = %q, want %q", got, e.instanceID)
	}
	// The launch itself went to EC2, not to CloudFormation: a worker is not a
	// stack, and a second stack would multiply the ways this can fail.
	e.f.called("RunInstances")
	e.f.notCalled("CreateStack")

	q := form(t, e.f.bodyOf("RunInstances"))
	if got := q.Get("LaunchTemplate.LaunchTemplateId"); got != "lt-0abc" {
		t.Errorf("launch template = %q, want the cluster's own", got)
	}
	if got := q["NetworkInterface.1.SecurityGroupId.1"]; len(got) != 1 || got[0] != "sg-0abc" {
		t.Errorf("security group = %v, want the cluster's own", got)
	}
	if got := q.Get("InstanceType"); got != "m7g.large" {
		t.Errorf("instance type = %q, want the one that was asked for", got)
	}
	if got := q.Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got != "60" {
		t.Errorf("root volume = %q GiB, want the size that was asked for", got)
	}
	if got := q.Get("BlockDeviceMapping.1.DeviceName"); got != rootDevice {
		t.Errorf("root device = %q, want the device the template used", got)
	}
	if got := q.Get("BlockDeviceMapping.1.Ebs.VolumeType"); got != "gp3" {
		t.Errorf("root volume type = %q", got)
	}
	if got := q.Get("BlockDeviceMapping.1.Ebs.Encrypted"); got != "true" {
		t.Errorf("root volume is not encrypted: %q", got)
	}
	if got := q.Get("NetworkInterface.1.AssociatePublicIpAddress"); got != "true" {
		t.Errorf("the worker has no route out: %q", got)
	}
	// The bootstrap credential is not how a worker joins, so it is not in here.
	if strings.Contains(e.f.bodyOf("RunInstances"), secret) {
		t.Error("the launch carried a credential")
	}
}

// A worker needs the cluster's own join command, because the token that joins it
// is minted by the cluster and is not something the provider can invent.
func TestAddNodeRunsTheJoinsCommand(t *testing.T) {
	e, _ := newEC2(t)
	a := newAWS(t, e.f, func(o *Options) {
		o.Join = func(_ context.Context, url string) (string, error) {
			if url != "https://team.example.com" {
				return "", errors.New("wrong url " + url)
			}
			return "https://team.example.com/v1/nodes/join K10abc::server:token", nil
		}
	})
	h := provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc","url":"https://team.example.com"}`))

	if _, err := a.AddNode(testContext(t), h, provider.NodeSpec{InstanceType: "t4g.medium", DiskGiB: 30}, provider.Bootstrap{}); err != nil {
		t.Fatal(err)
	}
	// What goes on the wire is base64, because RunInstances requires it and
	// refuses plain text with "Invalid BASE64 encoding of user data". The test
	// asserts the encoding and then decodes, so a change to either half shows
	// up as itself rather than as a confusing miss inside a base64 blob.
	encoded := form(t, e.f.bodyOf("RunInstances")).Get("UserData")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("user data is not base64, which RunInstances requires: %q", encoded)
	}
	script := string(raw)
	if !strings.Contains(script, "--join") || !strings.Contains(script, "K10abc::server:token") {
		t.Errorf("the worker's user data does not run the join command: %q", script)
	}
	if strings.Contains(script, secret) {
		t.Error("the worker was handed a credential instead of a join command")
	}
}

// Without a join command the adapter says the capability is missing rather than
// launching a box that would sit in the VPC doing nothing.
func TestAddNodeWithoutAJoinCommandIsUnavailable(t *testing.T) {
	e, _ := newEC2(t)
	a := newAWS(t, e.f, nil)
	h := provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc","url":"https://x"}`))

	_, err := a.AddNode(testContext(t), h, provider.NodeSpec{InstanceType: "t4g.medium", DiskGiB: 30}, provider.Bootstrap{})
	if !isUnavailable(err) {
		t.Errorf("error = %v, want ErrUnavailable", err)
	}
	e.f.notCalled("RunInstances")
}

// Removing a worker asks the cluster first, because only the cluster knows
// whether it still holds sandboxes. A busy node is refused before any terminate
// is sent — the node is left exactly as it was.
func TestRemoveNodeRefusesABusyNodeWithoutTerminatingIt(t *testing.T) {
	e, _ := newEC2(t)
	var releaseCalls int
	a := newAWS(t, e.f, func(o *Options) {
		o.Release = func(_ context.Context, url, node string) error {
			releaseCalls++
			return provider.ErrNodeBusy
		}
	})
	err := a.RemoveNode(testContext(t), clusterHandle(), "i-0abc")
	if !errors.Is(err, provider.ErrNodeBusy) {
		t.Fatalf("error = %v, want ErrNodeBusy", err)
	}
	if releaseCalls != 1 {
		t.Errorf("the cluster was asked %d times", releaseCalls)
	}
	e.f.notCalled("TerminateInstances")
}

// A node the cluster has released is terminated, and terminating one that is
// already gone is not an error: EC2 answers for a terminated instance for an hour
// after it stops.
func TestRemoveNodeTerminatesOnceTheClusterHasReleasedIt(t *testing.T) {
	e, _ := newEC2(t)
	a := newAWS(t, e.f, func(o *Options) {
		o.Release = func(context.Context, string, string) error { return nil }
	})
	if err := a.RemoveNode(testContext(t), clusterHandle(), "i-0abc"); err != nil {
		t.Fatal(err)
	}
	e.f.called("TerminateInstances")
	c, err := a.forRegion("eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.terminate(testContext(t), "i-already-gone"); err != nil {
		t.Errorf("terminating a terminated instance: %v", err)
	}
}

func clusterHandle() provider.Handle {
	return provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc","url":"https://team.example.com"}`))
}

// form decodes a query body so a test can look for a field by name rather than
// by guessing where in the string the encoder put it.
func form(t *testing.T, body string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("parse %q: %v", body, err)
	}
	return v
}

// TestNodeAddrsAnswersWithTheAddressTheClusterUses: the whole point of the
// lookup is that the control plane and the cluster can name the same worker, and
// they never name it the same way. So this is not "returns the private ip"; it
// is "returns the address that lets a hostname the cluster chose be matched to
// an instance id the provider chose".
func TestNodeAddrsAnswersWithTheAddressTheClusterUses(t *testing.T) {
	e, _ := newEC2(t)
	a := newAWS(t, e.f, nil)
	h := provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc","url":"https://team.example.com"}`))

	addrs, err := a.NodeAddrs(testContext(t), h, []string{"i-0abc"})
	if err != nil {
		t.Fatal(err)
	}
	if addrs["i-0abc"] != "10.0.1.7" {
		t.Errorf("address = %q, want the instance's own private address", addrs["i-0abc"])
	}
}

// TestNodeAddrsWithNothingAsked: the control plane calls this on every refresh,
// including the common one where every worker already matched by name. It must
// not spend an API call to answer nothing.
func TestNodeAddrsWithNothingAsked(t *testing.T) {
	e, _ := newEC2(t)
	a := newAWS(t, e.f, nil)
	h := provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc","url":"https://team.example.com"}`))

	addrs, err := a.NodeAddrs(testContext(t), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 0 {
		t.Errorf("asked about nothing and got %v", addrs)
	}
}

// TestNodeAddrsLeavesOutAWorkerThatIsGone: EC2 keeps a terminated instance
// describable for about an hour and then stops, and a worker that has been
// removed is something the caller is cleaning up rather than an error to
// report. A lookup that failed here would make node refresh fail on a cluster
// that is simply losing a worker.
func TestNodeAddrsLeavesOutAWorkerThatIsGone(t *testing.T) {
	e, _ := newEC2(t)
	e.gone = true
	a := newAWS(t, e.f, nil)
	h := provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc","url":"https://team.example.com"}`))

	if _, err := a.NodeAddrs(testContext(t), h, []string{"i-gone"}); err == nil {
		t.Error("a worker EC2 will not describe was reported as answered")
	}
}

// TestNodeAddrsRefusesAnEmptyHandle: the handle is what names the region, and a
// handle that does not is a caller bug rather than a lookup that quietly looks
// in the wrong place.
func TestNodeAddrsRefusesAnEmptyHandle(t *testing.T) {
	e, _ := newEC2(t)
	a := newAWS(t, e.f, nil)
	if _, err := a.NodeAddrs(testContext(t), provider.Handle{}, []string{"i-0abc"}); err == nil {
		t.Error("an empty handle was accepted")
	}
}

// TestRemoveNodeAsksTheStackForTheURL: a cluster created without an explicit
// domain - the default, and what the quickstart uses - has no URL in its handle,
// because the address is only known once the stack exists. Removing a worker
// from one used an empty URL, and the control plane, which looks a cluster up by
// URL, answered "no cluster is registered at " for a cluster it had finished
// provisioning a minute earlier.
//
// Every other worker test here used a handle carrying a URL, which is why this
// survived: the shape that fails is the one the product produces by default.
func TestRemoveNodeAsksTheStackForTheURL(t *testing.T) {
	e, _ := newEC2(t)
	got := ""
	a := newAWS(t, e.f, func(o *Options) {
		o.Release = func(_ context.Context, url, _ string) error {
			got = url
			return nil
		}
	})
	// No "url" and no "public_ip": what a domain-less cluster's handle holds.
	h := provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc"}`))

	if err := a.RemoveNode(testContext(t), h, "i-0abc"); err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("the cluster was asked about with an empty url")
	}
	if got != "https://team.example.com" {
		t.Errorf("asked the cluster at %q, want the url the stack reports", got)
	}
	e.f.called("TerminateInstances")
}

// TestRemoveNodeSaysSoWhenTheClusterHasNoURLAnywhere: a stack that reports no
// address and a handle that carries none means there is nothing to ask, and
// saying so is better than asking with an empty string.
func TestRemoveNodeSaysSoWhenTheClusterHasNoURLAnywhere(t *testing.T) {
	e, c := newEC2(t)
	c.outputs = map[string]string{} // a stack with no outputs at all
	asked := false
	a := newAWS(t, e.f, func(o *Options) {
		o.Release = func(context.Context, string, string) error { asked = true; return nil }
	})
	h := provider.NewHandle([]byte(`{"stack":"dawnbx-s1","parameter":"/p","region":"eu-west-1",` +
		`"security_group":"sg-0abc","launch_template":"lt-0abc"}`))

	err := a.RemoveNode(testContext(t), h, "i-0abc")
	if err == nil {
		t.Fatal("a cluster with no address anywhere was accepted")
	}
	if asked {
		t.Error("the cluster was asked with no address to ask at")
	}
}
