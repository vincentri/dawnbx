package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"dawnbx/internal/provider"
)

// A worker is one RunInstances call, not a stack. A second stack per worker
// would need a role the template has no parameter for, and would multiply
// stack-wait timeouts and rollback paths for a resource that is a single
// instance with a launch template already built for it.

// rootDevice is the device the server's root volume is on. The worker overrides
// the size of the same device so it gets an encrypted gp3 of the size that was
// asked for rather than whatever the template defaulted to.
const rootDevice = "/dev/sda1"

// runInstance starts one worker from the cluster's launch template and security
// group and returns its instance id.
//
// The launch template is reused rather than re-specified, because it carries the
// setting that is not about capacity: IMDSv2 with a hop limit of 1, so a pod on
// the worker cannot read instance metadata. A worker built from anything else
// would be a weaker host than the one the operator priced.
//
// It does not carry the instance profile. That is set on the instance, not the
// template, and runInstance passes none, so a worker cannot read the bootstrap
// SecureString — it joins with the command its own user-data was given. The
// narrower posture is the one worth having, but it belongs in the comment rather
// than being implied by a template it is not in.
//
// Instance type and root size are passed alongside the template. EC2 lets a
// launch override those two; image, user data and IAM may only come from the
// template, and that is exactly what is being inherited.
func (c *clients) runInstance(ctx context.Context, h handle, spec provider.NodeSpec, userData string) (string, error) {
	if h.LaunchTemplate == "" || h.SecurityGroup == "" {
		return "", fmt.Errorf("%w: cluster %s has no launch template or security group", provider.ErrNotFound, h.Stack)
	}
	in := &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		InstanceType: ec2types.InstanceType(spec.InstanceType),
		LaunchTemplate: &ec2types.LaunchTemplateSpecification{
			LaunchTemplateId: aws.String(h.LaunchTemplate),
			Version:          aws.String("$Latest"),
		},
		// A network interface, not SecurityGroupIds: the two cannot be combined,
		// and the interface is what asks for a public address. The server has an
		// Elastic IP; a worker does not, it only needs a route out to reach the
		// cluster's 6443.
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			AssociatePublicIpAddress: aws.Bool(true),
			DeleteOnTermination:      aws.Bool(true),
			Groups:                   []string{h.SecurityGroup},
		}},
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String(h.Stack)}},
		}},
	}
	if spec.DiskGiB > 0 {
		size := int32(spec.DiskGiB) //nolint:gosec // a disk size in GiB, bounded by the catalogue
		in.BlockDeviceMappings = []ec2types.BlockDeviceMapping{{
			DeviceName: aws.String(rootDevice),
			Ebs: &ec2types.EbsBlockDevice{
				VolumeSize:          aws.Int32(size),
				VolumeType:          ec2types.VolumeTypeGp3,
				Encrypted:           aws.Bool(true),
				DeleteOnTermination: aws.Bool(true),
			},
		}}
	}
	if userData != "" {
		in.UserData = aws.String(userData)
	}
	out, err := c.ec2.RunInstances(ctx, in)
	if err != nil {
		return "", fmt.Errorf("run instance on %s: %w", h.Stack, err)
	}
	if len(out.Instances) == 0 || aws.ToString(out.Instances[0].InstanceId) == "" {
		return "", fmt.Errorf("run instance on %s: ec2 returned no instance", h.Stack)
	}
	return aws.ToString(out.Instances[0].InstanceId), nil
}

// terminate stops a worker. EC2 keeps a terminated instance's record for about
// an hour, so an instance that is gone is still describable; that is why an id
// EC2 has never heard of and a terminated instance are different answers.
func (c *clients) terminate(ctx context.Context, node string) error {
	_, err := c.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{node}})
	if err != nil {
		if instanceGone(err) {
			return nil
		}
		return fmt.Errorf("terminate instance %s: %w", node, err)
	}
	return nil
}

// instanceGone recognises EC2's answer for an id it has never heard of. It is a
// plain ClientError carrying a code, not a modelled error, so the code is the
// test — and it is checked rather than assumed, because a launch that was
// refused for quota looks like a failure that must be retried.
func instanceGone(err error) bool {
	if err == nil {
		return false
	}
	switch apiCode(err) {
	case "InvalidInstanceID.NotFound", "InvalidInstanceID.Malformed":
		return true
	}
	return false
}

// workerUserData is the script a fresh worker runs: fetch the same release the
// server ran, then hand install.sh the join command the cluster's own API
// issued. Nothing about k3s is decided here — the command is a string the
// cluster produced, and the installer already knows how to run it.
func workerUserData(releaseURL, command string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("cd /root\n")
	b.WriteString("curl -fsSL --retry 3 -o install.sh '" + releaseURL + "/install.sh'\n")
	b.WriteString("bash install.sh --yes --join " + command + "\n")
	return b.String()
}
