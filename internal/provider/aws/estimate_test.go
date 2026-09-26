package aws

import (
	"strconv"
	"strings"
	"testing"

	"dawnbx/internal/provider"
)

// prices answers the three rates an estimate needs, with the real eu-west-1
// on-demand numbers a t4g.medium host costs.
func prices(t *testing.T, f *fake) {
	t.Helper()
	f.reply = func(action string, body []byte) (int, string) {
		if action != priceAction {
			return 200, "{}"
		}
		volumes, family := false, ""
		for _, x := range jsonBody(t, body)["Filters"].([]any) {
			kv := x.(map[string]any)
			switch kv["Field"] {
			case "volumeApiName":
				volumes = true
			case "productFamily":
				family, _ = kv["Value"].(string)
			}
		}
		switch {
		case volumes:
			return 200, `{"PriceList":[` + quoteJSON(entry(unitGBMon, "0.08", nil)) + `]}`
		case family == "Public IPv4 Address":
			return 200, `{"PriceList":[` + quoteJSON(entry(unitHours, "0.005", nil)) + `]}`
		}
		return 200, `{"PriceList":[` + quoteJSON(entry(unitHours, "0.0368", nil)) + `]}`
	}
}

// An estimate is a table an operator is about to approve, so the three claims it
// makes are checked: the fixed charges are there, they add up to the total, and
// the charges that are deliberately missing are named rather than implied.
func TestEstimateLinesAndExcluded(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	a := newAWS(t, f, nil)
	prices(t, f)

	est, err := a.Estimate(testContext(t), provider.ClusterSpec{
		Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(est.Lines) != 3 {
		t.Fatalf("lines = %d, want compute, storage and the address", len(est.Lines))
	}
	labels := make([]string, 0, 3)
	var sum float64
	for _, l := range est.Lines {
		labels = append(labels, l.Label)
		sum += l.Monthly
		if l.Monthly <= 0 || l.Hourly <= 0 {
			t.Errorf("line %+v is free", l)
		}
	}
	want := []string{"compute t4g.medium", "storage gp3 30 GiB", "public IPv4"}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, labels[i], want[i])
		}
	}
	// The total is the sum of what is shown. A total that does not add up is the
	// kind of thing an operator notices and then stops believing.
	if est.Monthly != round(sum) {
		t.Errorf("monthly total = %v, want the sum of the lines %v", est.Monthly, sum)
	}
	var hourly float64
	for _, l := range est.Lines {
		hourly += l.Hourly
	}
	if est.Hourly != round(hourly) {
		t.Errorf("hourly total = %v, want the sum of the lines %v", est.Hourly, hourly)
	}
	// 0.0368/h for 730 h, 30 GB of gp3, and one address at 0.005/h for 730 h.
	if want := 0.0368*hoursPerMonth + 30*0.08 + 0.005*hoursPerMonth; est.Monthly < want-0.01 || est.Monthly > want+0.01 {
		t.Errorf("monthly = %v, want about %v", est.Monthly, want)
	}
	if strings.Join(est.Excluded, ",") != "data_transfer,taxes,provider_discounts" {
		t.Errorf("excluded = %v, want the three named charges", est.Excluded)
	}
}

// The quote id is the gate on spending money: the control plane prices a
// configuration, takes the id back from the operator and passes it to Create. It
// has to be reproducible for the same question, and it has to move when the price
// or the configuration does.
func TestQuoteIDIdentifiesAPrice(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	a := newAWS(t, f, nil)
	prices(t, f)
	spec := provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30, Domain: "team.example.com"}

	first, err := a.Estimate(testContext(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	// Same question, different domain: the domain changes where the host is
	// reached and nothing about what it costs.
	second, err := a.Estimate(testContext(t), provider.ClusterSpec{
		Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30, Domain: "other.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.QuoteID != second.QuoteID {
		t.Errorf("the same configuration quoted twice gave %q and %q", first.QuoteID, second.QuoteID)
	}
	if !strings.HasPrefix(first.QuoteID, "q_") {
		t.Errorf("quote id = %q, want a recognisable prefix", first.QuoteID)
	}

	for _, c := range []struct {
		name string
		spec provider.ClusterSpec
	}{
		{"another region", provider.ClusterSpec{Region: "us-west-2", InstanceType: "t4g.medium", DiskGiB: 30}},
		{"another size", provider.ClusterSpec{Region: "eu-west-1", InstanceType: "m7g.large", DiskGiB: 30}},
		{"another disk", provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 60}},
	} {
		t.Run(c.name, func(t *testing.T) {
			other, err := a.Estimate(testContext(t), c.spec)
			if err != nil {
				t.Fatal(err)
			}
			if other.QuoteID == first.QuoteID {
				t.Errorf("%s quoted the same as the original", c.name)
			}
		})
	}

	// A price change has to move the id too, or a quote would keep authorising a
	// cluster at a price the operator never agreed to.
	// The same configuration at a different compute rate.
	f.reply = func(action string, body []byte) (int, string) {
		if action != priceAction {
			return 200, "{}"
		}
		if hasFilter(t, body, "volumeApiName") {
			return 200, `{"PriceList":[` + quoteJSON(entry(unitGBMon, "0.08", nil)) + `]}`
		}
		return 200, `{"PriceList":[` + quoteJSON(entry(unitHours, "0.09", nil)) + `]}`
	}
	third, err := a.Estimate(testContext(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	if third.QuoteID == first.QuoteID {
		t.Error("a price change did not move the quote id")
	}
}

// hasFilter reports whether a pricing request asked about one of these fields.
func hasFilter(t *testing.T, body []byte, field string) bool {
	t.Helper()
	for _, x := range jsonBody(t, body)["Filters"].([]any) {
		if x.(map[string]any)["Field"] == field {
			return true
		}
	}
	return false
}

// An estimate that cannot price is an error. A configuration with no disk is not
// a free cluster, it is a mistake, and the create route would reject it anyway.
func TestEstimateRefusesAConfigurationItCannotPrice(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	a := newAWS(t, f, nil)
	prices(t, f)
	if _, err := a.Estimate(testContext(t), provider.ClusterSpec{Region: "eu-west-1", InstanceType: "t4g.medium"}); err == nil {
		t.Error("an estimate with no disk succeeded")
	}
	if _, err := a.Estimate(testContext(t), provider.ClusterSpec{Region: "mars", InstanceType: "t4g.medium", DiskGiB: 30}); !isUnavailable(err) {
		t.Errorf("a region this build does not provision in: %v", err)
	}
}

// A storage line is derived from the monthly rate, so the two columns cannot
// disagree about the same resource, and both are rounded to the same place.
func TestCharge(t *testing.T) {
	c := charge("storage gp3 30 GiB", 0.08*30, 1)
	if c.Monthly != 2.4 {
		t.Errorf("monthly = %v", c.Monthly)
	}
	if c.Hourly != round(2.4/hoursPerMonth) {
		t.Errorf("hourly = %v", c.Hourly)
	}
	if got := charge("x", 0, 3); got.Monthly != 0 || got.Hourly != 0 {
		t.Errorf("a free line is not zero: %+v", got)
	}
	// 730, not 30x24: a month is what AWS says it is.
	if hoursPerMonth != 730 {
		t.Errorf("hoursPerMonth = %v", hoursPerMonth)
	}
	if strconv.Itoa(int(hoursPerMonth)) == "720" {
		t.Error("the monthly divisor is a calendar approximation")
	}
}
