package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"

	"dawnbx/internal/provider"
)

// Logical ids and output keys of deploy/aws/dawnbx.yaml. They are read out of
// the stack rather than out of a config file, so the adapter follows the
// template and a template change is a change here, in one place, visibly.
const (
	readyWaitID  = "ReadyWait" // AWS::CloudFormation::WaitCondition — the install's verdict
	serverID     = "Server"    // the EC2 instance; its CREATE_COMPLETE means a host exists
	sgID         = "Sg"        // the security group workers join through
	ltID         = "Lt"        // the launch template workers inherit
	outURL       = "Url"
	outPublicIP  = "PublicIp"
	outSecurity  = "SecurityGroup"
	outLaunchTpl = "LaunchTemplateId"
)

// resourceStatusFailed is the event status a wait condition carries when the
// install signalled FAILURE. The SDK models no constant for it, because a
// resource that is anything other than a CloudFormation resource cannot reach
// this state, and a WaitCondition is the one thing that can.
const resourceStatusFailed cftypes.ResourceStatus = "FAILED"

// createStack asks CloudFormation for the host.
//
// The create posture is DisableRollback with no OnFailure action, and it is the
// adapter's own teardown that makes it safe. A rollback would delete the instance
// before the install's failure reason could be read off the wait condition, and
// OnFailure=DELETE would take the stack away entirely, so the next poll would
// find nothing and the operator would be told "gone" instead of "it failed, and
// here is why". With rollback off a failure is stable: CREATE_FAILED, reason
// intact, resources still billing. describe reads the reason and then settles the
// teardown itself, and nothing above internal/provider ever learns that a
// rollback was possible at all.
func (a *AWS) createStack(ctx context.Context, c *clients, spec provider.ClusterSpec, h handle) error {
	params := []cftypes.Parameter{
		{ParameterKey: aws.String("InstanceType"), ParameterValue: aws.String(spec.InstanceType)},
		{ParameterKey: aws.String("DiskGiB"), ParameterValue: aws.String(strconv.Itoa(spec.DiskGiB))},
		{ParameterKey: aws.String("Domain"), ParameterValue: aws.String(spec.Domain)},
		{ParameterKey: aws.String("ReleaseUrl"), ParameterValue: aws.String(a.releaseURL)},
		{ParameterKey: aws.String("KeyName"), ParameterValue: aws.String(a.keyName)},
		{ParameterKey: aws.String("SshCidr"), ParameterValue: aws.String(a.sshCIDR)},
		// The name the host reads its bootstrap credential from. Only the name
		// crosses into CloudFormation; the value is in Parameter Store.
		{ParameterKey: aws.String("BootstrapParameter"), ParameterValue: aws.String(h.Parameter)},
	}
	_, err := c.cfn.CreateStack(ctx, &cloudformation.CreateStackInput{
		StackName:    aws.String(h.Stack),
		TemplateBody: aws.String(a.template),
		Parameters:   params,
		// DisableRollback is what makes the failure readable; see above.
		DisableRollback: aws.Bool(true),
		// The instance role that reads the SecureString is created by the stack,
		// which CloudFormation will not do without being told it may.
		Capabilities:       []cftypes.Capability{cftypes.CapabilityCapabilityIam, cftypes.CapabilityCapabilityNamedIam},
		ClientRequestToken: aws.String(stackToken(h.Stack, params)),
	})
	if err != nil {
		if apiCode(err) == "AlreadyExistsException" {
			return errStackExists
		}
		return fmt.Errorf("create stack %s: %w", h.Stack, err)
	}
	return nil
}

// errStackExists means this stack is already being provisioned. It is not a
// failure: the stack name comes from the domain, so a retried create for the same
// domain is the same cluster, and CloudFormation refusing a second one is the
// answer the retry needed. The caller keeps the stack and polls it.
var errStackExists = errors.New("stack already exists")

// apiCode is the service's error code, or "" when the error is not one the
// service named. Both CloudFormation and EC2 answer with an unmodelled code for
// the cases that matter most here, so it is read rather than matched by type.
func apiCode(err error) string {
	var api interface{ ErrorCode() string }
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// deleteStack asks for the stack to go. CloudFormation deletes it whether or not
// it ever finished, so this is also the teardown of a half-created cluster. A
// stack that is already gone is success: Destroy must be safe to call twice.
func (a *AWS) deleteStack(ctx context.Context, c *clients, h handle) error {
	_, err := c.cfn.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: aws.String(h.Stack)})
	if err != nil && !stackAbsent(err) {
		return fmt.Errorf("delete stack %s: %w", h.Stack, err)
	}
	return nil
}

// stackView is one reading of a stack: its status, why, what it published, and
// what it has done so far.
type stackView struct {
	found   bool
	status  cftypes.StackStatus
	reason  string
	outputs map[string]string
	events  []cftypes.StackEvent
}

func (v stackView) out(key string) string { return v.outputs[key] }

// hostUp reports whether the instance resource exists yet. It is what separates
// provider.Creating from provider.Bootstrapping: before the instance is created
// nothing is known, and after it is the host is installing.
func (v stackView) hostUp() bool {
	for _, e := range v.events {
		if aws.ToString(e.LogicalResourceId) == serverID && e.ResourceStatus == cftypes.ResourceStatusCreateComplete {
			return true
		}
	}
	return false
}

// waitFailure returns the install's own verdict from the wait condition. This is
// the only place the reason an install failed exists, and it is what an operator
// is shown, so it is sanitised before it is returned.
func (v stackView) waitFailure() string {
	for _, e := range v.events {
		if aws.ToString(e.LogicalResourceId) != readyWaitID {
			continue
		}
		if e.ResourceStatus == resourceStatusFailed {
			if r := aws.ToString(e.ResourceStatusReason); r != "" {
				return r
			}
		}
	}
	return ""
}

// resourceID returns the physical id CloudFormation assigned to a resource. It
// is how the security group and the launch template are found: their ids are not
// in the handle until the stack finishes, and a worker cannot wait that long.
func (v stackView) resourceID(logical string) string {
	for _, e := range v.events {
		if aws.ToString(e.LogicalResourceId) == logical {
			if id := aws.ToString(e.PhysicalResourceId); id != "" {
				return id
			}
		}
	}
	return ""
}

// stack reads the stack named by h, plus its events.
func (a *AWS) stack(ctx context.Context, c *clients, h handle) (stackView, error) {
	out, err := c.cfn.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(h.Stack)})
	if err != nil {
		if stackAbsent(err) {
			return stackView{}, nil
		}
		return stackView{}, fmt.Errorf("describe stack %s: %w", h.Stack, err)
	}
	if len(out.Stacks) == 0 {
		return stackView{}, nil
	}
	s := out.Stacks[0]
	v := stackView{
		found:   true,
		status:  s.StackStatus,
		reason:  aws.ToString(s.StackStatusReason),
		outputs: make(map[string]string, len(s.Outputs)),
	}
	for _, o := range s.Outputs {
		v.outputs[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	// Events are only needed to say what the stack has done — which resources
	// exist, and what the wait condition said — so a missing event list costs
	// the caller a less precise answer, not an error.
	ev, err := c.cfn.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: aws.String(h.Stack)})
	if err == nil {
		v.events = ev.StackEvents
	}
	return v, nil
}

// describe maps a stack onto the neutral state machine.
//
// The mapping is the whole point of this file. Four cases deserve their own
// comment, because getting any of them wrong leaks a cloud concept upstairs:
//
//   - A stack rolling back is reported as Bootstrapping, not as a failure. The
//     teardown is the adapter's own business and the orchestrator polls
//     provider.Status and nothing else; a state meaning "my cloud is cleaning up"
//     would be a new branch in internal/cluster with no meaning on a provider
//     that has no rollback.
//   - A terminal failure is not reported until the teardown has finished.
//     provider.Status's contract says the provider has settled its own cleanup
//     before it says Failed, so the caller's cleanup runs against nothing and
//     cannot race with it.
//   - A stack that is absent is Gone, not an error. Destroy leaves nothing to
//     poll, and a poll after a destroy is the normal end of a cluster's life.
//   - A status this build has never seen is still a stack that has not finished,
//     so it is Creating. A provider that invented a state would be a state the
//     orchestrator has never been shown.
func (a *AWS) describe(ctx context.Context, c *clients, h handle) (provider.Status, error) {
	v, err := a.stack(ctx, c, h)
	if err != nil {
		return provider.Status{}, err
	}
	if !v.found {
		return provider.Status{State: provider.Gone}, nil
	}
	switch v.status {
	case cftypes.StackStatusCreateInProgress, cftypes.StackStatusReviewInProgress:
		if v.hostUp() {
			return provider.Status{State: provider.Bootstrapping}, nil
		}
		return provider.Status{State: provider.Creating}, nil

	case cftypes.StackStatusCreateComplete:
		return provider.Status{State: provider.Ready, URL: a.url(h, v)}, nil

	case cftypes.StackStatusRollbackInProgress, cftypes.StackStatusUpdateRollbackInProgress:
		// The adapter's own teardown, in flight. Never surfaced.
		return provider.Status{State: provider.Bootstrapping}, nil

	case cftypes.StackStatusCreateFailed,
		cftypes.StackStatusRollbackComplete,
		cftypes.StackStatusRollbackFailed,
		cftypes.StackStatusUpdateRollbackComplete,
		cftypes.StackStatusUpdateRollbackFailed,
		cftypes.StackStatusDeleteFailed:
		return a.failed(ctx, c, h, v), nil

	case cftypes.StackStatusDeleteInProgress, cftypes.StackStatusDeleteComplete:
		return provider.Status{State: provider.Gone}, nil
	}
	return provider.Status{State: provider.Creating}, nil
}

// failed settles the teardown and then says so. The order matters: the reason is
// read first, because once the stack is deleted there is nothing left to read it
// from.
func (a *AWS) failed(ctx context.Context, c *clients, h handle, v stackView) provider.Status {
	reason := sanitise(firstNonEmpty(v.waitFailure(), v.reason))
	if err := a.settle(ctx, c, h); err != nil {
		// Not returned as an error: the orchestrator would poll again and learn
		// nothing. The operator is told the cluster failed and that AWS may
		// still be holding something, which is the truth and is actionable.
		reason = join(reason, "teardown incomplete: resources may still exist in "+h.Region)
	}
	return provider.Status{State: provider.Failed, Reason: reason}
}

// settle tears the cluster down and waits until it is really gone. It is the one
// call that blocks: a single-instance stack takes a couple of minutes to delete,
// and Status is the only thing that can settle a failure. The wait is bounded so
// a teardown that will not finish becomes a sentence in Reason rather than a
// request that never returns, and Options.SettleTimeout is there for a deployment
// that has measured a slower account.
//
// It is called on the failure path, where the alternative is worse: a stack left
// in CREATE_FAILED keeps billing an instance that will never answer, and only
// the provider knows the stack's name. It waits for a stack that is already
// rolling back rather than issuing a second delete, because CloudFormation
// rejects a delete of a stack mid-rollback and the caller would see an error for
// something already being undone.
func (a *AWS) settle(ctx context.Context, c *clients, h handle) error {
	v, err := a.stack(ctx, c, h)
	if err != nil {
		return err
	}
	if !v.found {
		return nil
	}
	switch v.status {
	case cftypes.StackStatusRollbackInProgress, cftypes.StackStatusUpdateRollbackInProgress, cftypes.StackStatusDeleteInProgress:
		// Already on its way out. Nothing to ask for; only to wait.
	default:
		if err := a.deleteStack(ctx, c, h); err != nil {
			return err
		}
	}
	deadline := a.now().Add(a.settleFor)
	for {
		v, err := a.stack(ctx, c, h)
		if err != nil {
			return err
		}
		if !v.found {
			return nil
		}
		if !a.now().Before(deadline) {
			return fmt.Errorf("stack %s is still %s after %s", h.Stack, v.status, a.settleFor)
		}
		if err := a.poll(ctx, a.every); err != nil {
			return err
		}
	}
}

// url is the cluster's own address. The stack's output is authoritative; the
// handle's copy and the sslip.io default are only there so a caller is never
// handed a Ready host with no way to reach it.
func (a *AWS) url(h handle, v stackView) string {
	if u := v.out(outURL); u != "" {
		return u
	}
	if h.URL != "" {
		return h.URL
	}
	if ip := firstNonEmpty(v.out(outPublicIP), h.PublicIP); ip != "" {
		return "https://" + ip + ".sslip.io"
	}
	return ""
}

// resources fills in what a worker needs to join: the security group and the
// launch template. They are read from the stack rather than trusted from the
// handle, because a handle stored before the stack finished does not have them
// yet and AddNode cannot wait.
func (a *AWS) resources(ctx context.Context, c *clients, h handle) (handle, error) {
	// The url is part of what is filled in here, so a handle that carries the two
	// resource ids but no url still has to be read: a cluster created without a
	// domain has an sslip.io url that only the stack's output knows.
	if h.SecurityGroup != "" && h.LaunchTemplate != "" && h.URL != "" {
		return h, nil
	}
	v, err := a.stack(ctx, c, h)
	if err != nil {
		return handle{}, err
	}
	if !v.found {
		return handle{}, fmt.Errorf("%w: stack %s", provider.ErrNotFound, h.Stack)
	}
	if h.SecurityGroup == "" {
		h.SecurityGroup = firstNonEmpty(v.out(outSecurity), v.resourceID(sgID))
	}
	if h.LaunchTemplate == "" {
		h.LaunchTemplate = firstNonEmpty(v.out(outLaunchTpl), v.resourceID(ltID))
	}
	if h.PublicIP == "" {
		h.PublicIP = v.out(outPublicIP)
	}
	if h.URL == "" {
		h.URL = a.url(h, v)
	}
	return h, nil
}

// stackAbsent recognises "no such stack". CloudFormation answers a missing stack
// with a generic ValidationError rather than a modelled one, and telling that
// apart from a transport failure is the difference between a cluster that is
// gone and a cluster we could not ask about — and the first is something the
// caller deletes, so a wrong answer here destroys a live cluster.
func stackAbsent(err error) bool {
	if err == nil {
		return false
	}
	code := apiCode(err)
	if code != "ValidationError" && code != "ResourceNotFoundException" {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "does not exist")
}

// reasonMax bounds a reason before it is stored, displayed and re-escaped. A
// stack's own StackStatusReason can be a paragraph; a dashboard row is not.
const reasonMax = 240

// secretishKey matches an assignment to something shaped like a credential and
// takes the rest of the line with it. Dropping the tail is deliberate: "Bearer
// abc123" is two tokens, and redacting only the first leaves the second sitting
// in a dashboard. The key is kept so the operator can still see that a credential
// was involved, which is the part that tells them where to look.
var secretishKey = regexp.MustCompile(`(?i)([\w.-]*(?:pass(?:word|wd)?|secret|token|credential|api[-_ ]?key|authorization)[\w.-]*\s*[:=])\s*\S+(?:\s+\S+)*\s*`)

// sanitise makes a reason safe to store and to show. It is applied to everything
// the provider puts in provider.Status.Reason, which reaches a dashboard and an
// audit log.
//
// It is applied to the install's own text even though install.sh already
// sanitises it, because a reason is attacker-influenced text — it contains a URL
// and the output of a command — and one sanitising pass is one bug away from
// being none. What it cannot do is remove a bare value it has never seen: the
// adapter does not retain the bootstrap credential after Create returns, because
// holding it for the life of the process would put it in every core dump. The
// defence is structural instead — the value goes to SSM and to nothing else.
func sanitise(s string) string {
	s = truncate(s, 4*reasonMax)
	s = strings.Join(strings.Fields(s), " ")
	s = secretishKey.ReplaceAllString(s, "$1")
	return truncate(s, reasonMax)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "..."
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func join(s, more string) string {
	if s == "" {
		return more
	}
	if more == "" {
		return s
	}
	return s + "; " + more
}

// stackToken makes CreateStack idempotent for a retried request: CloudFormation
// treats a repeat with the same token and the same parameters as the same call.
// A retry with a different token would try to create a second stack of the same
// name and fail, which is the least useful answer a control plane can give during
// a network blip.
//
// It is a hash because the API caps a client request token at 128 characters and
// the parameter list is longer than that for every configuration. Nothing in the
// input is secret — it is the stack name and the template's parameters, one of
// which is the name of the parameter store entry — so the digest carries no
// more than the input did.
func stackToken(name string, params []cftypes.Parameter) string {
	var b strings.Builder
	b.WriteString(name)
	for _, p := range params {
		b.WriteString("\x00")
		b.WriteString(aws.ToString(p.ParameterKey))
		b.WriteString("=")
		b.WriteString(aws.ToString(p.ParameterValue))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
