package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricetypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"dawnbx/internal/provider"
)

// The price list is one global service with regional filters, and it is only
// served out of us-east-1. That is why the pricing client is built from a copy of
// the config with the region replaced: the cluster is provisioned wherever the
// operator said, and its price is still read from the one place the list lives.
const pricingRegion = "us-east-1"

// Service codes from the price list API, not from the SDK: the SDK ships no
// enumeration of them because there are hundreds and they change independently
// of the SDK.
const (
	svcEC2 = "AmazonEC2"
	svcVPC = "AmazonVPC"
)

// The three fixed charges a dawnbx host has. Everything else a cluster costs is
// traffic, tax or a discount, and all three are named in the estimate's Excluded
// list rather than guessed at.
const (
	unitHours = "Hrs"
	unitGBMon = "GB-Mo"
)

// priceList is the slice of a price list entry this adapter reads. GetProducts
// answers with a JSON document per product and the whole catalogue for a region
// is far too much to model; the attributes and the first on-demand term are all
// the price needs.
type priceList struct {
	Product struct {
		Attributes map[string]string `json:"attributes"`
		ProductFam string            `json:"productFamily"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			Unit            string `json:"unit"`
			PriceDimensions map[string]struct {
				Unit         string `json:"unit"`
				PricePerUnit struct {
					USD string `json:"USD"`
				} `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// rate returns the on-demand USD rate for the given unit, or false when this
// product is not the one being asked for. The unit is the test, and it is
// necessary: gp3 is sold as storage (GB-Mo) and as provisioned IOPS and
// throughput in the same product family, and an estimate that quoted the IOPS
// rate as the disk rate would be wrong by three orders of magnitude and still
// look plausible.
func (p priceList) rate(unit string) (float64, bool) {
	for _, term := range p.Terms.OnDemand {
		if term.Unit != unit {
			continue
		}
		for _, dim := range term.PriceDimensions {
			if dim.Unit != "" && dim.Unit != unit {
				continue
			}
			v, err := strconv.ParseFloat(dim.PricePerUnit.USD, 64)
			if err != nil || v < 0 {
				continue
			}
			return v, true
		}
	}
	return 0, false
}

// filters builds a TERM_MATCH filter set. The price list is queried, never
// downloaded, so every call has to be narrow enough to return one product.
func filters(region string, kv ...string) []pricetypes.Filter {
	out := make([]pricetypes.Filter, 0, len(kv)/2+1)
	out = append(out, pricetypes.Filter{
		Type:  pricetypes.FilterTypeTermMatch,
		Field: aws.String("regionCode"),
		Value: aws.String(region),
	})
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, pricetypes.Filter{
			Type:  pricetypes.FilterTypeTermMatch,
			Field: aws.String(kv[i]),
			Value: aws.String(kv[i+1]),
		})
	}
	return out
}

// lowestRate is the whole price lookup: ask for products matching the filters,
// take the lowest that quotes the unit asked for, and fail loudly if none does.
// A silently missing price is worse than an error here, because an estimate
// with a zero line reads as free and an operator approves it.
func (c *clients) lowestRate(ctx context.Context, service, region, unit string, f []pricetypes.Filter) (float64, error) {
	out, err := c.pricing.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode:   aws.String(service),
		Filters:       f,
		MaxResults:    aws.Int32(100),
		FormatVersion: aws.String("aws_v1"),
	})
	if err != nil {
		return 0, fmt.Errorf("price %s in %s: %w", service, region, err)
	}
	// The lowest matching rate, not the first. A price list carries 1-year and
	// 3-year terms, savings plans and capacity reservations side by side, so
	// first-match is whichever one the API happened to order first — and
	// research D9 promised the lowest. The filters above already pin region,
	// tenancy, OS and capacity status, so this only chooses between terms.
	lowest := -1.0
	for _, raw := range out.PriceList {
		var p priceList
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return 0, fmt.Errorf("price %s in %s: unreadable price list entry: %w", service, region, err)
		}
		if v, ok := p.rate(unit); ok && (lowest < 0 || v < lowest) {
			lowest = v
		}
	}
	if lowest >= 0 {
		return lowest, nil
	}
	return 0, fmt.Errorf("%w: no %s price for %s (%s)", provider.ErrNotFound, unit, region, service)
}

// instanceRate is the on-demand hourly price of one instance type, Linux, shared
// tenancy, no pre-installed software. Every one of those qualifiers is a filter
// rather than an assumption: the list also holds Windows, dedicated-host and
// software-licensed variants of the same instance type at very different
// prices, and quoting one of those would be quoting a different machine.
func (c *clients) instanceRate(ctx context.Context, region, instanceType string) (float64, error) {
	return c.lowestRate(ctx, svcEC2, region, unitHours, filters(region,
		"instanceType", instanceType,
		"operatingSystem", "Linux",
		"tenancy", "Shared",
		"preInstalledSw", "NA",
		"capacitystatus", "Used",
	))
}

// storageRate is gp3 per GB-month. The root disk is priced by capacity and
// nothing else; the instance's own IOPS and throughput are included at the size
// the instance type brings.
func (c *clients) storageRate(ctx context.Context, region string) (float64, error) {
	return c.lowestRate(ctx, svcEC2, region, unitGBMon, filters(region,
		"volumeApiName", "gp3",
		"productFamily", "Storage",
	))
}

// publicIPv4Rate is the hourly charge for one public IPv4 address. It is billed
// whether or not the address is reachable, so a host with an Elastic IP is
// charged twice for the same thing and only one of them is a line item an
// operator can predict.
func (c *clients) publicIPv4Rate(ctx context.Context, region string) (float64, error) {
	return c.lowestRate(ctx, svcVPC, region, unitHours, filters(region,
		"productFamily", "Public IPv4 Address",
	))
}
