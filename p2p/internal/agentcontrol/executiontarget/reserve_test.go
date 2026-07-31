package executiontarget

import (
	"context"
	"errors"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type reservationCatalogFake struct {
	offer ReservationOffer
	calls int
}

func (f *reservationCatalogFake) ResolveReservation(_ context.Context, _ coreaws.Credentials, instanceType string, volumeGiB uint32) (ReservationOffer, error) {
	f.calls++
	offer := f.offer
	offer.InstanceType = instanceType
	offer.VolumeGiB = volumeGiB
	return offer, nil
}

func TestReserveCreatesImmutableServerOwnedRevisionAndReplays(t *testing.T) {
	now := time.Date(2036, 1, 2, 3, 4, 5, 0, time.UTC)
	credential := coreaws.RehydrateCredentials(importCredential, "reserve", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/reserve", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("secret-value"), nil, 4, 4, now, now)
	store := &importTargetStore{observations: map[string]storage.TargetObservationRecord{}}
	credentials := &importCredentialStore{credential: credential}
	catalog := &reservationCatalogFake{offer: ReservationOffer{
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AMIParameter:            coreexecution.AWSAL2023X8664AMIParameter, Architecture: "x86_64", AvailabilityZone: "us-east-1a",
		ManagementTransport: "aws_ssm", PublicIP: true, CostQuote: coreexecution.CostQuote{Amount: "0.02", Currency: "USD", ExpiresAt: now.Add(time.Hour)},
	}}
	service := New(Config{Targets: store, Credentials: credentials, Reservations: catalog, Now: func() time.Time { return now }})
	req := ReserveRequest{CredentialID: importCredential, CredentialRevision: 4, InstanceType: "t3.small", VolumeGiB: 20, IdempotencyKey: importKey}
	first, err := service.Reserve(context.Background(), importOwner, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != coreexecution.TargetKindAWSComputeReservation || first.Revision != 1 || !first.Digest.Valid() ||
		first.AccountID != credential.AccountID || first.Region != credential.Region || first.InfrastructureProfileID != coreaws.InfrastructureProfileGeneralLinuxSSMV1 ||
		first.Network.Mode != "restricted" || first.ComputeReservation == nil || !first.ComputeReservation.PublicIP || first.ComputeReservation.PublicInbound ||
		first.ComputeReservation.InstanceType != "t3.small" || first.ComputeReservation.VolumeGiB != 20 || len(first.CredentialRefs) != 1 || !first.CredentialRefs[0].BindingDigest.Valid() {
		t.Fatalf("target=%+v", first)
	}
	replay, err := service.Reserve(context.Background(), importOwner, req)
	if err != nil || replay.Digest != first.Digest || credentials.calls != 1 || catalog.calls != 1 || store.createTargets != 1 {
		t.Fatalf("replay=%+v err=%v calls=%d/%d/%d", replay, err, credentials.calls, catalog.calls, store.createTargets)
	}
	req.InstanceType = "m7i.large"
	if _, err := service.Reserve(context.Background(), importOwner, req); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("drift err=%v", err)
	}
	if credentials.calls != 1 || catalog.calls != 1 {
		t.Fatal("idempotency drift reached mutable dependencies")
	}
}

func TestReserveRejectsCatalogThatWidensNetwork(t *testing.T) {
	now := time.Date(2036, 1, 2, 3, 4, 5, 0, time.UTC)
	credential := coreaws.RehydrateCredentials(importCredential, "reserve", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/reserve", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("secret-value"), nil, 4, 4, now, now)
	store := &importTargetStore{observations: map[string]storage.TargetObservationRecord{}}
	catalog := &reservationCatalogFake{offer: ReservationOffer{InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1, AMIParameter: coreexecution.AWSAL2023X8664AMIParameter, Architecture: "x86_64", ManagementTransport: "aws_ssm", PublicIP: true, PublicInbound: true, CostQuote: coreexecution.CostQuote{Amount: "0.02", Currency: "USD", ExpiresAt: now.Add(time.Hour)}}}
	service := New(Config{Targets: store, Credentials: &importCredentialStore{credential: credential}, Reservations: catalog, Now: func() time.Time { return now }})
	_, err := service.Reserve(context.Background(), importOwner, ReserveRequest{CredentialID: importCredential, CredentialRevision: 4, InstanceType: "t3.small", VolumeGiB: 20, IdempotencyKey: importKey})
	if !errors.Is(err, ErrTargetUnavailable) || store.createTargets != 0 {
		t.Fatalf("err=%v writes=%d", err, store.createTargets)
	}
}
