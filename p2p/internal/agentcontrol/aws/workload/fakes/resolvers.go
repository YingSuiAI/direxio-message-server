// Package fakes provides deterministic Agent-side credential/secret resolvers
// for provider acceptance tests. It never stores raw values in logs.
package fakes

import (
	"context"
	"errors"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
)

type Resolvers struct {
	Credentials map[string]workaws.CredentialHandle
	Secrets     map[string]string
	Err         error
}

func (r *Resolvers) ResolveCredential(_ context.Context, ref string, revision int64) (workaws.CredentialHandle, error) {
	if r == nil || r.Err != nil {
		if r != nil && r.Err != nil {
			return workaws.CredentialHandle{}, r.Err
		}
		return workaws.CredentialHandle{}, errors.New("credential unavailable")
	}
	v, ok := r.Credentials[ref]
	if !ok || v.Revision != revision {
		return workaws.CredentialHandle{}, errors.New("credential unavailable")
	}
	return v, nil
}
func (r *Resolvers) ResolveSecretReference(_ context.Context, ref string) (string, error) {
	if r == nil || r.Err != nil {
		if r != nil && r.Err != nil {
			return "", r.Err
		}
		return "", errors.New("secret unavailable")
	}
	v, ok := r.Secrets[ref]
	if !ok {
		return "", errors.New("secret unavailable")
	}
	return v, nil
}

var _ workaws.CredentialResolver = (*Resolvers)(nil)
var _ workaws.SecretResolver = (*Resolvers)(nil)
