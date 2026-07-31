package executiontarget

// This file is the production reservation catalog. It verifies the selected
// instance against the live EC2 regional catalog and obtains a bounded quote
// from the signed AWS Price List Query API. A missing/unsupported pricing
// endpoint is a hard readiness/lookup failure; no static or zero-price
// fallback is permitted.

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

var ErrReservationCatalogUnavailable = errors.New("execution target: live aws reservation catalog unavailable")

const (
	pricingServiceCode = "AmazonEC2"
	pricingEndpoint    = "us-east-1"
	defaultQuoteTTL    = 15 * time.Minute
	maxPricingPages    = 5
	maxOfferingPages   = 5
)

// AWSReservationCatalog is intentionally concrete. Tests may provide SDK
// clients through the factory seam, while production uses SDKFactory and
// signed AWS requests for both availability and price.
type AWSReservationCatalog struct {
	factory coreaws.ReservationClientFactory
	now     func() time.Time
	ttl     time.Duration
	timeout time.Duration
}

func NewAWSReservationCatalog(factory coreaws.ReservationClientFactory, now func() time.Time) *AWSReservationCatalog {
	if factory == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &AWSReservationCatalog{factory: factory, now: now, ttl: defaultQuoteTTL, timeout: 30 * time.Second}
}

func (c *AWSReservationCatalog) Ready() bool {
	return c != nil && c.factory != nil && c.now != nil && c.ttl > 0 && c.ttl <= time.Hour && c.timeout > 0
}

func (c *AWSReservationCatalog) ResolveReservation(ctx context.Context, credential coreaws.Credentials, instanceType string, volumeGiB uint32) (ReservationOffer, error) {
	var zero ReservationOffer
	instanceType = strings.TrimSpace(instanceType)
	if !c.Ready() || credential.Validate() != nil || credential.AccountID == "" || credential.Region == "" ||
		credential.VerifiedRevision != credential.Revision || !reservationInstanceType.MatchString(instanceType) || volumeGiB < 8 || volumeGiB > 16384 ||
		!pricingSupportsRegion(credential.Region) {
		return zero, ErrReservationCatalogUnavailable
	}
	ec2c, err := c.factory.NewEC2Reservation(credential)
	if err != nil || ec2c == nil {
		return zero, ErrReservationCatalogUnavailable
	}
	pricingClient, err := c.factory.NewPricing(credential)
	if err != nil || pricingClient == nil {
		return zero, ErrReservationCatalogUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var nextOfferingToken *string
	availabilityZones := map[string]struct{}{}
	for page := 0; page < maxOfferingPages; page++ {
		offerings, callErr := ec2c.DescribeInstanceTypeOfferings(callCtx, &ec2.DescribeInstanceTypeOfferingsInput{
			LocationType: ec2types.LocationTypeAvailabilityZone,
			Filters:      []ec2types.Filter{{Name: awsapi.String("instance-type"), Values: []string{instanceType}}},
			NextToken:    nextOfferingToken,
		})
		if callErr != nil || offerings == nil {
			return zero, ErrReservationCatalogUnavailable
		}
		for _, offering := range offerings.InstanceTypeOfferings {
			if string(offering.InstanceType) != instanceType {
				return zero, ErrReservationCatalogUnavailable
			}
			location := strings.TrimSpace(awsapi.ToString(offering.Location))
			if !coreexecution.ValidateAvailabilityZone(credential.Region, location) {
				return zero, ErrReservationCatalogUnavailable
			}
			availabilityZones[location] = struct{}{}
		}
		nextOfferingToken = offerings.NextToken
		if strings.TrimSpace(awsapi.ToString(nextOfferingToken)) == "" {
			break
		}
		if page == maxOfferingPages-1 {
			return zero, ErrReservationCatalogUnavailable
		}
	}
	if len(availabilityZones) == 0 {
		return zero, ErrReservationCatalogUnavailable
	}
	locations := make([]string, 0, len(availabilityZones))
	for location := range availabilityZones {
		locations = append(locations, location)
	}
	sort.Strings(locations)
	availabilityZone := locations[0]
	typesOut, err := ec2c.DescribeInstanceTypes(callCtx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []ec2types.InstanceType{ec2types.InstanceType(instanceType)}})
	if err != nil || typesOut == nil || len(typesOut.InstanceTypes) != 1 || string(typesOut.InstanceTypes[0].InstanceType) != instanceType ||
		typesOut.InstanceTypes[0].ProcessorInfo == nil || !containsArchitecture(typesOut.InstanceTypes[0].ProcessorInfo.SupportedArchitectures, ec2types.ArchitectureTypeX8664) ||
		typesOut.InstanceTypes[0].VCpuInfo == nil || awsapi.ToInt32(typesOut.InstanceTypes[0].VCpuInfo.DefaultVCpus) <= 0 ||
		typesOut.InstanceTypes[0].MemoryInfo == nil || awsapi.ToInt64(typesOut.InstanceTypes[0].MemoryInfo.SizeInMiB) <= 0 {
		return zero, ErrReservationCatalogUnavailable
	}
	compute, err := queryUniquePrice(callCtx, pricingClient, map[string]string{
		"instanceType": instanceType, "operatingSystem": "Linux", "tenancy": "Shared", "preInstalledSw": "NA",
		"capacitystatus": "Used", "regionCode": credential.Region,
	}, "Hrs")
	if err != nil {
		return zero, ErrReservationCatalogUnavailable
	}
	storage, err := queryUniquePrice(callCtx, pricingClient, map[string]string{
		"volumeApiName": "gp3", "productFamily": "Storage", "regionCode": credential.Region,
	}, "GB-Mo")
	if err != nil {
		return zero, ErrReservationCatalogUnavailable
	}
	// AWS publishes gp3 storage in GB-month. A 730-hour billing month converts
	// it to the same hourly unit as the instance quote. The amount is the exact
	// selected instance plus selected root-volume hourly estimate.
	storage.Mul(storage, new(big.Rat).SetInt64(int64(volumeGiB)))
	storage.Quo(storage, new(big.Rat).SetInt64(730))
	total := new(big.Rat).Add(compute, storage)
	amount := decimalAmount(total)
	if amount == "" {
		return zero, ErrReservationCatalogUnavailable
	}
	now := c.now().UTC().Truncate(time.Microsecond)
	return ReservationOffer{
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AMIParameter:            coreexecution.AWSAL2023X8664AMIParameter,
		InstanceType:            instanceType,
		AvailabilityZone:        availabilityZone,
		VolumeGiB:               volumeGiB,
		Architecture:            "x86_64",
		ManagementTransport:     "aws_ssm",
		PublicIP:                true,
		PublicInbound:           false,
		CostQuote: coreexecution.CostQuote{
			Amount: amount, Currency: "USD", ExpiresAt: now.Add(c.ttl),
		},
	}, nil
}

func pricingSupportsRegion(region string) bool {
	region = strings.TrimSpace(region)
	if region == "" || strings.HasPrefix(region, "cn-") || strings.HasPrefix(region, "us-gov-") ||
		strings.HasPrefix(region, "us-iso-") || strings.HasPrefix(region, "us-isob-") || strings.HasPrefix(region, "us-isof-") || strings.HasPrefix(region, "eu-isoe-") {
		return false
	}
	return pricingEndpoint == "us-east-1"
}

func containsArchitecture(values []ec2types.ArchitectureType, want ec2types.ArchitectureType) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type priceDocument struct {
	Product struct {
		ProductFamily string            `json:"productFamily"`
		Attributes    map[string]string `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

func queryUniquePrice(ctx context.Context, client coreaws.PricingClient, attributes map[string]string, unit string) (*big.Rat, error) {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	filters := make([]pricingtypes.Filter, 0, len(keys))
	for _, key := range keys {
		filters = append(filters, pricingtypes.Filter{Field: awsapi.String(key), Type: pricingtypes.FilterTypeTermMatch, Value: awsapi.String(attributes[key])})
	}
	var next *string
	prices := map[string]*big.Rat{}
	for page := 0; page < maxPricingPages; page++ {
		out, err := client.GetProducts(ctx, &pricing.GetProductsInput{ServiceCode: awsapi.String(pricingServiceCode), Filters: filters, FormatVersion: awsapi.String("aws_v1"), MaxResults: awsapi.Int32(100), NextToken: next})
		if err != nil || out == nil {
			return nil, ErrReservationCatalogUnavailable
		}
		for _, raw := range out.PriceList {
			var doc priceDocument
			if json.Unmarshal([]byte(raw), &doc) != nil {
				return nil, ErrReservationCatalogUnavailable
			}
			for key, want := range attributes {
				actual := doc.Product.Attributes[key]
				// In AWS Price List documents productFamily is a top-level
				// product field even though it is selected with an ordinary
				// TERM_MATCH filter. Requiring it inside attributes rejects the
				// real signed gp3 response and makes every reservation fail.
				if key == "productFamily" {
					actual = doc.Product.ProductFamily
				}
				if actual != want {
					return nil, ErrReservationCatalogUnavailable
				}
			}
			for _, term := range doc.Terms.OnDemand {
				for _, dimension := range term.PriceDimensions {
					if dimension.Unit != unit {
						continue
					}
					value := strings.TrimSpace(dimension.PricePerUnit["USD"])
					rat, ok := new(big.Rat).SetString(value)
					if !ok || rat.Sign() <= 0 {
						return nil, ErrReservationCatalogUnavailable
					}
					prices[rat.RatString()] = rat
				}
			}
		}
		if strings.TrimSpace(awsapi.ToString(out.NextToken)) == "" {
			break
		}
		next = out.NextToken
		if page == maxPricingPages-1 {
			return nil, ErrReservationCatalogUnavailable
		}
	}
	if len(prices) != 1 {
		return nil, ErrReservationCatalogUnavailable
	}
	for _, price := range prices {
		return new(big.Rat).Set(price), nil
	}
	return nil, ErrReservationCatalogUnavailable
}

func decimalAmount(value *big.Rat) string {
	if value == nil || value.Sign() <= 0 {
		return ""
	}
	result := strings.TrimRight(strings.TrimRight(value.FloatString(10), "0"), ".")
	if result == "" || result == "0" {
		return ""
	}
	return result
}
