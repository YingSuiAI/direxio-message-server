package aws

import "testing"

func saveVerifiedCredential(t *testing.T, service *Service, repo *MemoryRepository, in CredentialInput) (CredentialView, error) {
	t.Helper()
	view, err := service.SaveCredential(t.Context(), in)
	if err != nil {
		return CredentialView{}, err
	}
	_, err = repo.RecordCredentialIdentity(t.Context(), view.ID, view.Revision, Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/test"})
	if err != nil {
		return CredentialView{}, err
	}
	credential, err := repo.GetCredential(t.Context(), view.ID)
	if err != nil {
		return CredentialView{}, err
	}
	return credential.View(), nil
}
