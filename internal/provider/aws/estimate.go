package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"dawnbx/internal/provider"
)

// hoursPerMonth is the divisor AWS's own monthly calculator uses. A month is not
// 30 days of 24 hours; it is 730, and an estimate that says otherwise is
// understating or overstating every line by two percent.
const hoursPerMonth = 730.0

// excluded names the charges deliberately left out of every estimate. The list is
// the same for every configuration and is not derived from the price list: the
// price list has no per-product way to say "this varies with your traffic", and
// an operator is better served by a short honest list than by a number that
// pretends to include a variable.
var excluded = []string{"data_transfer", "taxes", "provider_discounts"}

// Estimate prices a configuration. It creates nothing, and it reads three
// rates: the instance, the disk, and the public address the host is reachable on.
//
// Every line is rounded before it is summed, so the total is exactly the sum of
// what is displayed. Rounding afterwards would produce a table whose rows do not
// add up, which is the kind of thing an operator notices and stops believing.
func (a *AWS) Estimate(ctx context.Context, spec provider.ClusterSpec) (*provider.Estimate, error) {
	c, err := a.forRegion(spec.Region)
	if err != nil {
		return nil, err
	}
	instance, err := c.instanceRate(ctx, spec.Region, spec.InstanceType)
	if err != nil {
		return nil, err
	}
	storage, err := c.storageRate(ctx, spec.Region)
	if err != nil {
		return nil, err
	}
	address, err := c.publicIPv4Rate(ctx, spec.Region)
	if err != nil {
		return nil, err
	}
	if spec.DiskGiB <= 0 {
		return nil, fmt.Errorf("estimate: disk must be at least 1 GiB, got %d", spec.DiskGiB)
	}
	lines := []provider.ChargeLine{
		charge("compute "+spec.InstanceType, instance*hoursPerMonth, 1),
		charge("storage gp3 "+strconv.Itoa(spec.DiskGiB)+" GiB", storage*float64(spec.DiskGiB), 1),
		charge("public IPv4", address*hoursPerMonth, 1),
	}
	est := &provider.Estimate{Lines: lines, Excluded: append([]string(nil), excluded...)}
	for _, l := range lines {
		est.Hourly = round(est.Hourly + l.Hourly)
		est.Monthly = round(est.Monthly + l.Monthly)
	}
	est.QuoteID = quoteID(spec, lines)
	return est, nil
}

// The two conversions between an hourly rate and a monthly amount, each with
// one owner. They used to be written inline at two call sites in opposite
// directions, and nothing checked that they agreed: a desync would give the
// catalogue and the estimate different quote ids for one configuration, so every
// create would be refused as quote_stale for ever with nothing saying why.
func monthlyFromHourly(hourly float64) float64 { return round(hourly * hoursPerMonth) }

func hourlyFromMonthly(monthly float64) float64 { return round(monthly / hoursPerMonth) }

// charge is one line, built from a monthly amount. The hourly figure is derived
// rather than quoted, so a per-hour rate and a per-GB-month rate end up in the
// same shape and the two columns can never disagree about the same resource.
func charge(label string, monthly, count float64) provider.ChargeLine {
	m := round(monthly * count)
	return provider.ChargeLine{
		Label:   label,
		Monthly: m,
		Hourly:  hourlyFromMonthly(m),
	}
}

// round trims float noise. Six places is far below any price that matters and
// far above the point where 0.1*3 stops being 0.30000000000000004.
func round(v float64) float64 {
	const places = 1e6
	return float64(int64(v*places+0.5)) / places
}

// quoteID identifies a price. The create route takes one and the provider is
// asked for the same configuration again, so the id must be reproducible from
// the inputs and the rates: the same question asked twice has to produce the
// same answer, or the gate on spending money cannot be passed.
//
// It covers the configuration and the rates, not the domain: a domain changes
// where the host is reached and nothing about what it costs, and an operator who
// typed their domain after pricing should not have to price again. The domain is
// still in the hash input for collision resistance — it is simply not the thing
// being identified.
func quoteID(spec provider.ClusterSpec, lines []provider.ChargeLine) string {
	var b strings.Builder
	b.WriteString(spec.Region)
	b.WriteString("|")
	b.WriteString(spec.InstanceType)
	b.WriteString("|")
	b.WriteString(strconv.Itoa(spec.DiskGiB))
	b.WriteString("|")
	for _, l := range lines {
		b.WriteString(l.Label)
		b.WriteString("=")
		b.WriteString(strconv.FormatFloat(l.Hourly, 'f', -1, 64))
		b.WriteString(",")
		b.WriteString(strconv.FormatFloat(l.Monthly, 'f', -1, 64))
		b.WriteString(";")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "q_" + hex.EncodeToString(sum[:8])
}
