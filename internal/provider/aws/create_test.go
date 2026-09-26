package aws

import (
	"strings"
	"testing"

	"dawnbx/internal/provider"
)

// Create is where money starts, so each way it can fail matters. The pattern is
// always the same — the secret is written, then the host is asked for — and the
// rule is that a failure leaves nothing behind. A half-created cluster is a
// billable resource nobody is tracking, which is the specific outcome this file
// exists to prevent.

func TestCreateTearsDownWhenTheSecretCannotBeStored(t *testing.T) {
	f := newFake(t, func(action string, _ []byte) (int, string) {
		if action == "AmazonSSM.PutParameter" {
			return 400, jsonErr("AccessDeniedException", "not authorised")
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	if _, err := a.Create(testContext(t), clusterSpec(), provider.Bootstrap{AdminPassword: secret}); err == nil {
		t.Fatal("a refused secret was reported as success")
	}
	// Nothing was asked of CloudFormation, so no host can exist.
	f.notCalled("CreateStack")
}

func TestCreateTearsDownBothWhenTheStackCallFails(t *testing.T) {
	f := newFake(t, func(action string, _ []byte) (int, string) {
		switch action {
		case "CreateStack":
			return 400, cfnErr("ValidationError", "template rejected")
		case "AmazonSSM.PutParameter":
			return 200, `{"Version":1}`
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	if _, err := a.Create(testContext(t), clusterSpec(), provider.Bootstrap{AdminPassword: secret}); err == nil {
		t.Fatal("a refused stack was reported as success")
	}
	// The secret that was already written must go, or it is a credential
	// sitting in a parameter store for a host that does not exist.
	f.called("AmazonSSM.DeleteParameter")
}

// TestCreateOnAStackThatAlreadyExistsIsARetryNotASecondCluster: a create that
// lands on an existing stack name is an operator pressing the button twice, not
// a request for two clusters. The secret that was just written is the one the
// install will read, and the caller polls the cluster it already asked for.
func TestCreateOnAStackThatAlreadyExistsIsARetryNotASecondCluster(t *testing.T) {
	f := newFake(t, func(action string, _ []byte) (int, string) {
		switch action {
		case "AmazonSSM.PutParameter":
			return 200, `{"Version":1}`
		case "CreateStack":
			return 400, cfnAlreadyExists("dawnbx-probe1")
		}
		return 400, jsonErr("InvalidAction", "unknown")
	})
	a := newAWS(t, f, nil)
	h, err := a.Create(testContext(t), clusterSpec(), provider.Bootstrap{AdminPassword: secret})
	if err != nil {
		t.Fatalf("a retry was reported as a failure: %v", err)
	}
	// The handle is usable, so the caller can poll the cluster that is already
	// there rather than being told to start over.
	if h.Empty() {
		t.Error("a retry returned no handle to poll with")
	}
	// And nothing was torn down: the running cluster's secret must survive.
	f.notCalled("AmazonSSM.DeleteParameter")
	f.notCalled("DeleteStack")
}

// TestCreateScrubsTheSecretFromItsError: the one error that could carry the
// credential is a stack call that failed after the secret was written, so the
// message is passed through scrub. A password in a log line is the failure this
// prevents.
func TestCreateScrubsTheSecretFromItsError(t *testing.T) {
	leaky := secret + "-appears-in-the-error-text"
	f := newFake(t, func(action string, _ []byte) (int, string) {
		if action == "CreateStack" {
			return 400, jsonErr("ValidationError", "template rejected: "+leaky)
		}
		return 200, "{}"
	})
	a := newAWS(t, f, nil)
	_, err := a.Create(testContext(t), clusterSpec(), provider.Bootstrap{AdminPassword: secret})
	if err == nil {
		t.Fatal("a refused stack was reported as success")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the bootstrap password reached the error text: %v", err)
	}
}

// TestCreateDoesNotValidateTheSpec: validation belongs to internal/cluster,
// which checks a request against the provider's catalogue before any cloud call
// is made. Duplicating it here would be a second rule to keep in step with the
// first, and a disagreement between them would reject a valid request or accept
// an invalid one. What this asserts instead is the ordering that does belong
// here: the secret is written before the host is asked for, so a host can never
// come up without a credential to read.
func TestCreateWritesTheSecretBeforeAskingForAHost(t *testing.T) {
	f := newFake(t, func(action string, _ []byte) (int, string) {
		if action == "CreateStack" {
			return 200, query("CreateStack", "")
		}
		return 200, `{}`
	})
	a := newAWS(t, f, nil)
	spec := clusterSpec()
	spec.InstanceType = "" // the caller's validation, not this package's
	if _, err := a.Create(testContext(t), spec, provider.Bootstrap{AdminPassword: secret}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	order := f.order()
	iPut, iStack := -1, -1
	for i, a := range order {
		switch a {
		case "AmazonSSM.PutParameter":
			iPut = i
		case "CreateStack":
			iStack = i
		}
	}
	if iPut < 0 || iStack < 0 || iPut > iStack {
		t.Errorf("the credential must be stored before the host is asked for, got %v", order)
	}
}

// clusterSpec is a configuration the adapter accepts, so a test can change one
// field and know the rest is not what failed.
func clusterSpec() provider.ClusterSpec {
	return provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30}
}

// jsonErr is the json-1.1 error envelope SSM sends.
func jsonErr(code, message string) string {
	return `{"__type":"` + code + `","message":"` + message + `"}`
}

// cfnAlreadyExists is CloudFormation's answer for a stack name already taken.
func cfnAlreadyExists(name string) string {
	return `<ErrorResponse><Error><Code>AlreadyExistsException</Code>` +
		`<Message>Stack [` + name + `] already exists</Message></Error></ErrorResponse>`
}

// cfnErr is CloudFormation's error envelope, which is the query protocol's
// shape rather than the json-1.1 one SSM sends.
func cfnErr(code, message string) string {
	return `<ErrorResponse><Error><Code>` + code + `</Code><Message>` + message +
		`</Message></Error></ErrorResponse>`
}
