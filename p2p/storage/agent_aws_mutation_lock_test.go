package storage

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

func TestPostgresAcquireProvisionMutationUsesDurableLeaseAndReleases(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	r, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM core_aws_ec2_provisions`).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`INSERT INTO core_aws_ec2_provision_mutation_leases`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s).*UPDATE core_aws_ec2_provision_mutation_leases SET token=\$1.*`).WillReturnRows(sqlmock.NewRows([]string{"epoch"}).AddRow(int64(1)))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(`(?s).*UPDATE core_aws_ec2_provision_mutation_leases SET expires_at=\$1.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM core_aws_ec2_provision_mutation_leases`).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectBegin()
	mock.ExpectExec(`(?s).*UPDATE core_aws_ec2_provision_mutation_leases SET token=NULL.*`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	lease, err := r.AcquireProvisionMutation(context.Background(), testAWSProvisionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Assert(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The release closure is idempotent and must not retain a connection.
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresProvisionMutationWrongTokenReleaseDoesNotClearLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(`(?s).*UPDATE core_aws_ec2_provision_mutation_leases SET token=NULL.*`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := r.releaseProvisionMutation(context.Background(), testAWSProvisionID, "11111111-1111-4111-8111-111111111111", 1); err != agentaws.ErrConflict {
		t.Fatalf("wrong-token release = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresProvisionMutationClaimRefreshesExpiredFenceAndBindsOperation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "66666666-6666-4666-8666-666666666666"
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s).*UPDATE core_aws_ec2_provision_mutation_leases SET token=\$1,epoch=epoch\+1,state='active',expires_at=\$5.*`).
		WithArgs(sqlmock.AnyArg(), testAWSOwnerID, testAWSProvisionID, operationID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"epoch"}).AddRow(int64(3)))
	mock.ExpectCommit()
	mock.ExpectExec(`(?s).*UPDATE core_aws_ec2_provision_mutation_leases SET operation_id=\$1.*`).
		WithArgs(operationID, sqlmock.AnyArg(), testAWSOwnerID, testAWSProvisionID, sqlmock.AnyArg(), int64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	lease, err := r.ClaimProvisionMutation(context.Background(), testAWSProvisionID, operationID)
	if err != nil {
		t.Fatal(err)
	}
	binder, ok := lease.(agentaws.ProvisionMutationOperationBinder)
	if !ok {
		t.Fatal("postgres lease does not expose operation binder")
	}
	if err := binder.BindOperation(context.Background(), operationID); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
