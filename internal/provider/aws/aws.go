// Package aws is the AWS provider adapter, and the only place in dawnbx where
// an AWS SDK may appear.
//
// It exists to make AWS disappear. Everything above internal/provider deals in
// a ClusterSpec, a Bootstrap, an opaque Handle and a State; everything in this
// package is stacks, security groups, launch templates, wait conditions and
// SSM parameters, and none of it may leak upward. The test that enforces that is
// internal/provider/provider_test.go, which reads the imports of the neutral
// package; the test that enforces the other direction is here — no file in this
// directory may import dawnbx/internal/cluster, dawnbx/internal/api, or anything
// else above it, because that would be a cycle and because it would put a
// control-plane concept in the cloud's own layer.
//
// Every AWS client is built from one aws.Config whose HTTP client is injected, so
// the whole adapter runs against an httptest server in a unit test and no test
// here calls AWS.
package aws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"dawnbx/internal/provider"
)

// AWS is the adapter. It is stateless apart from a cache of per-region clients,
// so one instance serves every cluster in the process.
type AWS struct {
	region     string
	account    string
	regions    []string
	types      []string
	releaseURL string
	keyName    string
	sshCIDR    string
	template   string
	join       JoinFunc
	release    ReleaseFunc
	now        func() time.Time
	poll       func(context.Context, time.Duration) error
	settleFor  time.Duration
	every      time.Duration

	base  aws.Config
	cache sync.Map // region -> *clients
}

var _ provider.Provider = (*AWS)(nil)

// clients are the four service clients a region needs. They are built together
// because a handle names a region and every call on that handle has to land in
// that one region; a client per region is cheaper than a wrong one.
type clients struct {
	cfn     *cloudformation.Client
	ec2     *ec2.Client
	ssm     *ssm.Client
	pricing *pricing.Client
}

// JoinFunc returns the shell command a fresh worker runs to join the cluster at
// url. The command is a k3s token the cluster's own API minted, and it is the
// only thing standing between "create a worker" and "reimplement the join
// protocol here" — which is exactly the k3s knowledge that must not reach a
// provider layer.
type JoinFunc func(ctx context.Context, url string) (command string, err error)

// ReleaseFunc removes node from the cluster at url. It must return
// provider.ErrNodeBusy when the node still holds sandboxes, and the adapter
// relays that rather than terminating a node the cluster is still using.
type ReleaseFunc func(ctx context.Context, url, node string) error

// Options is what the process knows about its AWS access at startup. Anything
// here that is missing is a refusal to start rather than a default, because a
// control plane that guesses a region or opens SSH to the world is worse than one
// that says it is not configured.
type Options struct {
	// Region is the default region, and the one a stack is created in when a
	// ClusterSpec names this one. Required.
	Region string
	// ReleaseURL is the base URL of a dawnbx release: install.sh, the server
	// binary and checksums.txt must all be under it. Required.
	ReleaseURL string
	// Template is the CloudFormation template body, normally the contents of
	// deploy/aws/dawnbx.yaml. Required.
	Template string
	// KeyName is the EC2 key pair the stack installs. It is the rescue path,
	// not the normal one: the control plane never uses SSH, and the key is only
	// there for the case where the cluster is up and unreachable. Required,
	// because the template asks for it.
	KeyName string
	// SSHCIDR is who may use it, as a CIDR. Required, and deliberately narrow:
	// it is a second factor's worth of exposure and a default of 0.0.0.0/0 would
	// make the rescue path a permanent one.
	SSHCIDR string
	// HTTPClient replaces the default transport. Optional; nil uses the SDK's.
	HTTPClient aws.HTTPClient
	// BaseEndpoint overrides every service endpoint, which is how the adapter is
	// pointed at a test server and how a process behind a VPC endpoint proxy
	// reaches AWS. Optional.
	BaseEndpoint string
	// Join and Release wire the cluster's own API into worker operations. Both
	// are optional; worker add and remove return provider.ErrUnavailable without
	// them, and every other operation is unaffected.
	Join    JoinFunc
	Release ReleaseFunc
	// InstanceTypes is the catalogue HostSizes offers. Defaults to the arm64
	// sizes deploy/aws/dawnbx.yaml allows.
	InstanceTypes []string
	// Now and Poll are the clock and the sleep the settle loop uses. A test
	// replaces both so the wait is exercised in microseconds; production leaves
	// them nil.
	Now  func() time.Time
	Poll func(ctx context.Context, d time.Duration) error
	// SettleTimeout bounds the wait for a teardown. Defaults to settleTimeout.
	SettleTimeout time.Duration
}

// regions is the catalogue this build provisions in. It is a list, not a
// discovery: DescribeRegions is another call and another permission, and the
// price list and the arm64 image set do not cover every region AWS has.
var regions = []string{
	"us-east-1", "us-east-2", "us-west-2",
	"eu-west-1", "eu-west-2", "eu-central-1",
	"ap-southeast-1", "ap-southeast-2", "ap-northeast-1",
	"ca-central-1", "sa-east-1",
}

// settleTimeout and pollInterval bound and pace the teardown wait. A stack delete
// is minutes, not seconds, and the wait is what stops Failed being reported over
// resources that are still being removed — five minutes is generous for one
// instance, and going over it is reported rather than waited on forever.
const (
	settleTimeout = 5 * time.Minute
	pollInterval  = 15 * time.Second
)

// defaultTypes is deploy/aws/dawnbx.yaml's AllowedValues: arm64, because the AMI
// is arm64 and an x86 type would not boot it.
var defaultTypes = []string{"t4g.medium", "t4g.large", "t4g.xlarge", "m7g.large", "m7g.xlarge", "m7g.2xlarge"}

// New builds the adapter and proves it can spend money before the caller finds
// out. Every failure is provider.ErrUnavailable: from outside this package the
// answer to all of them is the same — this build cannot provision with AWS right
// now — and the message says which of the four was missing.
func New(ctx context.Context, o Options) (provider.Provider, error) {
	for _, req := range []struct{ name, value string }{
		{"region", o.Region},
		{"release url", o.ReleaseURL},
		{"cloudformation template", o.Template},
		{"ec2 key pair", o.KeyName},
		{"rescue ssh cidr", o.SSHCIDR},
	} {
		if req.value == "" {
			return nil, fmt.Errorf("%w: aws %s is not configured", provider.ErrUnavailable, req.name)
		}
	}
	// The rescue path is meant to be a rescue path. Naming the whole internet
	// would leave the host's SSH open to everyone for as long as it exists, which
	// is the opposite of what this field is for — so it is refused here rather
	// than described in a comment and hoped for.
	if c := o.SSHCIDR; c == "0.0.0.0/0" || c == "/0" {
		return nil, fmt.Errorf("%w: aws rescue ssh cidr must name who may use the key, not the whole internet", provider.ErrUnavailable)
	}
	if !contains(regions, o.Region) {
		return nil, fmt.Errorf("%w: aws region %q is not one this build provisions in", provider.ErrUnavailable, o.Region)
	}
	cfg, err := load(ctx, o.Region, o.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("%w: aws config: %v", provider.ErrUnavailable, classify(err))
	}
	if o.BaseEndpoint != "" {
		cfg.BaseEndpoint = aws.String(o.BaseEndpoint)
	}
	account, err := identity(ctx, cfg)
	if err != nil {
		return nil, err
	}
	types := o.InstanceTypes
	if len(types) == 0 {
		types = defaultTypes
	}
	a := &AWS{
		region:     o.Region,
		account:    account,
		regions:    regions,
		types:      append([]string(nil), types...),
		releaseURL: strings.TrimSuffix(o.ReleaseURL, "/"),
		keyName:    o.KeyName,
		sshCIDR:    o.SSHCIDR,
		template:   o.Template,
		join:       o.Join,
		release:    o.Release,
		base:       cfg,
		now:        o.Now,
		poll:       o.Poll,
		settleFor:  o.SettleTimeout,
		every:      pollInterval,
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.poll == nil {
		a.poll = sleep
	}
	if a.settleFor <= 0 {
		a.settleFor = settleTimeout
	}
	return a, nil
}

// ID is the value used in the URL.
func (a *AWS) ID() string { return "aws" }

// Capabilities is static for the process lifetime. Delivery names SSM and a
// SecureString because that is the honest ceiling: the credential is never in a
// create payload, never in a tag, and never in a URL, and the dashboard shows
// this string so an operator is never told a weaker guarantee than the one they
// have.
func (a *AWS) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Available: true,
		Delivery:  "aws-ssm-securestring",
		Regions:   append([]string(nil), a.regions...),
	}
}

// Regions lists what Capabilities promises. It makes no API call: the answer is
// the same list the capabilities route returns, and a provider that answers this
// from a network would be able to answer it with an empty list at 3am.
func (a *AWS) Regions(context.Context) ([]string, error) {
	return append([]string(nil), a.regions...), nil
}

// HostSizes lists the catalogue with each size's current hourly and monthly
// on-demand price in region. One price query per size: the price list has no
// "these six types" filter, and asking for a region's whole EC2 catalogue to
// throw away all but six of them is not a saving.
func (a *AWS) HostSizes(ctx context.Context, region string) ([]provider.HostSize, error) {
	c, err := a.forRegion(region)
	if err != nil {
		return nil, err
	}
	out := make([]provider.HostSize, 0, len(a.types))
	for _, t := range a.types {
		rate, err := c.instanceRate(ctx, region, t)
		if err != nil {
			return nil, err
		}
		out = append(out, provider.HostSize{ID: t, HourlyUSD: round(rate), MonthlyUSD: round(rate * hoursPerMonth)})
	}
	return out, nil
}

// Create makes a host and puts the bootstrap credential where the host can read
// it. It returns as soon as CloudFormation has accepted the request; everything
// after that is Status.
//
// The order is secret first, stack second, and it is not interchangeable. The
// template's user data reads the parameter during the install, so a stack created
// before the parameter exists would install without a credential and then fail
// with a reason that says nothing useful. If the stack call fails, both are torn
// down again — including a stack whose response was lost, which is the one case
// where an error does not mean nothing was created.
func (a *AWS) Create(ctx context.Context, spec provider.ClusterSpec, boot provider.Bootstrap) (provider.Handle, error) {
	h, err := a.newHandle(spec)
	if err != nil {
		return provider.Handle{}, err
	}
	c, err := a.forRegion(spec.Region)
	if err != nil {
		return provider.Handle{}, err
	}
	ps := parametersFor(c.ssm)
	if err := ps.putSecret(ctx, h.Parameter, boot.AdminPassword); err != nil {
		return provider.Handle{}, err
	}
	if err := a.createStack(ctx, c, spec, h); err != nil {
		if errors.Is(err, errStackExists) {
			// A retry, not a second cluster. The stack stays, the secret that was
			// just written is the one the install will read, and the caller polls
			// the cluster it already asked for.
			return toHandle(h)
		}
		ps.deleteSecret(ctx, h.Parameter)
		a.deleteStack(ctx, c, h)
		return provider.Handle{}, scrub(err, boot.AdminPassword)
	}
	return toHandle(h)
}

// Status is the provider's own poll.
func (a *AWS) Status(ctx context.Context, h provider.Handle) (provider.Status, error) {
	ph, err := fromHandle(h)
	if err != nil {
		return provider.Status{}, err
	}
	c, err := a.forRegion(ph.Region)
	if err != nil {
		return provider.Status{}, err
	}
	return a.describe(ctx, c, ph)
}

// Destroy removes the stack and the parameter, in that order: the stack owns the
// instance, and deleting the secret first would leave an instance that boots
// without a credential and reports a failure nobody can explain.
//
// It is idempotent in every step. A stack that is already deleting, a stack that
// never existed, and a parameter that is already gone are all the state a second
// call is being asked to confirm, not errors.
func (a *AWS) Destroy(ctx context.Context, h provider.Handle) error {
	ph, err := fromHandle(h)
	if err != nil {
		return err
	}
	c, err := a.forRegion(ph.Region)
	if err != nil {
		return err
	}
	// settle, not deleteStack: it waits for the stack to actually be gone, which
	// is what FR-018 promises before the record is forgotten. A DeleteStack call
	// that returns is a request accepted, not a teardown finished, and forgetting
	// the record on the strength of it would leave a stack nobody is tracking.
	if err := a.settle(ctx, c, ph); err != nil {
		return err
	}
	return parametersFor(c.ssm).deleteSecret(ctx, ph.Parameter)
}

// AddNode starts a worker on an existing cluster.
//
// The bootstrap credential is ignored: the worker joins with a token the cluster
// itself minted, and the password the server holds is not a credential the
// worker can use for anything. That is the case provider.Bootstrap's own
// documentation describes, and ignoring it here is why the parameter exists.
func (a *AWS) AddNode(ctx context.Context, h provider.Handle, spec provider.NodeSpec, boot provider.Bootstrap) (string, error) {
	_ = boot
	ph, err := fromHandle(h)
	if err != nil {
		return "", err
	}
	c, err := a.forRegion(ph.Region)
	if err != nil {
		return "", err
	}
	ph, err = a.resources(ctx, c, ph)
	if err != nil {
		return "", err
	}
	if a.join == nil {
		return "", fmt.Errorf("%w: adding a worker needs a join command from the cluster", provider.ErrUnavailable)
	}
	url := a.url(ph, stackView{})
	if url == "" {
		return "", fmt.Errorf("%w: cluster %s has no url yet", provider.ErrNotFound, ph.Stack)
	}
	command, err := a.join(ctx, url)
	if err != nil {
		return "", err
	}
	return c.runInstance(ctx, ph, spec, workerUserData(a.releaseURL, command))
}

// RemoveNode takes a worker out of the cluster and then terminates it.
//
// The cluster is asked first, because only it knows whether the node holds
// sandboxes, and a node that does is left exactly as it was: no terminate call
// is made, and the caller's 409 carries the cluster's own answer rather than a
// guess from a count that was cached somewhere.
func (a *AWS) RemoveNode(ctx context.Context, h provider.Handle, node string) error {
	ph, err := fromHandle(h)
	if err != nil {
		return err
	}
	c, err := a.forRegion(ph.Region)
	if err != nil {
		return err
	}
	if a.release == nil {
		return fmt.Errorf("%w: removing a worker needs the cluster's own answer", provider.ErrUnavailable)
	}
	url := a.url(ph, stackView{})
	if err := a.release(ctx, url, node); err != nil {
		if errors.Is(err, provider.ErrNodeBusy) {
			return fmt.Errorf("%w: %s", provider.ErrNodeBusy, node)
		}
		return err
	}
	return c.terminate(ctx, node)
}

// SetBootstrap re-delivers a rotated credential by overwriting the same
// parameter. The host reads it on its next start; a running cluster keeps the
// credential it already has until an operator applies the new one, which is the
// cluster's business and not this adapter's.
func (a *AWS) SetBootstrap(ctx context.Context, h provider.Handle, boot provider.Bootstrap) error {
	ph, err := fromHandle(h)
	if err != nil {
		return err
	}
	c, err := a.forRegion(ph.Region)
	if err != nil {
		return err
	}
	return parametersFor(c.ssm).putSecret(ctx, ph.Parameter, boot.AdminPassword)
}

// NodeAddrs maps instance ids to the private address the cluster reaches them
// on. EC2 keeps a terminated instance describable for about an hour, so an id
// that no longer exists is left out of the answer rather than reported as an
// error: a worker that is gone is not a failure of this lookup, it is the thing
// the caller is about to clean up.
func (a *AWS) NodeAddrs(ctx context.Context, h provider.Handle, nodes []string) (map[string]string, error) {
	ph, err := fromHandle(h)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return map[string]string{}, nil
	}
	c, err := a.forRegion(ph.Region)
	if err != nil {
		return nil, err
	}
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: nodes,
		Filters: []ec2types.Filter{{
			Name:   aws.String("instance-state-name"),
			Values: []string{"pending", "running", "stopping", "stopped"},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe workers on %s: %w", ph.Stack, err)
	}
	addrs := make(map[string]string, len(nodes))
	for _, r := range out.Reservations {
		for _, in := range r.Instances {
			if ip := aws.ToString(in.PrivateIpAddress); ip != "" {
				addrs[aws.ToString(in.InstanceId)] = ip
			}
		}
	}
	return addrs, nil
}

// forRegion returns the clients for a region, building them once. A handle names
// its own region because it can outlive a restart with a different default, and
// using this process's region for it would either fail in a way that looks like
// a missing stack or, worse, succeed against the wrong account's resources.
func (a *AWS) forRegion(region string) (*clients, error) {
	if region == "" {
		region = a.region
	}
	if !contains(a.regions, region) {
		return nil, fmt.Errorf("%w: aws region %q is not one this build provisions in", provider.ErrUnavailable, region)
	}
	if c, ok := a.cache.Load(region); ok {
		return c.(*clients), nil
	}
	cfg := a.base.Copy()
	cfg.Region = region
	c := &clients{
		cfn:     cloudformation.NewFromConfig(cfg),
		ec2:     ec2.NewFromConfig(cfg),
		ssm:     ssm.NewFromConfig(cfg),
		pricing: pricing.NewFromConfig(cfg, func(o *pricing.Options) { o.Region = pricingRegion }),
	}
	if real, loaded := a.cache.LoadOrStore(region, c); loaded {
		return real.(*clients), nil
	}
	return c, nil
}

// newHandle names the resources a cluster will have.
//
// The neutral spec carries no cluster name, so the stack name comes from the
// domain when there is one. That is a determinism worth having: a retried create
// lands on the same stack with the same client request token, which CloudFormation
// treats as the same call, instead of quietly provisioning a second host that
// nobody asked for and that would fight over the same DNS name anyway. With no
// domain there is nothing to derive a name from, so it is a random one and each
// create is its own cluster.
func (a *AWS) newHandle(spec provider.ClusterSpec) (handle, error) {
	name, err := stackName(spec.Domain)
	if err != nil {
		return handle{}, err
	}
	h := handle{
		Stack:     name,
		Parameter: "/dawnbx/bootstrap/" + name,
		Region:    spec.Region,
	}
	if spec.Domain != "" {
		h.URL = "https://" + spec.Domain
	}
	return h, nil
}

// stackName maps a domain onto a CloudFormation stack name, which allows letters,
// digits and hyphens, must start with a letter, and is capped at 128 characters.
func stackName(domain string) (string, error) {
	if s := slug(domain); s != "" {
		return "dawnbx-" + truncate(s, 100), nil
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("name a stack: %w", err)
	}
	return "dawnbx-host-" + hex.EncodeToString(b[:4]), nil
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// contains is a linear scan on purpose. The region list is eleven entries and is
// not kept sorted, and a binary search over a list that only looks sorted fails
// silently on exactly the call that matters: a handle naming a real region.
func contains(vs []string, v string) bool {
	for _, s := range vs {
		if s == v {
			return true
		}
	}
	return false
}

// sleep waits, or gives up when the context does.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
