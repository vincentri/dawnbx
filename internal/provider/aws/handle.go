package aws

import (
	"encoding/json"
	"fmt"

	"dawnbx/internal/provider"
)

// handle is the adapter's private state. The control plane stores this JSON
// verbatim and never reads a field out of it, so the shape is free to change
// with the adapter and to name AWS resources: nothing above internal/provider
// knows what a stack is. Adding a field is backwards compatible; removing one
// is not, because a stored handle would stop decoding.
//
// Every field is a name the adapter needs again, and nothing else:
//
//   - stack             the CloudFormation stack that owns the host
//   - parameter         the SSM SecureString the instance role reads the
//     bootstrap credential from; also the name install.sh is told to read
//   - security_group    workers join through it
//   - launch_template   workers inherit its MetadataOptions, which is what
//     keeps pods away from IMDS
//   - public_ip         the Elastic IP the URL is built from
//   - url               the cluster's own URL, once the stack publishes it
//   - region            the region the clients are rebuilt for on every call,
//     because a process provisions into exactly one region but a handle may
//     outlive a restart with a different default
type handle struct {
	Stack          string `json:"stack"`
	Parameter      string `json:"parameter"`
	SecurityGroup  string `json:"security_group"`
	LaunchTemplate string `json:"launch_template"`
	PublicIP       string `json:"public_ip"`
	URL            string `json:"url"`
	Region         string `json:"region"`
}

// usable reports whether a decoded handle names a cluster. A stack and its
// secret are the minimum: without both there is nothing to poll and nothing to
// delete, so a partial handle is treated as no handle at all.
func (h handle) usable() bool { return h.Stack != "" && h.Parameter != "" }

// toHandle renders the adapter's state as the opaque value the control plane
// stores. Marshalling cannot fail for this struct, so a failure here is a bug
// rather than a condition to report; the caller turns it into an error.
func toHandle(h handle) (provider.Handle, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return provider.Handle{}, fmt.Errorf("encode handle: %w", err)
	}
	return provider.NewHandle(b), nil
}

// fromHandle decodes a stored handle. Every failure is provider.ErrNotFound:
// a handle this adapter cannot read names nothing it knows, and the control
// plane treats that as "not a cluster of ours" rather than as a 500. A handle
// written by an older adapter, truncated in a database column, or hand-edited
// lands here too, and must not panic on the way.
func fromHandle(h provider.Handle) (handle, error) {
	if h.Empty() {
		return handle{}, fmt.Errorf("%w: empty provider state", provider.ErrNotFound)
	}
	var out handle
	if err := json.Unmarshal(h.Bytes(), &out); err != nil {
		return handle{}, fmt.Errorf("%w: unreadable provider state", provider.ErrNotFound)
	}
	if !out.usable() {
		return handle{}, fmt.Errorf("%w: provider state names no stack", provider.ErrNotFound)
	}
	return out, nil
}
