package aws

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"dawnbx/internal/provider"
)

// entry renders one price list entry the way the service does: a JSON document
// per product, inside a JSON array of strings.
func entry(unit, usd string, attrs map[string]string) string {
	if attrs == nil {
		attrs = map[string]string{}
	}
	doc := map[string]any{
		"product":     map[string]any{"attributes": attrs, "productFamily": attrs["productFamily"]},
		"serviceCode": "AmazonEC2",
		"terms": map[string]any{"OnDemand": map[string]any{"t.1": map[string]any{
			"unit": unit,
			"priceDimensions": map[string]any{"d.1": map[string]any{
				"unit":         unit,
				"pricePerUnit": map[string]any{"USD": usd},
			}},
		}}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// An instance is quoted for an hour, for Linux, on shared tenancy, with no
// pre-installed software. The fake answers only when the adapter asked for
// exactly that, so a test that quietly dropped a qualifier would get no price
// rather than the wrong one.
func TestInstanceRateAsksForTheRightProduct(t *testing.T) {
	f := newFake(t, func(action string, body []byte) (int, string) {
		if action != priceAction {
			t.Errorf("unexpected %s", action)
			return 200, "{}"
		}
		m := jsonBody(t, body)
		got := map[string]string{}
		for _, f := range m["Filters"].([]any) {
			kv := f.(map[string]any)
			got[kv["Field"].(string)] = kv["Value"].(string)
		}
		if m["ServiceCode"] != svcEC2 {
			t.Errorf("service = %v", m["ServiceCode"])
		}
		for k, v := range map[string]string{
			"regionCode": "eu-west-1", "instanceType": "t4g.medium", "operatingSystem": "Linux",
			"tenancy": "Shared", "preInstalledSw": "NA", "capacitystatus": "Used",
		} {
			if got[k] != v {
				t.Errorf("filter %s = %q, want %q", k, got[k], v)
			}
		}
		return 200, `{"FormatVersion":"aws_v1","PriceList":[` +
			quoteJSON(entry(unitHours, "0.0368", map[string]string{"instanceType": "t4g.medium"})) + `]}`
	})
	c := priceFor(t, f)
	got, err := c.instanceRate(testContext(t), "eu-west-1", "t4g.medium")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0.0368 {
		t.Errorf("rate = %v", got)
	}
}

// gp3 is sold as storage per GB-month and as provisioned IOPS and throughput in
// the same product family. An estimate that quoted the wrong one would be out by
// three orders of magnitude and still look plausible, so the unit is the test.
func TestStorageRateIgnoresTheIOPSAndThroughputSKUs(t *testing.T) {
	f := newFake(t, func(_ string, body []byte) (int, string) {
		var filterValue string
		for _, x := range jsonBody(t, body)["Filters"].([]any) {
			kv := x.(map[string]any)
			if kv["Field"] == "volumeApiName" {
				filterValue = kv["Value"].(string)
			}
		}
		if filterValue != "gp3" {
			t.Errorf("storage was not asked about gp3: %q", filterValue)
		}
		// The first entry is the expensive one, as the price list orders them.
		return 200, `{"FormatVersion":"aws_v1","PriceList":[` +
			quoteJSON(entry("IOps", "0.005", map[string]string{"volumeApiName": "gp3"})) + `,` +
			quoteJSON(entry("GB-Mo", "0.08", map[string]string{"volumeApiName": "gp3"})) + `]}`
	})
	got, err := priceFor(t, f).storageRate(testContext(t), "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0.08 {
		t.Errorf("storage rate = %v, want the per GB-month rate", got)
	}
}

// A public address is billed whether or not anything is reachable, and it lives
// in a different service from the instance.
func TestPublicIPv4RateComesFromVPC(t *testing.T) {
	f := newFake(t, func(_ string, body []byte) (int, string) {
		m := jsonBody(t, body)
		if m["ServiceCode"] != svcVPC {
			t.Errorf("service = %v, want %v", m["ServiceCode"], svcVPC)
		}
		return 200, `{"FormatVersion":"aws_v1","PriceList":[` +
			quoteJSON(entry(unitHours, "0.005", map[string]string{"productFamily": "Public IPv4 Address"})) + `]}`
	})
	got, err := priceFor(t, f).publicIPv4Rate(testContext(t), "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0.005 {
		t.Errorf("rate = %v", got)
	}
}

// A price the list did not return must be an error, never a zero. A zero line
// reads as free and an operator approves it.
func TestMissingPriceIsAnErrorNotAZero(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) {
		return 200, `{"FormatVersion":"aws_v1","PriceList":[]}`
	})
	if _, err := priceFor(t, f).instanceRate(testContext(t), "eu-west-1", "t9g.large"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// A price list entry that is not the JSON it claims to be is a failure to price,
// not a price of zero.
func TestUnreadablePriceListIsAnError(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) {
		return 200, `{"FormatVersion":"aws_v1","PriceList":["not json"]}`
	})
	if _, err := priceFor(t, f).instanceRate(testContext(t), "eu-west-1", "t4g.medium"); err == nil {
		t.Error("an unreadable price list entry produced a price")
	}
}

// The price list is served from one region whatever the cluster's region is, and
// a request made against the wrong one would be a 403 in production.
func TestPricingClientIsAlwaysUsEast1(t *testing.T) {
	if pricingRegion != "us-east-1" {
		t.Errorf("pricing region = %q", pricingRegion)
	}
	f := newFake(t, func(string, []byte) (int, string) {
		return 200, `{"FormatVersion":"aws_v1","PriceList":[` + quoteJSON(entry(unitHours, "0.01", nil)) + `]}`
	})
	a := newAWS(t, f, func(o *Options) { o.Region = "ap-northeast-1" })
	c, err := a.forRegion("ap-northeast-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.instanceRate(testContext(t), "ap-northeast-1", "t4g.medium"); err != nil {
		t.Fatal(err)
	}
	if got := c.pricing.Options().Region; got != pricingRegion {
		t.Errorf("the pricing client is in %q, want %q", got, pricingRegion)
	}
	if got := c.cfn.Options().Region; got != "ap-northeast-1" {
		t.Errorf("the stack client is in %q, want the cluster's own region", got)
	}
}

// HostSizes is the catalogue, and it is priced in the region that was asked for
// rather than the one the process happens to be configured with.
func TestHostSizesPricesTheRegionItWasAskedFor(t *testing.T) {
	f := newFake(t, func(action string, body []byte) (int, string) {
		if action != priceAction {
			t.Errorf("unexpected %s", action)
			return 200, "{}"
		}
		_ = jsonBody(t, body)
		return 200, `{"FormatVersion":"aws_v1","PriceList":[` +
			quoteJSON(entry(unitHours, "0.1", map[string]string{"regionCode": "any"})) + `]}`
	})
	a := newAWS(t, f, nil)
	sizes, err := a.HostSizes(testContext(t), "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != len(defaultTypes) {
		t.Fatalf("catalogue = %d sizes, want %d", len(sizes), len(defaultTypes))
	}
	if sizes[0].ID != defaultTypes[0] || sizes[0].HourlyUSD != 0.1 {
		t.Errorf("first size = %+v", sizes[0])
	}
	// Monthly is the hourly rate over 730 hours, which is the divisor AWS's own
	// calculator uses and not 30x24.
	if want := 0.1 * hoursPerMonth; sizes[0].MonthlyUSD != round(want) {
		t.Errorf("monthly = %v, want %v", sizes[0].MonthlyUSD, round(want))
	}
	if _, err := a.HostSizes(testContext(t), "mars-central-1"); !isUnavailable(err) {
		t.Errorf("a region this build does not provision in: %v", err)
	}
}

// Regions is the promise Capabilities makes, with no network call behind it: a
// provider that could answer it from an API could also answer it with an empty
// list at 3am.
func TestRegionsMatchThePromise(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	a := newAWS(t, f, nil)
	got, err := a.Regions(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	want := a.Capabilities().Regions
	if len(got) != len(want) {
		t.Fatalf("Regions = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Regions = %v, want %v", got, want)
		}
	}
}

// priceFor builds the adapter's clients against f and hands back the pricing one.
func priceFor(t *testing.T, f *fake) *clients {
	t.Helper()
	a := newAWS(t, f, nil)
	c, err := a.forRegion("eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// priceAction is the X-Amz-Target the pricing client sends.
const priceAction = "AWSPriceListService.GetProducts"

// quoteJSON is one element of the PriceList array, which is an array of JSON
// documents encoded as strings.
func quoteJSON(s string) string { return strconv.Quote(s) }
