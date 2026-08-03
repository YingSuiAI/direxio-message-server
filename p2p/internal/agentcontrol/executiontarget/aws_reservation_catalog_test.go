package executiontarget

import (
	"context"
	"strings"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

type priceListFamilyFixture struct{}

func (priceListFamilyFixture) GetProducts(context.Context, *pricing.GetProductsInput, ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	return &pricing.GetProductsOutput{PriceList: []string{`{
        "product":{"productFamily":"Storage","attributes":{"volumeApiName":"gp3","regionCode":"us-east-1"}},
        "terms":{"OnDemand":{"term":{"priceDimensions":{"dimension":{"unit":"GB-Mo","pricePerUnit":{"USD":"0.0800000000"}}}}}}
    }`}}, nil
}

func TestQueryUniquePriceReadsAWSProductFamilyFromProduct(t *testing.T) {
	price, err := queryUniquePrice(context.Background(), priceListFamilyFixture{}, map[string]string{
		"volumeApiName": "gp3", "productFamily": "Storage", "regionCode": "us-east-1",
	}, "GB-Mo")
	if err != nil || price == nil || price.FloatString(2) != "0.08" {
		t.Fatalf("price=%v err=%v", price, err)
	}
}

type reservationCatalogEC2Fake struct {
	locationType ec2types.LocationType
	locations    []string
}

func (f *reservationCatalogEC2Fake) DescribeInstanceTypeOfferings(_ context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	f.locationType = in.LocationType
	locations := f.locations
	if len(locations) == 0 {
		locations = []string{"us-east-1b", "us-east-1a"}
	}
	offerings := make([]ec2types.InstanceTypeOffering, 0, len(locations))
	for _, location := range locations {
		offerings = append(offerings, ec2types.InstanceTypeOffering{InstanceType: ec2types.InstanceTypeT3Small, Location: awsapi.String(location)})
	}
	return &ec2.DescribeInstanceTypeOfferingsOutput{InstanceTypeOfferings: offerings}, nil
}

func (*reservationCatalogEC2Fake) DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []ec2types.InstanceTypeInfo{{
		InstanceType:  ec2types.InstanceTypeT3Small,
		ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{ec2types.ArchitectureTypeX8664}},
		VCpuInfo:      &ec2types.VCpuInfo{DefaultVCpus: awsapi.Int32(2)},
		MemoryInfo:    &ec2types.MemoryInfo{SizeInMiB: awsapi.Int64(2048)},
	}}}, nil
}

type reservationCatalogPricingFake struct{}

func (reservationCatalogPricingFake) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	storage := false
	for _, filter := range in.Filters {
		if strings.TrimSpace(awsapi.ToString(filter.Field)) == "volumeApiName" {
			storage = true
		}
	}
	if storage {
		return &pricing.GetProductsOutput{PriceList: []string{`{"product":{"productFamily":"Storage","attributes":{"volumeApiName":"gp3","regionCode":"us-east-1"}},"terms":{"OnDemand":{"term":{"priceDimensions":{"dimension":{"unit":"GB-Mo","pricePerUnit":{"USD":"0.0800000000"}}}}}}}`}}, nil
	}
	return &pricing.GetProductsOutput{PriceList: []string{`{"product":{"productFamily":"Compute Instance","attributes":{"instanceType":"t3.small","operatingSystem":"Linux","tenancy":"Shared","preInstalledSw":"NA","capacitystatus":"Used","regionCode":"us-east-1"}},"terms":{"OnDemand":{"term":{"priceDimensions":{"dimension":{"unit":"Hrs","pricePerUnit":{"USD":"0.0200000000"}}}}}}}`}}, nil
}

type reservationCatalogFactoryFake struct {
	ec2     *reservationCatalogEC2Fake
	pricing reservationCatalogPricingFake
}

func (f reservationCatalogFactoryFake) NewEC2Reservation(coreaws.Credentials) (coreaws.EC2ReservationClient, error) {
	return f.ec2, nil
}
func (f reservationCatalogFactoryFake) NewPricing(coreaws.Credentials) (coreaws.PricingClient, error) {
	return f.pricing, nil
}

func TestAWSReservationCatalogPinsDeterministicAvailableZone(t *testing.T) {
	now := time.Date(2036, 1, 2, 3, 4, 5, 0, time.UTC)
	credential := coreaws.RehydrateCredentials("11111111-1111-4111-8111-111111111111", "catalog", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/catalog", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("secret-value"), nil, 4, 4, now, now)
	ec2Fake := &reservationCatalogEC2Fake{}
	catalog := NewAWSReservationCatalog(reservationCatalogFactoryFake{ec2: ec2Fake}, func() time.Time { return now })
	offer, err := catalog.ResolveReservation(context.Background(), credential, "t3.small", 20)
	if err != nil || offer.AvailabilityZone != "us-east-1a" || ec2Fake.locationType != ec2types.LocationTypeAvailabilityZone {
		t.Fatalf("offer=%+v err=%v location_type=%s", offer, err, ec2Fake.locationType)
	}
}

func TestAWSReservationCatalogRejectsUnsafeAvailabilityZone(t *testing.T) {
	credential := coreaws.RehydrateCredentials("11111111-1111-4111-8111-111111111111", "catalog", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/catalog", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("secret-value"), nil, 4, 4, time.Now(), time.Now())
	ec2Fake := &reservationCatalogEC2Fake{locations: []string{"us-west-2a"}}
	catalog := NewAWSReservationCatalog(reservationCatalogFactoryFake{ec2: ec2Fake}, time.Now)
	if _, err := catalog.ResolveReservation(context.Background(), credential, "t3.small", 20); err == nil {
		t.Fatal("cross-region availability zone accepted")
	}
}
