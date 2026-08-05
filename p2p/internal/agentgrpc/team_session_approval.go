package agentgrpc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
)

const teamSessionApprovalSignerDomain = "dirextalk/team-session-approval-signer/v1"

type teamSessionApprovalSigner struct {
	keyID      string
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
}

func (signer *teamSessionApprovalSigner) clear() {
	if signer == nil {
		return
	}
	clear(signer.publicKey)
	clear(signer.privateKey)
	signer.keyID = ""
}

func (runner *Runner) newTeamSessionApprovalSigner() (
	*teamSessionApprovalSigner,
	error,
) {
	if runner == nil || runner.ownerID == "" || runner.serviceKeyFile == "" {
		return nil, errors.New("Team session approval is unavailable")
	}
	raw, err := loadMountedServiceKey(runner.serviceKeyFile)
	if err != nil {
		return nil, errors.New("Team session approval is unavailable")
	}
	defer clear(raw)
	value := bytes.TrimSpace(raw)
	if err := validateServiceKey(value); err != nil {
		return nil, errors.New("Team session approval is unavailable")
	}
	separator := bytes.IndexByte(value, '.')
	encodedSecret := value[separator+1:]
	secret := make([]byte, base64.RawURLEncoding.DecodedLen(len(encodedSecret)))
	decoded, err := base64.RawURLEncoding.Decode(secret, encodedSecret)
	if err != nil || decoded != sha256.Size {
		clear(secret)
		return nil, errors.New("Team session approval is unavailable")
	}
	secret = secret[:decoded]
	defer clear(secret)

	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(teamSessionApprovalSignerDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(runner.ownerID))
	seed := mac.Sum(nil)
	defer clear(seed)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := append(
		ed25519.PublicKey(nil),
		privateKey.Public().(ed25519.PublicKey)...,
	)
	digest := sha256.Sum256(publicKey)
	return &teamSessionApprovalSigner{
		keyID:      "cloud-device-" + hex.EncodeToString(digest[:])[:24],
		publicKey:  publicKey,
		privateKey: privateKey,
	}, nil
}

func (runner *Runner) ensureTeamSessionApprovalSigner(
	ctx context.Context,
	signer *teamSessionApprovalSigner,
) error {
	if signer == nil || len(signer.publicKey) != ed25519.PublicKeySize {
		return errors.New("Team session approval is unavailable")
	}
	idempotencyKey := uuid.NewSHA1(
		uuid.NameSpaceURL,
		[]byte(
			teamSessionApprovalSignerDomain+"\x00register\x00"+
				runner.ownerID+"\x00"+signer.keyID,
		),
	).String()
	_, err := runner.bootstrapFirstTeamApprovalDevice(
		ctx,
		map[string]any{
			"idempotency_key": idempotencyKey,
			"key_id":          signer.keyID,
			"public_key_base64url": base64.RawURLEncoding.EncodeToString(
				signer.publicKey,
			),
		},
	)
	return err
}
