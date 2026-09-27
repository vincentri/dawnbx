package aws

import (
	"testing"

	"dawnbx/internal/provider"
)

// The catalogue the operator picks from and the estimate they are billed against
// come from the same rate, but in opposite directions: the catalogue rounds the
// hourly and multiplies up (aws.go), the estimate takes a monthly and divides
// down (estimate.go). Two sites owned that relationship and nothing checked that
// they agreed.
//
// The failure is not a wrong number. A desync gives the two different quote ids
// for one configuration, so POST /v1/clusters refuses with quote_stale and keeps
// refusing - a plausible error, an unchanging cause, and no path for the
// operator to find the real one.

// TestCatalogueAndEstimateAgreeOnHourly is the invariant: for every size the
// operator can pick, the estimate reproduces the catalogue's hourly figure. It
// is the check the issue asks for and nothing had it.
func TestCatalogueAndEstimateAgreeOnHourly(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	a := newAWS(t, f, nil)
	prices(t, f)
	ctx := testContext(t)

	sizes, err := a.HostSizes(ctx, "eu-west-1")
	if err != nil {
		t.Fatalf("HostSizes: %v", err)
	}
	if len(sizes) == 0 {
		t.Fatal("the catalogue is empty, so there is nothing to compare an estimate against")
	}

	for _, s := range sizes {
		est, err := a.Estimate(ctx, provider.ClusterSpec{
			Region: "eu-west-1", InstanceType: s.ID, DiskGiB: 30,
		})
		if err != nil {
			t.Fatalf("Estimate(%s): %v", s.ID, err)
		}
		var compute *provider.ChargeLine
		for i := range est.Lines {
			if est.Lines[i].Label == "compute "+s.ID {
				compute = &est.Lines[i]
				break
			}
		}
		if compute == nil {
			t.Fatalf("no compute line for %s in %+v", s.ID, est.Lines)
		}
		if compute.Hourly != s.HourlyUSD {
			t.Errorf("%s: the catalogue says %.6f/h but the estimate says %.6f/h; "+
				"the quote would never match the catalogue the operator picked from, "+
				"so every create would be refused as stale",
				s.ID, s.HourlyUSD, compute.Hourly)
		}
	}
}

// TestBothDirectionsRoundTrip is the structural half. If the two conversions are
// one function rather than two arbitrary ones, they cannot drift; this pins that
// a value survives the round trip through both.
func TestBothDirectionsRoundTrip(t *testing.T) {
	for _, rate := range []float64{0.04161, 0.0424, 0.0098765, 0.1, 0.007, 0.123456} {
		back := hourlyFromMonthly(monthlyFromHourly(rate))
		if back != round(rate) {
			t.Errorf("rate %.8f goes to %.6f/mo and back to %.6f/h, want %.6f; "+
				"the two directions are not one function", rate, monthlyFromHourly(rate), back, round(rate))
		}
	}
}

// TestCatalogueMonthlyComesFromTheHourly: the catalogue's monthly column must
// be derived, not quoted, so it cannot disagree with the hourly beside it.
func TestCatalogueMonthlyComesFromTheHourly(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	a := newAWS(t, f, nil)
	prices(t, f)

	sizes, err := a.HostSizes(testContext(t), "eu-west-1")
	if err != nil {
		t.Fatalf("HostSizes: %v", err)
	}
	for _, s := range sizes {
		if want := monthlyFromHourly(s.HourlyUSD); s.MonthlyUSD != want {
			t.Errorf("%s: monthly is %.6f, want %.6f derived from the hourly",
				s.ID, s.MonthlyUSD, want)
		}
	}
}
